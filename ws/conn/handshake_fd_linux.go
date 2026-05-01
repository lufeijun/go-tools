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

	// Dup the fd so net.FileConn gets its own copy that it can safely close.
	// This prevents the GC finalizer on *os.File from closing our original fd.
	dupFd, err := unix.Dup(fd)
	if err != nil {
		return fd, fmt.Errorf("dup: %w", err)
	}

	f := os.NewFile(uintptr(dupFd), "ws-conn")
	if f == nil {
		unix.Close(dupFd)
		return fd, fmt.Errorf("os.NewFile failed")
	}

	nc, err := net.FileConn(f)
	// Close the *os.File immediately to release the dup'd fd from GC finalizer.
	// net.FileConn has already done its own dup internally, so nc uses yet another fd.
	f.Close()
	if err != nil {
		return fd, fmt.Errorf("net.FileConn: %w", err)
	}
	defer nc.Close() // Close the net.FileConn's internal dup

	nc.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	defer nc.SetDeadline(time.Time{})

	br := bufio.NewReader(nc)
	req, err := http.ReadRequest(br)
	if err != nil {
		return fd, fmt.Errorf("read request: %w", err)
	}

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

	// Dup the fd so net.FileConn gets its own copy that it can safely close.
	dupFd, err := unix.Dup(fd)
	if err != nil {
		return fmt.Errorf("dup: %w", err)
	}

	f := os.NewFile(uintptr(dupFd), "ws-conn")
	if f == nil {
		unix.Close(dupFd)
		return fmt.Errorf("os.NewFile failed")
	}

	nc, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("net.FileConn: %w", err)
	}
	defer nc.Close()

	nc.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	defer nc.SetDeadline(time.Time{})

	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	secKey, err := generateClientSecKey()
	if err != nil {
		return err
	}
	acceptKey := computeAcceptKey(secKey)

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

	if _, err := nc.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

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
