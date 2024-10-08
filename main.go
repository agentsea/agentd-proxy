package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Create instances of both servers.
	proxyServer := &AgentdProxyServer{}
	proxyVNCServer := &VNCProxyServer{}

	// Start both servers on different ports or addresses.
	err := proxyServer.Start(":9090") // Listening on port 9090.
	if err != nil {
		log.Fatalf("Failed to start Agentd ProxyServer: %v", err)
	}

	err = proxyVNCServer.Start(":8080") // Listening on port 8080.
	if err != nil {
		log.Fatalf("Failed to start VNC ProxyServer: %v", err)
	}

	// Wait for interrupt signal to gracefully shut down the servers.
	waitForShutdown(proxyServer, proxyVNCServer)
}

// waitForShutdown waits for a shutdown signal and then shuts down the servers gracefully.
func waitForShutdown(proxyServer *AgentdProxyServer, proxyVNCServer *VNCProxyServer) {
	// Create a channel to receive OS signals.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Wait for a signal.
	<-stop

	log.Println("Shutdown signal received, shutting down servers...")

	// Channel to wait for both servers to shut down.
	done := make(chan struct{}, 2)

	// Shutdown ProxyServer.
	go func() {
		if err := proxyServer.Stop(); err != nil {
			log.Printf("Agentd ProxyServer forced to shutdown: %v", err)
		} else {
			log.Println("Agentd ProxyServer exited gracefully")
		}
		done <- struct{}{}
	}()

	// Shutdown ProxyVNCServer.
	go func() {
		if err := proxyVNCServer.Stop(); err != nil {
			log.Printf("VNC ProxyServer forced to shutdown: %v", err)
		} else {
			log.Println("VNC ProxyServer exited gracefully")
		}
		done <- struct{}{}
	}()

	// Wait for both servers to finish shutdown.
	<-done
	<-done

	log.Println("All servers exited")
}
