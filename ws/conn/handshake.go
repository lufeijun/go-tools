package conn

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var errInvalidHandshake = errors.New("invalid websocket handshake")

// ServerHandshake validates the HTTP Upgrade request and returns the raw net.Conn.
func ServerHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return nil, errInvalidHandshake
	}
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "Upgrade required", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "Connection upgrade required", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}
	secKey := r.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		http.Error(w, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "Unsupported WebSocket version", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}

	acceptKey := computeAcceptKey(secKey)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return nil, errInvalidHandshake
	}
	netConn, bw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n\r\n"
	if _, err := bw.WriteString(response); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}
	return netConn, nil
}

// ClientHandshake initiates a client WebSocket connection and returns the raw net.Conn.
func ClientHandshake(rawURL string, headers http.Header) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	useTLS := false
	switch u.Scheme {
	case "ws":
		u.Scheme = "tcp"
	case "wss":
		u.Scheme = "tcp"
		useTLS = true
	default:
		return nil, errors.New("invalid scheme: use ws:// or wss://")
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if useTLS {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var netConn net.Conn
	if useTLS {
		netConn, err = tls.Dial("tcp", host, nil)
	} else {
		netConn, err = net.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}
	secKey := generateClientSecKey()
	acceptKey := computeAcceptKey(secKey)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + secKey + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if headers != nil {
		for k, vs := range headers {
			for _, v := range vs {
				req += k + ": " + v + "\r\n"
			}
		}
	}
	req += "\r\n"
	if _, err := netConn.Write([]byte(req)); err != nil {
		netConn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(netConn), nil)
	if err != nil {
		netConn.Close()
		return nil, err
	}
	if resp.StatusCode != 101 {
		netConn.Close()
		return nil, errors.New("server returned status " + resp.Status)
	}
	if resp.Header.Get("Upgrade") != "websocket" {
		netConn.Close()
		return nil, errInvalidHandshake
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey {
		netConn.Close()
		return nil, errors.New("invalid Sec-WebSocket-Accept")
	}
	return netConn, nil
}

func computeAcceptKey(secKey string) string {
	h := sha1.New()
	h.Write([]byte(secKey + websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func generateClientSecKey() string {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		panic("handshake: failed to generate random key: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(key)
}
