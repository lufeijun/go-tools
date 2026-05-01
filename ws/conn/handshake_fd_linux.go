//go:build linux

package conn

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ServerHandshakeFD performs the WebSocket server handshake on a raw file descriptor.
// It temporarily makes the fd blocking for the handshake, then restores non-blocking.
// Returns the fd on success.
func ServerHandshakeFD(fd int) (int, error) {
	// Temporarily set to blocking
	if err := unix.SetNonblock(fd, false); err != nil {
		return fd, fmt.Errorf("set blocking: %w", err)
	}
	defer unix.SetNonblock(fd, true) // Restore non-blocking when done

	// Create a file from the fd
	f := os.NewFile(uintptr(fd), "ws-conn")
	if f == nil {
		return fd, fmt.Errorf("os.NewFile failed")
	}
	// Note: We don't close f because that would close the fd

	// Get a net.Conn (this dups the fd internally)
	nc, err := net.FileConn(f)
	if err != nil {
		return fd, fmt.Errorf("net.FileConn: %w", err)
	}
	defer nc.Close() // Safe to close - it closes the dup'd fd

	// Set deadline for handshake
	nc.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	defer nc.SetDeadline(time.Time{})

	// Read HTTP request
	br := bufio.NewReader(nc)
	req, err := http.ReadRequest(br)
	if err != nil {
		return fd, fmt.Errorf("read request: %w", err)
	}

	// Validate request
	if req.Method != "GET" {
		return fd, errInvalidHandshake
	}
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return fd, errInvalidHandshake
	}
	if !hasConnectionUpgrade(req.Header.Get("Connection")) {
		return fd, errInvalidHandshake
	}
	secKey := req.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		return fd, errInvalidHandshake
	}
	if req.Header.Get("Sec-WebSocket-Version") != "13" {
		return fd, errInvalidHandshake
	}

	// Compute accept key and write response
	acceptKey := computeAcceptKey(secKey)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n\r\n"

	if _, err := nc.Write([]byte(response)); err != nil {
		return fd, fmt.Errorf("write response: %w", err)
	}

	return fd, nil
}

// ClientHandshakeFD performs the WebSocket client handshake on a raw file descriptor.
// It temporarily makes the fd blocking for the handshake, then restores non-blocking.
func ClientHandshakeFD(fd int, rawURL string, headers http.Header) error {
	// Temporarily set to blocking
	if err := unix.SetNonblock(fd, false); err != nil {
		return fmt.Errorf("set blocking: %w", err)
	}
	defer unix.SetNonblock(fd, true) // Restore non-blocking when done

	// Create a file from the fd
	f := os.NewFile(uintptr(fd), "ws-conn")
	if f == nil {
		return fmt.Errorf("os.NewFile failed")
	}
	// Note: We don't close f because that would close the fd

	// Get a net.Conn (this dups the fd internally)
	nc, err := net.FileConn(f)
	if err != nil {
		return fmt.Errorf("net.FileConn: %w", err)
	}
	defer nc.Close() // Safe to close - it closes the dup'd fd

	// Set deadline for handshake
	nc.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	defer nc.SetDeadline(time.Time{})

	// Parse URL
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	// Generate client key and compute accept key
	secKey, err := generateClientSecKey()
	if err != nil {
		return err
	}
	acceptKey := computeAcceptKey(secKey)

	// Build request
	var b strings.Builder
	b.WriteString("GET " + u.RequestURI() + " HTTP/1.1\r\n")
	b.WriteString("Host: " + u.Host + "\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + secKey + "\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, vs := range headers {
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	req := b.String()

	// Write request
	if _, err := nc.Write([]byte(req)); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	// Read response
	resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	// Validate response
	if resp.StatusCode != 101 {
		return fmt.Errorf("server returned status %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return errInvalidHandshake
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey {
		return fmt.Errorf("invalid Sec-WebSocket-Accept")
	}

	return nil
}
