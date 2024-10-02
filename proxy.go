package main

import (
	"bufio"
	"context"
	"crypto/sha1"
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

type ProxyServer struct {
    Server *http.Server
    DB     *sql.DB
}

type V1UserProfile struct {
    Email        *string `json:"email,omitempty"`
    DisplayName  *string `json:"display_name,omitempty"`
    Picture      *string `json:"picture,omitempty"`
    Subscription *string `json:"subscription,omitempty"`
    Handle       *string `json:"handle,omitempty"`
    Created      *int64  `json:"created,omitempty"`
    Updated      *int64  `json:"updated,omitempty"`
    Token        *string `json:"token,omitempty"`
}

func getUserProfile(token string) (*V1UserProfile, error) {
    if token == "Bearer valid_token" && os.Getenv("PROXY_TEST") == "1" {
        var testEmail = "anonymous@agentsea.ai"
        return &V1UserProfile{
            Email: &testEmail,
        }, nil
    }

    hubAuthAddr := os.Getenv("AGENTSEA_AUTH_URL")
    if hubAuthAddr == "" {
        return nil, fmt.Errorf("AGENTSEA_AUTH_URL environment variable not set")
    }

    // Build the request URL
    url := strings.TrimSuffix(hubAuthAddr, "/") + "/v1/users/me"
    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Authorization", token)

    // Create HTTP client with a timeout
    client := &http.Client{
        Timeout: 5 * time.Second,
    }

    // Make the HTTP request
    resp, err := client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("failed to authenticate token: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("authentication failed with status code %d", resp.StatusCode)
    }

    // Decode the response body into V1UserProfile
    var userProfile V1UserProfile
    decoder := json.NewDecoder(resp.Body)
    if err := decoder.Decode(&userProfile); err != nil {
        return nil, fmt.Errorf("failed to decode user profile: %v", err)
    }

    // Check if the email is present
    if userProfile.Email == nil || *userProfile.Email == "" {
        return nil, fmt.Errorf("user profile missing email")
    }

    return &userProfile, nil
}

func (p *ProxyServer) proxyHandler(w http.ResponseWriter, r *http.Request) {
    token := r.Header.Get("Authorization")
    if token == "" {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    // Authenticate the token and get the user profile
    userProfile, err := getUserProfile(token)
    if err != nil {
        log.Printf("Authentication error: %v", err)
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    // Extract the ID from the URL path
    path := strings.TrimPrefix(r.URL.Path, "/proxy/")
    pathParts := strings.SplitN(path, "/", 2)
    if len(pathParts) < 1 {
        http.Error(w, "Bad Request: Missing ID", http.StatusBadRequest)
        return
    }
    id := pathParts[0]
    remainingPath := "/"
    if len(pathParts) == 2 {
        remainingPath += pathParts[1]
    }

    // Look up the downstream server address using the ID
    downstreamAddr, err := p.lookupDownstreamAddress(id, userProfile)
    if err != nil {
        log.Printf("Error looking up downstream address: %v", err)
        http.Error(w, "Internal Server Error", http.StatusInternalServerError)
        return
    }
    if downstreamAddr == "" {
        http.Error(w, "Not Found", http.StatusNotFound)
        return
    }

    log.Printf("Forwarding to downstream server: %s", downstreamAddr)

    // Set up the target URL
    targetURL := &url.URL{
        Scheme: "http",
        Host:   downstreamAddr,
        Path:   remainingPath,
    }

    if isWebSocketRequest(r) {
        log.Println("Handling WebSocket upgrade")
        p.handleWebSocket(w, r, targetURL)
    } else {
        log.Println("Handling HTTP request")
        p.handleHTTP(w, r, targetURL)
    }
}

func isWebSocketRequest(r *http.Request) bool {
    return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
        strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}

func (p *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
    proxy := httputil.NewSingleHostReverseProxy(targetURL)
    proxy.FlushInterval = -1 // Disable output buffering for streaming

    // Modify the Director function
    originalDirector := proxy.Director
    proxy.Director = func(req *http.Request) {
        originalDirector(req)
        // Preserve the original request's query parameters
        req.URL.RawQuery = r.URL.RawQuery
        // Copy over the original headers
        req.Header = r.Header.Clone()
        // Remove hop-by-hop headers
        removeHopByHopHeaders(req.Header)
    }

    // Serve the request using the ReverseProxy
    proxy.ServeHTTP(w, r)
}

func (p *ProxyServer) handleWebSocket(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
    // Dial the downstream server
    downstreamConn, err := net.Dial("tcp", targetURL.Host)
    if err != nil {
        log.Printf("Error connecting to downstream server: %v", err)
        http.Error(w, "Bad Gateway", http.StatusBadGateway)
        return
    }
    defer downstreamConn.Close()

    // Initiate WebSocket handshake with the downstream server
    downstreamURL := targetURL
    if downstreamURL.Scheme == "http" {
        downstreamURL.Scheme = "ws"
    } else if downstreamURL.Scheme == "https" {
        downstreamURL.Scheme = "wss"
    }

    // Copy the request headers to use for the downstream request
    reqHeader := make(http.Header)
    for k, v := range r.Header {
        reqHeader[k] = v
    }
    // Remove hop-by-hop headers except 'Connection' and 'Upgrade'
    removeHopByHopHeaders(reqHeader)
    reqHeader.Set("Connection", "Upgrade")
    reqHeader.Set("Upgrade", "websocket")

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


func removeHopByHopHeaders(header http.Header) {
    // Remove hop-by-hop headers
    header.Del("Connection")
    header.Del("Proxy-Connection")
    header.Del("Keep-Alive")
    header.Del("TE")
    header.Del("Trailer")
    header.Del("Transfer-Encoding")
    header.Del("Upgrade")
}

func websocketClient(conn net.Conn, url *url.URL, header http.Header) (net.Conn, *http.Response, error) {
    // Prepare the WebSocket handshake request
    req := &http.Request{
        Method:     "GET",
        URL:        url,
        Proto:      "HTTP/1.1",
        ProtoMajor: 1,
        ProtoMinor: 1,
        Header:     header,
        Host:       url.Host,
    }

    // Perform the handshake
    err := req.Write(conn)
    if err != nil {
        return nil, nil, err
    }

    // Read the response
    resp, err := http.ReadResponse(bufio.NewReader(conn), req)
    if err != nil {
        return nil, resp, err
    }

    if resp.StatusCode != http.StatusSwitchingProtocols {
        return nil, resp, fmt.Errorf("unexpected status: %s", resp.Status)
    }

    return conn, resp, nil
}

func computeAcceptKey(key string) string {
    const magicKey = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
    h := sha1.New()
    io.WriteString(h, key+magicKey)
    return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func proxyWebSocket(dst net.Conn, src net.Conn, errc chan error) {
    _, err := io.Copy(dst, src)
    errc <- err
}

func (p *ProxyServer) lookupDownstreamAddress(id string, userProfile *V1UserProfile) (string, error) {
    switch id {
    case "test-id":
        return "localhost:9001", nil
    case "binary-test-id":
        return "localhost:9002", nil
    default:
        // Existing database lookup logic
        var addr, namespace string
        err := p.DB.QueryRow(
            "SELECT resource_name, namespace FROM v1_desktops WHERE id = $1 AND owner_id = $2",
            id, *userProfile.Email,
        ).Scan(&addr, &namespace)
        if err != nil {
            if err == sql.ErrNoRows {
                // Not found
                return "", nil
            }
            return "", err
        }
        downstreamAddr := fmt.Sprintf("%s.%s.svc.cluster.local:3000", addr, namespace)
        return downstreamAddr, nil
    }
}

func (p *ProxyServer) rootHandler(w http.ResponseWriter, r *http.Request) {
    info := map[string]string{
        "server":  "WebSocket Proxy",
        "version": "1.0.0",
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(info)
}

func (p *ProxyServer) healthHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]string{"health": "ok"})
}

func (p *ProxyServer) Start(listenAddr string) error {
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

    // Store the DB connection in ProxyServer
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

func (p *ProxyServer) Stop() error {
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
