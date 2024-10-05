package main

import (
	"context"
	"crypto/tls"
	"database/sql"
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

// AgentdProxyServer struct holds the HTTP server and DB connection.
type AgentdProxyServer struct {
	Server *http.Server
	DB     *sql.DB
}

// V1UserProfile represents the user profile structure.
type V1UserProfile struct {
	ID           *string `json:"id,omitempty"`
	Email        *string `json:"email,omitempty"`
	DisplayName  *string `json:"display_name,omitempty"`
	Picture      *string `json:"picture,omitempty"`
	Subscription *string `json:"subscription,omitempty"`
	Handle       *string `json:"handle,omitempty"`
	Created      *int64  `json:"created,omitempty"`
	Updated      *int64  `json:"updated,omitempty"`
	Token        *string `json:"token,omitempty"`
}

// getUserProfile authenticates the token and retrieves the user profile.
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

// proxyHandler handles incoming proxy requests.
func (p *AgentdProxyServer) proxyHandler(w http.ResponseWriter, r *http.Request) {
	// Extract the Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Authenticate the token and get userID
	userProfile, err := getUserProfile(authHeader)
	if err != nil {
		log.Printf("Authentication failed: %v", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	userID := *userProfile.Email // Or *userProfile.ID if available

	// Remove user info from the incoming request URL
	r.URL.User = nil

	// Extract id from the path and get the target path
	pathParts := strings.SplitN(r.URL.Path, "/", 4)
	if len(pathParts) < 3 || pathParts[1] != "proxy" || pathParts[2] == "" {
		http.Error(w, "Bad Request: Missing ID", http.StatusBadRequest)
		return
	}
	id := pathParts[2]

	// The target path is the remaining path after /proxy/<id>
	var targetPath string
	if len(pathParts) >= 4 {
		targetPath = "/" + pathParts[3]
	} else {
		targetPath = "/"
	}

	// Look up the downstream server address and scheme using the ID and userID
	downstreamAddr, scheme, err := p.lookupDownstreamAddress(id, userID)
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

	// Set up the target URL, including path
	targetURL := &url.URL{
		Scheme:   scheme,
		Host:     downstreamAddr,
		Path:     targetPath,
		RawQuery: r.URL.RawQuery,
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


// handleHTTP handles regular HTTP requests.
func (p *AgentdProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
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

// handleWebSocket handles WebSocket upgrade requests.
func (p *AgentdProxyServer) handleWebSocket(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
	// Prepare the downstream URL
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

// lookupDownstreamAddress looks up the downstream address in the agent_instances table.
func (p *AgentdProxyServer) lookupDownstreamAddress(id string, userID string) (string, string, error) {
    if os.Getenv("PROXY_TEST") == "1" {
        switch id {
        case "test-id":
            return "localhost:9001", "http", nil
        default:
            return "", "", nil
        }
    }

    var resourceName, namespace string
    err := p.DB.QueryRow(
        "SELECT resource_name, namespace FROM v1_desktops WHERE id = $1 AND owner_id = $2",
        id, userID,
    ).Scan(&resourceName, &namespace)
    if err != nil {
        if err == sql.ErrNoRows {
            // Not found or not authorized
            return "", "", nil
        }
        return "", "", err
    }
	downstreamAddr := fmt.Sprintf("%s.%s.svc.cluster.local:8000", resourceName, namespace)
	return downstreamAddr, "http", nil
}

// rootHandler handles requests to the root path.
func (p *AgentdProxyServer) rootHandler(w http.ResponseWriter, r *http.Request) {
	info := map[string]string{
		"server":  "WebSocket Proxy",
		"version": "1.0.0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// healthHandler responds with the health status.
func (p *AgentdProxyServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"health": "ok"})
}

// Start initializes and starts the proxy server.
func (p *AgentdProxyServer) Start(listenAddr string) error {
    // Set up the database connection only if not in test mode
    if os.Getenv("PROXY_TEST") != "1" {
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

        // Store the DB connection in AgentdProxyServer
        p.DB = db
    }

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

// Stop gracefully shuts down the proxy server.
func (p *AgentdProxyServer) Stop() error {
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
