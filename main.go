package main

import (
	"log"
)

func main() {
    // Create instances of both servers.
    proxyServer := &AgentdProxyServer{}
    proxyVNCServer := &VNCProxyServer{}

    // Start both servers on different ports or addresses.
    err := proxyServer.Start(":9090") // Listening on port 8080.
    if err != nil {
        log.Fatalf("Failed to start ProxyServer: %v", err)
    }

    err = proxyVNCServer.Start(":8080") // Listening on port 9090.
    if err != nil {
        log.Fatalf("Failed to start ProxyVNCServer: %v", err)
    }

    // Wait for interrupt signal to gracefully shut down the servers.
    waitForShutdown(proxyServer, proxyVNCServer)
}

// waitForShutdown waits for a shutdown signal and then shuts down the server gracefully.
func waitForShutdown(proxyServer *AgentdProxyServer, proxyVNCServer *VNCProxyServer) {
    // ... (signal handling code)

    // Channel to wait for both servers to shut down.
    done := make(chan struct{}, 2)

    // Shutdown ProxyServer.
    go func() {
        if err := proxyServer.Stop(); err != nil {
            log.Printf("ProxyServer forced to shutdown: %v", err)
        } else {
            log.Println("ProxyServer exited gracefully")
        }
        done <- struct{}{}
    }()

    // Shutdown ProxyVNCServer.
    go func() {
        if err := proxyVNCServer.Stop(); err != nil {
            log.Printf("ProxyVNCServer forced to shutdown: %v", err)
        } else {
            log.Println("ProxyVNCServer exited gracefully")
        }
        done <- struct{}{}
    }()

    // Wait for both servers to finish shutdown.
    <-done
    <-done

    log.Println("All servers exited")
}

