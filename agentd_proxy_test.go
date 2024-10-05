package main

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

func TestAgentdProxyServer(t *testing.T) {
    // Set the environment variable to indicate test mode
    os.Setenv("PROXY_TEST", "1")
    defer os.Unsetenv("PROXY_TEST")

    // Set up a mock downstream HTTP/WebSocket server
    downstreamAddr := "localhost:9001"
    downstreamMessages := make(chan string, 10)
    downstreamConns := make(chan *websocket.Conn, 10)
    go startMockDownstreamServer(downstreamAddr, downstreamMessages, downstreamConns, false) // Pass false since no auth is required

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9002"
    proxyServer := &AgentdProxyServer{}
    defer proxyServer.Stop()
    err := proxyServer.Start(":" + proxyAddr[10:])
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // **WebSocket client URL to include "/proxy/<id>"**
    wsURL := "ws://" + proxyAddr + "/proxy/test-id/"

    // Set up the WebSocket client with the Authorization header.
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
