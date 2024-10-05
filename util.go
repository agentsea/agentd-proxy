package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func isWebSocketRequest(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
		strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}

func removeHopByHopHeaders(header http.Header) {
	// Remove hop-by-hop headers
	header.Del("Connection")
	header.Del("Proxy-Connection")
	header.Del("Keep-Alive")
	header.Del("TE")
	header.Del("Trailer")
	header.Del("Transfer-Encoding")
	header.Del("Upgrade")
}

func websocketClient(conn net.Conn, url *url.URL, header http.Header) (net.Conn, *http.Response, error) {
	// Prepare the WebSocket handshake request
	req := &http.Request{
		Method:     "GET",
		URL:        url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Host:       url.Host,
	}

	// If credentials are present, set the Authorization header
	if url.User != nil {
		username := url.User.Username()
		password, _ := url.User.Password()
		credentials := fmt.Sprintf("%s:%s", username, password)
		encodedCredentials := base64.StdEncoding.EncodeToString([]byte(credentials))
		req.Header.Set("Authorization", "Basic "+encodedCredentials)
	}

	// Perform the handshake
	err := req.Write(conn)
	if err != nil {
		return nil, nil, err
	}

	// Read the response
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, resp, err
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, resp, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	return conn, resp, nil
}

func computeAcceptKey(key string) string {
	const magicKey = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New()
	io.WriteString(h, key+magicKey)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func proxyWebSocket(dst net.Conn, src net.Conn, errc chan error) {
	_, err := io.Copy(dst, src)
	errc <- err
}