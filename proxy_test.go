package main

import (
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

func TestProxyServer(t *testing.T) {
    // Set up a mock downstream HTTP/WebSocket server.
    downstreamAddr := "localhost:9001"
    downstreamMessages := make(chan string, 10)
    go startMockDownstreamServer(downstreamAddr, downstreamMessages)

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9000"
    proxyServer := &ProxyServer{}
    defer proxyServer.Stop()
    err := proxyServer.Start(proxyAddr)
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // Build the WebSocket client URL.
    wsURL := "ws://" + proxyAddr + "/proxy/test-id/"

    // Set up the WebSocket client.
    header := http.Header{}
    header.Set("Authorization", "Bearer valid_token")

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

var downstreamConns = make(chan *websocket.Conn, 10)

func startMockDownstreamServer(addr string, messages chan<- string) {
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        upgrader := websocket.Upgrader{}
        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Printf("Mock downstream server upgrade error: %v", err)
            return
        }
        // Notify the test that we have a connection to the downstream server.
        downstreamConns <- conn

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

func TestProxyServerBinaryMessage(t *testing.T) {
    // Set up a mock downstream HTTP/WebSocket server.
    downstreamAddr := "localhost:9002"
    downstreamBinaryMessages := make(chan []byte, 10)
    go startMockDownstreamServerBinary(downstreamAddr, downstreamBinaryMessages)

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9003"
    proxyServer := &ProxyServer{}
    defer proxyServer.Stop()
    err := proxyServer.Start(proxyAddr)
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // Build the WebSocket client URL.
    wsURL := "ws://" + proxyAddr + "/proxy/binary-test-id/"

    // Set up the WebSocket client.
    header := http.Header{}
    header.Set("Authorization", "Bearer valid_token")

    dialer := websocket.Dialer{}

    clientConn, _, err := dialer.Dial(wsURL, header)
    if err != nil {
        t.Fatalf("Failed to connect to proxy server: %v", err)
    }
    defer clientConn.Close()

    // Send a binary message from the client to the downstream server.
    clientMessage := []byte{0x00, 0x01, 0x02, 0x03}
    err = clientConn.WriteMessage(websocket.BinaryMessage, clientMessage)
    if err != nil {
        t.Fatalf("Failed to send binary message from client: %v", err)
    }

    // Verify that the downstream server received the binary message.
    select {
    case msg := <-downstreamBinaryMessages:
        assert.Equal(t, clientMessage, msg, "Downstream server did not receive the correct binary message")
    case <-time.After(1 * time.Second):
        t.Fatalf("Timed out waiting for downstream server to receive binary message")
    }

    // Send a binary message from the downstream server to the client.
    downstreamConn := <-downstreamConnsBinary
    downstreamResponse := []byte{0x04, 0x05, 0x06, 0x07}
    err = downstreamConn.WriteMessage(websocket.BinaryMessage, downstreamResponse)
    if err != nil {
        t.Fatalf("Failed to send binary message from downstream server: %v", err)
    }

    // Read the binary message on the client side.
    _, payload, err := clientConn.ReadMessage()
    if err != nil {
        t.Fatalf("Failed to read binary message on client side: %v", err)
    }

    assert.Equal(t, downstreamResponse, payload, "Client did not receive the correct binary message from downstream")
}

var downstreamConnsBinary = make(chan *websocket.Conn, 10)

func startMockDownstreamServerBinary(addr string, binaryMessages chan<- []byte) {
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        upgrader := websocket.Upgrader{}
        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Printf("Mock downstream server upgrade error: %v", err)
            return
        }
        // Notify the test that we have a connection to the downstream server.
        downstreamConnsBinary <- conn

        for {
            msgType, msg, err := conn.ReadMessage()
            if err != nil {
                if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) || err == io.EOF {
                    return
                }
                log.Printf("Mock downstream server read error: %v", err)
                return
            }
            if msgType == websocket.BinaryMessage {
                binaryMessages <- msg
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
