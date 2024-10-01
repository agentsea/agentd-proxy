package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
    // Replace with your actual downstream server address and port.
    downstreamServerAddr := "downstream_server:port" // e.g., "localhost:8081"

    // Create an instance of ProxyServer.
    proxyServer := &ProxyServer{
        DownstreamServerAddr: downstreamServerAddr,
    }

    // Start the proxy server on the desired address and port.
    err := proxyServer.Start(":8080") // Listening on port 8080.
    if err != nil {
        log.Fatalf("Failed to start proxy server: %v", err)
    }

    // Wait for interrupt signal to gracefully shut down the server.
    waitForShutdown(proxyServer)
}

// waitForShutdown waits for an interrupt signal and gracefully shuts down the server.
func waitForShutdown(proxyServer *ProxyServer) {
    // Create a channel to receive OS signals.
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

    // Wait for a signal.
    <-quit
    log.Println("Shutting down server...")

    // Create a context with a timeout for the shutdown process.
    _, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // Attempt to gracefully shut down the server.
    if err := proxyServer.Stop(); err != nil {
        log.Fatalf("Server forced to shutdown: %v", err)
    }

    log.Println("Server exited")
}
