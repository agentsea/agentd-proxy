package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

type ProxyServer struct {
    Server               *http.Server
    DownstreamServerAddr string
}

func validateToken(token string) bool {
    token = strings.TrimPrefix(token, "Bearer ")
    // TODO: Implement your validation logic.
    return token == "valid_token"
}

func (p *ProxyServer) wsHandler(w http.ResponseWriter, r *http.Request) {
    token := r.Header.Get("Authorization")
    if !validateToken(token) {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }

    clientConn, _, _, err := ws.UpgradeHTTP(r, w)
    if err != nil {
        log.Println("Failed to upgrade client connection:", err)
        return
    }
    defer clientConn.Close()

    // Establish a TCP connection to the downstream WebSocket server.
    tcpConn, err := net.Dial("tcp", p.DownstreamServerAddr)
    if err != nil {
        log.Println("Failed to connect to downstream server:", err)
        return
    }
    defer func() {
        if tcpConn != nil {
            tcpConn.Close()
        }
    }()

    // Perform the WebSocket handshake with the downstream server using our existing net.Conn.
    dialer := ws.Dialer{
        NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
            return tcpConn, nil
        },
    }

    downstreamURL := "ws://" + p.DownstreamServerAddr + r.URL.Path

    downstreamConn, _, _, err := dialer.Dial(context.Background(), downstreamURL)
    if err != nil {
        log.Println("Failed to perform WebSocket handshake with downstream server:", err)
        return
    }
    tcpConn = nil
    defer downstreamConn.Close()

    // Start proxying data between clientConn and downstreamConn.
    proxyConn(clientConn, downstreamConn)
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
    mux := http.NewServeMux()
    mux.HandleFunc("/ws", p.wsHandler)

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
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    return p.Server.Shutdown(ctx)
}
