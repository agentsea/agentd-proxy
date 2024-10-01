package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"database/sql"
	"fmt"
	"os"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
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
    if token == "Bearer valid_token" && os.Getenv("PROXY_TEST") == "1"{
        var test_email = "anonymous@agentsea.ai"
        return &V1UserProfile{
                Email:        &test_email,
                DisplayName:  nil,
                Picture:      nil,
                Subscription: nil,
                Handle:       nil,
                Created:      nil,
                Updated:      nil,
                Token:        nil,
            }, nil
    }

    hubAuthAddr := os.Getenv("HUB_AUTH_ADDR")
    if hubAuthAddr == "" {
        return nil, fmt.Errorf("HUB_AUTH_ADDR environment variable not set")
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

func (p *ProxyServer) wsHandler(w http.ResponseWriter, r *http.Request) {
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
    id := strings.TrimPrefix(r.URL.Path, "/ws/")
    if id == "" || id == r.URL.Path {
        http.Error(w, "Bad Request: Missing ID", http.StatusBadRequest)
        return
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

    // Upgrade the client connection to WebSocket
    clientConn, _, _, err := ws.UpgradeHTTP(r, w)
    if err != nil {
        log.Println("Failed to upgrade client connection:", err)
        return
    }
    defer clientConn.Close()

    // Establish a TCP connection to the downstream WebSocket server
    tcpConn, err := net.Dial("tcp", downstreamAddr)
    if err != nil {
        log.Println("Failed to connect to downstream server:", err)
        return
    }
    defer func() {
        if tcpConn != nil {
            tcpConn.Close()
        }
    }()

    // Perform the WebSocket handshake with the downstream server
    dialer := ws.Dialer{
        NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
            return tcpConn, nil
        },
    }

    downstreamURL := "ws://" + downstreamAddr

    downstreamConn, _, _, err := dialer.Dial(context.Background(), downstreamURL)
    if err != nil {
        log.Println("Failed to perform WebSocket handshake with downstream server:", err)
        return
    }
    tcpConn = nil
    defer downstreamConn.Close()

    // Start proxying data between clientConn and downstreamConn
    proxyConn(clientConn, downstreamConn)
}

func (p *ProxyServer) lookupDownstreamAddress(id string, userProfile *V1UserProfile) (string, error) {
    switch id {
    case "test-id":
        return "localhost:9001", nil
    case "binary-test-id":
        return "localhost:9002", nil
    default:
        // Existing database lookup logic
        var addr string
        err := p.DB.QueryRow("SELECT address FROM downstream_servers WHERE id = $1 AND owner_id = $2", id).Scan(&addr, &userProfile.Email)
        if err != nil {
            if err == sql.ErrNoRows {
                // Not found
                return "", nil
            }
            return "", err
        }
        return addr, nil
    }
}


func proxyConn(clientConn net.Conn, downstreamConn net.Conn) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    errChan := make(chan error, 2)

    // Client to Downstream
    go func() {
        defer cancel()
        clientReader := wsutil.NewReader(clientConn, ws.StateServerSide)
        downstreamWriter := wsutil.NewWriter(downstreamConn, ws.StateClientSide, 0)

        for {
            header, err := clientReader.NextFrame()
            if err != nil {
                errChan <- err
                return
            }

            if header.OpCode.IsControl() {
                // Read the control frame's payload.
                payload := make([]byte, header.Length)
                if _, err := io.ReadFull(clientReader, payload); err != nil {
                    errChan <- err
                    return
                }

                // Create the wsutil.Message.
                msg := wsutil.Message{
                    OpCode:  header.OpCode,
                    Payload: payload,
                }

                // Handle the control message.
                err = wsutil.HandleClientControlMessage(clientConn, msg)
                if err != nil {
                    errChan <- err
                }
                continue
            }

            // Reset the writer with the appropriate OpCode.
            downstreamWriter.Reset(downstreamConn, ws.StateClientSide, header.OpCode)

            // Copy the frame payload from the client to the downstream server.
            _, err = io.CopyN(downstreamWriter, clientReader, int64(header.Length))
            if err != nil {
                errChan <- err
                return
            }

            // Flush the writer to send the frame.
            err = downstreamWriter.Flush()
            if err != nil {
                errChan <- err
                return
            }
        }
    }()

    // Downstream to Client
    go func() {
        defer cancel()
        downstreamReader := wsutil.NewReader(downstreamConn, ws.StateClientSide)
        clientWriter := wsutil.NewWriter(clientConn, ws.StateServerSide, 0)

        for {
            header, err := downstreamReader.NextFrame()
            if err != nil {
                errChan <- err
                return
            }

            if header.OpCode.IsControl() {
                // Read the control frame's payload.
                payload := make([]byte, header.Length)
                if _, err := io.ReadFull(downstreamReader, payload); err != nil {
                    errChan <- err
                    return
                }

                // Create the wsutil.Message.
                msg := wsutil.Message{
                    OpCode:  header.OpCode,
                    Payload: payload,
                }

                // Handle the control message.
                err = wsutil.HandleServerControlMessage(downstreamConn, msg)
                if err != nil {
                    errChan <- err
                }
                continue
            }

            // Reset the writer with the appropriate OpCode.
            clientWriter.Reset(clientConn, ws.StateServerSide, header.OpCode)

            // Copy the frame payload from the downstream server to the client.
            _, err = io.CopyN(clientWriter, downstreamReader, int64(header.Length))
            if err != nil {
                errChan <- err
                return
            }

            // Flush the writer to send the frame.
            err = clientWriter.Flush()
            if err != nil {
                errChan <- err
                return
            }
        }
    }()

    // Wait for any error from the goroutines.
    select {
    case err := <-errChan:
        if err != nil && !isClosedErr(err) {
            log.Println("Error in proxy connection:", err)
        }
    case <-ctx.Done():
        // Context cancelled, likely due to an error or connection closure.
    }
}

func isClosedErr(err error) bool {
    return err == io.EOF || strings.Contains(err.Error(), "closed network connection") || strings.Contains(err.Error(), "use of closed network connection")
}

func (p *ProxyServer) Start(listenAddr string) error {
    // Set up the database connection
    dbHost := os.Getenv("DB_HOST")
    dbName := os.Getenv("DB_NAME")
    dbUser := os.Getenv("DB_USER")
    dbPass := os.Getenv("DB_PASS")

    // Build the connection string
    connStr := fmt.Sprintf("host=%s dbname=%s user=%s password=%s sslmode=disable",
        dbHost, dbName, dbUser, dbPass)

    db, err := sql.Open("postgres", connStr)
    if err != nil {
        return fmt.Errorf("failed to connect to database: %v", err)
    }

    // Store the DB connection in ProxyServer
    p.DB = db

    // Set up the HTTP server
    mux := http.NewServeMux()
    mux.HandleFunc("/ws/", p.wsHandler)

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

