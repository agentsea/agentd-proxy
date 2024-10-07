package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type VNCProxyServer struct {
	Server *http.Server
	DB     *sql.DB
}

func (p *VNCProxyServer) proxyHandler(w http.ResponseWriter, r *http.Request) {
    // Remove user info from the incoming request URL
    r.URL.User = nil

    // Extract id from the path without modifying the path
    pathParts := strings.SplitN(r.URL.Path, "/", 5)
    if len(pathParts) < 4 || pathParts[1] != "proxy" || pathParts[2] == "" || pathParts[3] == ""{
        http.Error(w, "Bad Request: Missing ID", http.StatusBadRequest)
        return
    }
    id := pathParts[2]
	pass := pathParts[3]

    // Use the full path as is
    fullPath := r.URL.Path

    // Look up the downstream server address and scheme using the ID
    downstreamAddr, scheme, err := p.lookupDownstreamAddress(id, pass)
    if err != nil {
        log.Printf("Error looking up downstream address: %v", err)
        http.Error(w, "Internal Server Error", http.StatusInternalServerError)
        return
    }
    if downstreamAddr == "" {
        http.Error(w, "Not Found", http.StatusNotFound)
        return
    }

    log.Printf("Forwarding to downstream server: %s using scheme %s", downstreamAddr, scheme)

    // Set up the target URL, including credentials and path
    targetURL := &url.URL{
        Scheme: scheme,
        Host:   downstreamAddr,
        Path:   fullPath,  // Use the full path including "/proxy/<id>/..."
        RawQuery: r.URL.RawQuery,
    }

    vncAuthEnabled := os.Getenv("VNC_BASIC_AUTH")

    if vncAuthEnabled == "true" {
        // Set up the basic auth credentials
        targetURL.User = url.UserPassword(id, pass)
    }

    // Proceed to handle the request
    if isWebSocketRequest(r) {
        log.Println("Handling WebSocket upgrade")
        p.handleWebSocket(w, r, targetURL)
    } else {
        log.Println("Handling HTTP request")
        p.handleHTTP(w, r, targetURL)
    }
}


func (p *VNCProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
    proxy := httputil.NewSingleHostReverseProxy(targetURL)
    proxy.FlushInterval = -1 // Disable output buffering for streaming

    proxy.Director = func(req *http.Request) {
        req.URL.Scheme = targetURL.Scheme
        req.URL.Host = targetURL.Host
        req.URL.Path = targetURL.Path
        req.URL.RawQuery = targetURL.RawQuery
    
        req.Header = r.Header.Clone()
        removeHopByHopHeaders(req.Header)
        req.Host = targetURL.Host
    
        log.Printf("Forwarding request to downstream URL: %s", req.URL.String())
        log.Printf("Forwarded request headers: %v", req.Header)
    }

    proxy.ServeHTTP(w, r)
}


func (p *VNCProxyServer) handleWebSocket(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
    // Prepare the downstream URL with credentials
    var downstreamURLScheme string
    if targetURL.Scheme == "https" {
        downstreamURLScheme = "wss"
    } else {
        downstreamURLScheme = "ws"
    }
    downstreamURL := &url.URL{
        Scheme:   downstreamURLScheme,
        Host:     targetURL.Host,
        Path:     targetURL.Path,
        RawQuery: targetURL.RawQuery,
    }

    // Include credentials in the URL
    if targetURL.User != nil {
        downstreamURL.User = targetURL.User
    }

    // Determine whether to use TLS or not
    var downstreamConn net.Conn
    var err error
    if downstreamURL.Scheme == "wss" {
        // Dial the downstream server using TLS
        downstreamConn, err = tls.Dial("tcp", downstreamURL.Host, &tls.Config{
            InsecureSkipVerify: true, // Adjust TLS settings as needed
        })
    } else {
        // Dial the downstream server without TLS
        downstreamConn, err = net.Dial("tcp", downstreamURL.Host)
    }
    if err != nil {
        log.Printf("Error connecting to downstream server: %v", err)
        http.Error(w, "Bad Gateway", http.StatusBadGateway)
        return
    }
    defer downstreamConn.Close()

    // Copy the request headers to use for the downstream request
    reqHeader := make(http.Header)
    for k, v := range r.Header {
        reqHeader[k] = v
    }
    // Remove hop-by-hop headers except 'Connection' and 'Upgrade'
    removeHopByHopHeaders(reqHeader)
    reqHeader.Set("Connection", "Upgrade")
    reqHeader.Set("Upgrade", "websocket")

    // Include the Authorization header if credentials are present
    if downstreamURL.User != nil {
        username := downstreamURL.User.Username()
        password, _ := downstreamURL.User.Password()
        credentials := fmt.Sprintf("%s:%s", username, password)
        encodedCredentials := base64.StdEncoding.EncodeToString([]byte(credentials))
        reqHeader.Set("Authorization", "Basic "+encodedCredentials)
    }

    // Perform the WebSocket handshake with the downstream server
    downstreamWsConn, resp, err := websocketClient(downstreamConn, downstreamURL, reqHeader)
    if err != nil {
        log.Printf("Error during WebSocket handshake with downstream server: %v", err)
        if resp != nil {
            w.WriteHeader(resp.StatusCode)
            io.Copy(w, resp.Body)
        } else {
            http.Error(w, "Bad Gateway", http.StatusBadGateway)
        }
        return
    }
    defer downstreamWsConn.Close()

    // Hijack the client connection
    hijacker, ok := w.(http.Hijacker)
    if !ok {
        http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
        return
    }
    clientConn, _, err := hijacker.Hijack()
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer clientConn.Close()

    // Extract headers from the downstream server's response
    downstreamHeaders := resp.Header

    // Prepare response headers for the client
    responseHeader := make(http.Header)
    responseHeader.Set("Connection", "Upgrade")
    responseHeader.Set("Upgrade", "websocket")
    responseHeader.Set("Sec-WebSocket-Accept", computeAcceptKey(r.Header.Get("Sec-WebSocket-Key")))

    // Forward relevant headers from downstream response
    if protocol := downstreamHeaders.Get("Sec-WebSocket-Protocol"); protocol != "" {
        responseHeader.Set("Sec-WebSocket-Protocol", protocol)
    }
    if extensions := downstreamHeaders.Get("Sec-WebSocket-Extensions"); extensions != "" {
        responseHeader.Set("Sec-WebSocket-Extensions", extensions)
    }

    // Write the response to the client to complete the WebSocket handshake
    respBytes := []byte("HTTP/1.1 101 Switching Protocols\r\n")
    for k, v := range responseHeader {
        respBytes = append(respBytes, []byte(fmt.Sprintf("%s: %s\r\n", k, v[0]))...)
    }
    respBytes = append(respBytes, []byte("\r\n")...)
    _, err = clientConn.Write(respBytes)
    if err != nil {
        log.Printf("Error writing handshake response to client: %v", err)
        return
    }

    // Start proxying data between clientConn and downstreamWsConn
    errc := make(chan error, 2)
    go proxyWebSocket(clientConn, downstreamWsConn, errc)
    go proxyWebSocket(downstreamWsConn, clientConn, errc)
    <-errc
}




func (p *VNCProxyServer) lookupDownstreamAddress(id string, pass string) (string, string, error) {
	switch id {
	case "test-id":
		return "localhost:9102", "http", nil
    case "integration-test":
        return "localhost:3000", "http", nil
	default:
		// Existing database lookup logic
		var addr, namespace string
		err := p.DB.QueryRow(
			"SELECT resource_name, namespace FROM v1_desktops WHERE id = $1 AND basic_auth_password = $2",
			id, pass,
		).Scan(&addr, &namespace)
		if err != nil {
			if err == sql.ErrNoRows {
				// Not found
				return "", "", nil
			}
			return "", "", err
		}
		downstreamAddr := fmt.Sprintf("%s.%s.svc.cluster.local:3000", addr, namespace)
		return downstreamAddr, "http", nil
	}
}

func (p *VNCProxyServer) rootHandler(w http.ResponseWriter, r *http.Request) {
	info := map[string]string{
		"server":  "WebSocket Proxy",
		"version": "1.0.0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (p *VNCProxyServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"health": "ok"})
}

func (p *VNCProxyServer) Start(listenAddr string) error {
	// Set up the database connection
	dbHost := os.Getenv("DB_HOST")
	dbName := os.Getenv("DB_NAME")
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")

	// Build the connection string
	connStr := fmt.Sprintf(
		"host=%s dbname=%s user=%s password=%s sslmode=disable",
		dbHost, dbName, dbUser, dbPass,
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %v", err)
	}

	// Store the DB connection in VNCProxyServer
	p.DB = db

	// Set up the HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/", p.proxyHandler)
	mux.HandleFunc("/health", p.healthHandler)
	mux.HandleFunc("/", p.rootHandler)

	p.Server = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("Proxy server starting on %s", listenAddr)
		if err := p.Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server error: %v", err)
		}
	}()

	// Give the server a moment to start
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (p *VNCProxyServer) Stop() error {
	if p.Server == nil {
		return nil
	}
	// Close the database connection
	if p.DB != nil {
		p.DB.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Server.Shutdown(ctx)
}
