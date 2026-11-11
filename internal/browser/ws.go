package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket opcodes (RFC 6455 §5.2).
const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA
)

// wsMaxMessage bounds a single reassembled message. DevTools replies carrying
// screenshots are large, but nothing legitimate comes close to this.
const wsMaxMessage = 512 << 20

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn is a minimal RFC 6455 client: just enough to carry the DevTools
// protocol. Reads must come from a single goroutine; writes are serialised.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex
}

// dialWS opens a WebSocket connection to a ws:// or wss:// URL.
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	var conn net.Conn
	switch u.Scheme {
	case "ws":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", host)
	case "wss":
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
		conn, err = (&tls.Dialer{Config: &tls.Config{ServerName: u.Hostname()}}).DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	c := &wsConn{conn: conn, br: bufio.NewReaderSize(conn, 64<<10)}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = c.handshake(u)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake with %s: %w", u.Host, err)
	}
	return c, nil
}

func (c *wsConn) handshake(u *url.URL) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c.conn, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(c.br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return errors.New("server did not upgrade to websocket")
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return errors.New("bad Sec-WebSocket-Accept")
	}
	return nil
}

// ReadMessage returns the next complete text or binary message, reassembling
// fragments and answering control frames along the way.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return nil, err
			}
		case wsPong:
		case wsClose:
			c.writeFrame(wsClose, nil)
			return nil, io.EOF
		case wsText, wsBinary:
			if started {
				return nil, errors.New("websocket: new message before previous one finished")
			}
			started, msg = true, payload
		case wsContinuation:
			if !started {
				return nil, errors.New("websocket: unexpected continuation frame")
			}
			if len(msg)+len(payload) > wsMaxMessage {
				return nil, errors.New("websocket: message too large")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %#x", op)
		}
		if started && fin && op < wsClose {
			return msg, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [8]byte
	if _, err = io.ReadFull(c.br, hdr[:2]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	op = hdr[0] & 0x0f
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	masked := hdr[1]&0x80 != 0
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		if _, err = io.ReadFull(c.br, hdr[:2]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(hdr[:2]))
	case 127:
		if _, err = io.ReadFull(c.br, hdr[:8]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(hdr[:8])
	}
	if op >= wsClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if n > wsMaxMessage {
		return false, 0, nil, errors.New("websocket: frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, op, payload, nil
}

// WriteMessage sends data as a single masked text frame.
func (c *wsConn) WriteMessage(data []byte) error {
	return c.writeFrame(wsText, data)
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	buf := make([]byte, 0, len(payload)+14)
	buf = append(buf, 0x80|op)
	switch n := len(payload); {
	case n <= 125:
		buf = append(buf, 0x80|byte(n))
	case n <= 0xffff:
		buf = append(buf, 0x80|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, 0x80|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	buf = append(buf, mask[:]...)
	start := len(buf)
	buf = append(buf, payload...)
	for i := start; i < len(buf); i++ {
		buf[i] ^= mask[(i-start)&3]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(buf)
	return err
}

// Close sends a close frame on a best-effort basis and drops the connection.
func (c *wsConn) Close() error {
	c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	c.writeFrame(wsClose, nil)
	return c.conn.Close()
}
