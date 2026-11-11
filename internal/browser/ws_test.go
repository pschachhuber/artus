package browser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsTestServer upgrades a connection and hands the raw stream to serve.
func wsTestServer(t *testing.T, serve func(rw *bufio.ReadWriter)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			t.Error("the client sent an Origin header; Chrome rejects those")
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		serve(rw)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// serverFrame builds an unmasked frame as a server sends it.
func serverFrame(fin bool, op byte, payload []byte) []byte {
	b := []byte{op}
	if fin {
		b[0] |= 0x80
	}
	switch n := len(payload); {
	case n <= 125:
		b = append(b, byte(n))
	case n <= 0xffff:
		b = append(b, 126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	return append(b, payload...)
}

// readClientFrame reads one frame from a client and unmasks it, insisting
// that it was masked as the protocol requires.
func readClientFrame(t *testing.T, r io.Reader) (op byte, payload []byte) {
	t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Error(err)
		return 0, nil
	}
	if hdr[1]&0x80 == 0 {
		t.Error("client frame is not masked")
	}
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		io.ReadFull(r, ext[:])
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		io.ReadFull(r, ext[:])
		n = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	io.ReadFull(r, mask[:])
	payload = make([]byte, n)
	io.ReadFull(r, payload)
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return hdr[0] & 0x0f, payload
}

func TestWebSocketFraming(t *testing.T) {
	small := []byte(`{"id":1}`)
	medium := bytes.Repeat([]byte("m"), 300)       // 16-bit length
	large := bytes.Repeat([]byte("L"), 200<<10)    // 64-bit length, over 64 KiB
	fragmented := bytes.Repeat([]byte("f"), 70000) // sent in three frames
	echoed := make(chan []byte, 3)
	url := wsTestServer(t, func(rw *bufio.ReadWriter) {
		rw.Write(serverFrame(true, wsText, small))
		rw.Write(serverFrame(true, wsText, medium))
		rw.Write(serverFrame(true, wsBinary, large))
		// A fragmented message with a ping in the middle of it.
		rw.Write(serverFrame(false, wsText, fragmented[:10]))
		rw.Write(serverFrame(true, wsPing, []byte("are you there")))
		rw.Write(serverFrame(false, wsContinuation, fragmented[10:66000]))
		rw.Write(serverFrame(true, wsContinuation, fragmented[66000:]))
		rw.Flush()
		for i := 0; i < 4; i++ {
			op, payload := readClientFrame(t, rw)
			if op == wsPong {
				if string(payload) != "are you there" {
					t.Errorf("pong payload %q", payload)
				}
				continue
			}
			echoed <- payload
		}
		rw.Write(serverFrame(true, wsClose, nil))
		rw.Flush()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialWS(ctx, url+"/devtools/browser/x")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, want := range [][]byte{small, medium, large, fragmented} {
		got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("message %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
	// What we write must arrive intact for every length encoding.
	for _, msg := range [][]byte{small, medium, large} {
		if err := c.WriteMessage(msg); err != nil {
			t.Fatal(err)
		}
		if got := <-echoed; !bytes.Equal(got, msg) {
			t.Fatalf("server received %d bytes, want %d", len(got), len(msg))
		}
	}
	if _, err := c.ReadMessage(); err != io.EOF {
		t.Fatalf("after a close frame ReadMessage returned %v, want io.EOF", err)
	}
}

func TestWebSocketHandshakeFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	if _, err := dialWS(ctx, "ws"+strings.TrimPrefix(plain.URL, "http")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("non-upgrading server: %v", err)
	}
	badAccept := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
		rw.Flush()
	}))
	defer badAccept.Close()
	if _, err := dialWS(ctx, "ws"+strings.TrimPrefix(badAccept.URL, "http")); err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Errorf("wrong accept key: %v", err)
	}
	if _, err := dialWS(ctx, "http://example.invalid/"); err == nil {
		t.Error("an http:// URL was accepted")
	}
	// A server that never answers the handshake must not hang the dial.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if _, err := dialWS(short, "ws://"+ln.Addr().String()); err == nil {
		t.Error("dial did not give up when the context ended")
	}
}

func TestCDPCallsAndEvents(t *testing.T) {
	url := wsTestServer(t, func(rw *bufio.ReadWriter) {
		for {
			op, payload := readClientFrame(t, rw)
			if op != wsText {
				return
			}
			switch {
			case bytes.Contains(payload, []byte(`"Test.ok"`)):
				// Events sent before the reply must be handled in order.
				rw.Write(serverFrame(true, wsText, []byte(`{"method":"Test.event","params":{"n":1},"sessionId":"S"}`)))
				rw.Write(serverFrame(true, wsText, []byte(`{"method":"Test.event","params":{"n":2},"sessionId":"S"}`)))
				rw.Write(serverFrame(true, wsText, []byte(`{"id":1,"result":{"answer":42}}`)))
			case bytes.Contains(payload, []byte(`"Test.fail"`)):
				rw.Write(serverFrame(true, wsText, []byte(`{"id":2,"error":{"code":-32000,"message":"No node with given id found"}}`)))
			case bytes.Contains(payload, []byte(`"Test.hang"`)):
				continue
			}
			rw.Flush()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialCDP(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 4)
	conn.session("S", func(method string, params json.RawMessage) { events <- method + string(params) })
	root := conn.session("", nil)
	var res struct{ Answer int }
	if err := root.call(ctx, "Test.ok", obj{"x": 1}, &res); err != nil || res.Answer != 42 {
		t.Fatalf("call: %v, %+v", err, res)
	}
	if a, b := <-events, <-events; a != `Test.event{"n":1}` || b != `Test.event{"n":2}` {
		t.Fatalf("events out of order: %s, %s", a, b)
	}
	err = root.call(ctx, "Test.fail", nil, nil)
	if err == nil || !cdpErrorContains(err, "No node with given id") || !isStale(err) || err.Error() != "Test.fail: No node with given id found" {
		t.Fatalf("protocol error: %v", err)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := root.call(short, "Test.hang", nil, nil); err != context.DeadlineExceeded {
		t.Fatalf("hanging call: %v", err)
	}
	// Calls on a dropped session and on a closed connection fail fast.
	gone := conn.session("gone", nil)
	conn.dropSession("gone")
	if err := gone.call(ctx, "Test.ok", nil, nil); err != errTargetClosed {
		t.Fatalf("dropped session: %v", err)
	}
	pending := make(chan error, 1)
	go func() { pending <- root.call(ctx, "Test.hang", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	conn.Close()
	if err := <-pending; err != errConnClosed {
		t.Fatalf("pending call after close: %v", err)
	}
	if err := root.call(ctx, "Test.ok", nil, nil); err != errConnClosed || !isClosedErr(err) {
		t.Fatalf("call after close: %v", err)
	}
}
