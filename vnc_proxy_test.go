package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

var downstreamConns = make(chan *websocket.Conn, 10)

func TestVncProxyServer(t *testing.T) {
    // Set the environment variable to indicate test mode
    os.Setenv("TEST_ENV", "1")
    defer os.Unsetenv("TEST_ENV")

    // Set up a mock downstream HTTP/WebSocket server that requires basic auth.
    downstreamAddr := "localhost:9002"
    downstreamMessages := make(chan string, 10)
    go startMockDownstreamServer(downstreamAddr, downstreamMessages, downstreamConns, true) // Pass true to require basic auth

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9000"
    proxyServer := &VNCProxyServer{}
    defer proxyServer.Stop()
    err := proxyServer.Start(proxyAddr)
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // **Updated WebSocket client URL to include "/proxy/"**
    wsURL := "ws://" + proxyAddr + "/proxy/test-id/"

    // Set up the WebSocket client with the custom header for credentials.
    header := http.Header{}
    header.Set("X-User-Credentials", "user:pass")

    dialer := websocket.Dialer{}

    clientConn, _, err := dialer.Dial(wsURL, header)
    if err != nil {
        t.Fatalf("Failed to connect to proxy server: %v", err)
    }
    defer clientConn.Close()

    // Send a message from the client to the downstream server.
    clientMessage := "Hello from client"
    err = clientConn.WriteMessage(websocket.TextMessage, []byte(clientMessage))
    if err != nil {
        t.Fatalf("Failed to send message from client: %v", err)
    }

    // Verify that the downstream server received the message.
    select {
    case msg := <-downstreamMessages:
        assert.Equal(t, clientMessage, msg, "Downstream server did not receive the correct message")
    case <-time.After(1 * time.Second):
        t.Fatalf("Timed out waiting for downstream server to receive message")
    }

    // Send a message from the downstream server to the client.
    downstreamConn := <-downstreamConns
    downstreamResponse := "Hello from downstream"
    err = downstreamConn.WriteMessage(websocket.TextMessage, []byte(downstreamResponse))
    if err != nil {
        t.Fatalf("Failed to send message from downstream server: %v", err)
    }

    // Read the message on the client side.
    _, payload, err := clientConn.ReadMessage()
    if err != nil {
        t.Fatalf("Failed to read message on client side: %v", err)
    }

    assert.Equal(t, downstreamResponse, string(payload), "Client did not receive the correct message from downstream")
}

func startMockDownstreamServer(addr string, messages chan<- string, conns chan<- *websocket.Conn, requireAuth bool) {
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        // Check for basic auth if required
        if requireAuth {
            username, password, ok := r.BasicAuth()
            if !ok || username != "user" || password != "pass" {
                w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
        }

        upgrader := websocket.Upgrader{}
        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Printf("Mock downstream server upgrade error: %v", err)
            return
        }
        // Notify the test that we have a connection to the downstream server.
        conns <- conn

        for {
            msgType, msg, err := conn.ReadMessage()
            if err != nil {
                if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) || err == io.EOF {
                    return
                }
                log.Printf("Mock downstream server read error: %v", err)
                return
            }
            if msgType == websocket.TextMessage {
                messages <- string(msg)
            }
        }
    })

    server := &http.Server{
        Addr:    addr,
        Handler: mux,
    }
    go func() {
        if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatalf("Mock downstream server error: %v", err)
        }
    }()
}