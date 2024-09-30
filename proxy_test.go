package main

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/stretchr/testify/assert"
)

func TestProxyServer(t *testing.T) {
    // Set up a mock downstream WebSocket server.
    downstreamAddr := "localhost:9001"
    downstreamMessages := make(chan string, 10)
    go startMockDownstreamServer(downstreamAddr, downstreamMessages)

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9000"
    proxyServer := &ProxyServer{
        DownstreamServerAddr: downstreamAddr,
    }
    defer proxyServer.Stop()
    err := proxyServer.Start(proxyAddr)
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // Connect a WebSocket client to the proxy server.
    clientConn, _, _, err := ws.DefaultDialer.Dial(context.Background(), "ws://"+proxyAddr+"/ws")
    if err != nil {
        t.Fatalf("Failed to connect to proxy server: %v", err)
    }
    defer clientConn.Close()

    // Send a message from the client to the downstream server.
    clientMessage := "Hello from client"
    err = wsutil.WriteClientText(clientConn, []byte(clientMessage))
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
    err = wsutil.WriteServerText(downstreamConn, []byte(downstreamResponse))
    if err != nil {
        t.Fatalf("Failed to send message from downstream server: %v", err)
    }

    // Read the message on the client side.
    clientReader := wsutil.NewReader(clientConn, ws.StateClientSide)
    header, err := clientReader.NextFrame()
    if err != nil {
        t.Fatalf("Failed to read message on client side: %v", err)
    }
    payload := make([]byte, header.Length)
    _, err = io.ReadFull(clientReader, payload)
    if err != nil {
        t.Fatalf("Failed to read message payload on client side: %v", err)
    }

    assert.Equal(t, downstreamResponse, string(payload), "Client did not receive the correct message from downstream")
}

var downstreamConns = make(chan net.Conn, 10)

func startMockDownstreamServer(addr string, messages chan<- string) {
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        log.Fatalf("Failed to start mock downstream server: %v", err)
    }
    defer ln.Close()
    for {
        conn, err := ln.Accept()
        if err != nil {
            log.Printf("Mock downstream server accept error: %v", err)
            continue
        }
        go handleDownstreamConnection(conn, messages)
    }
}

func handleDownstreamConnection(conn net.Conn, messages chan<- string) {
    defer conn.Close()
    // Perform the server-side WebSocket handshake.
    _, err := ws.Upgrade(conn)
    if err != nil {
        log.Printf("Mock downstream server handshake error: %v", err)
        return
    }
    // Notify the test that we have a connection to the downstream server.
    downstreamConns <- conn

    for {
        msg, op, err := wsutil.ReadClientData(conn)
        if err != nil {
            if err == io.EOF {
                return
            }
            log.Printf("Mock downstream server read error: %v", err)
            return
        }
        if op == ws.OpText {
            messages <- string(msg)
        }
    }
}

var (
    downstreamConnsBinary = make(chan net.Conn, 10)
)

func TestProxyServerBinaryMessage(t *testing.T) {
    // Set up a mock downstream WebSocket server.
    downstreamAddr := "localhost:9002"
    downstreamBinaryMessages := make(chan []byte, 10)
    go startMockDownstreamServerBinary(downstreamAddr, downstreamBinaryMessages)

    // Give the downstream server time to start.
    time.Sleep(100 * time.Millisecond)

    // Set up the proxy server.
    proxyAddr := "localhost:9003"
    proxyServer := &ProxyServer{
        DownstreamServerAddr: downstreamAddr,
    }
    defer proxyServer.Stop()
    err := proxyServer.Start(proxyAddr)
    if err != nil {
        t.Fatalf("Failed to start proxy server: %v", err)
    }

    // Connect a WebSocket client to the proxy server.
    clientConn, _, _, err := ws.DefaultDialer.Dial(context.Background(), "ws://"+proxyAddr+"/ws")
    if err != nil {
        t.Fatalf("Failed to connect to proxy server: %v", err)
    }
    defer clientConn.Close()

    // Send a binary message from the client to the downstream server.
    clientMessage := []byte{0x00, 0x01, 0x02, 0x03}
    err = wsutil.WriteClientBinary(clientConn, clientMessage)
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
    err = wsutil.WriteServerBinary(downstreamConn, downstreamResponse)
    if err != nil {
        t.Fatalf("Failed to send binary message from downstream server: %v", err)
    }

    // Read the binary message on the client side.
    clientReader := wsutil.NewReader(clientConn, ws.StateClientSide)
    header, err := clientReader.NextFrame()
    if err != nil {
        t.Fatalf("Failed to read binary message on client side: %v", err)
    }
    payload := make([]byte, header.Length)
    _, err = io.ReadFull(clientReader, payload)
    if err != nil {
        t.Fatalf("Failed to read binary message payload on client side: %v", err)
    }

    assert.Equal(t, downstreamResponse, payload, "Client did not receive the correct binary message from downstream")
}

func startMockDownstreamServerBinary(addr string, binaryMessages chan<- []byte) {
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        log.Fatalf("Failed to start mock downstream server: %v", err)
    }
    defer ln.Close()
    for {
        conn, err := ln.Accept()
        if err != nil {
            log.Printf("Mock downstream server accept error: %v", err)
            continue
        }
        go handleDownstreamConnectionBinary(conn, binaryMessages)
    }
}

func handleDownstreamConnectionBinary(conn net.Conn, binaryMessages chan<- []byte) {
    defer conn.Close()
    // Perform the server-side WebSocket handshake.
    _, err := ws.Upgrade(conn)
    if err != nil {
        log.Printf("Mock downstream server handshake error: %v", err)
        return
    }
    // Notify the test that we have a connection to the downstream server.
    downstreamConnsBinary <- conn

    for {
        msg, op, err := wsutil.ReadClientData(conn)
        if err != nil {
            if err == io.EOF {
                return
            }
            log.Printf("Mock downstream server read error: %v", err)
            return
        }
        if op == ws.OpBinary {
            binaryMessages <- msg
        }
    }
}