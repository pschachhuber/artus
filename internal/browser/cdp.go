package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// errConnClosed is returned for calls on a connection that has gone away,
// typically because the browser exited.
var errConnClosed = errors.New("browser connection closed")

// errTargetClosed is returned for calls on a detached page or frame target.
var errTargetClosed = errors.New("target closed")

// cdpError is an error reply from the browser.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return e.Message + ": " + e.Data
	}
	return e.Message
}

// cdpErrorContains reports whether err is a protocol error mentioning substr.
func cdpErrorContains(err error, substr string) bool {
	var ce *cdpError
	return errors.As(err, &ce) && strings.Contains(ce.Error(), substr)
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpPending struct {
	session string
	reply   chan cdpMessage
}

// cdpConn multiplexes DevTools protocol sessions over one WebSocket.
//
// Replies are routed straight to the waiting caller by the read loop. Events
// are queued and delivered in arrival order by a single dispatch goroutine, so
// handlers observe a consistent order and may themselves issue calls.
type cdpConn struct {
	ws   *wsConn
	done chan struct{}

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]cdpPending
	sessions map[string]*cdpSession
	queue    []cdpMessage
	wake     chan struct{}
}

// cdpSession is one protocol session: the browser itself (empty id) or an
// attached page, iframe or worker target.
type cdpSession struct {
	conn    *cdpConn
	id      string
	handler func(method string, params json.RawMessage)
}

// dialCDP connects to a browser-level DevTools WebSocket endpoint.
func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	ws, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &cdpConn{
		ws:       ws,
		done:     make(chan struct{}),
		pending:  map[int64]cdpPending{},
		sessions: map[string]*cdpSession{},
		wake:     make(chan struct{}, 1),
	}
	go c.readLoop()
	go c.dispatchLoop()
	return c, nil
}

// session registers a handler for events of the given session id and returns
// a handle for issuing calls on it. The empty id is the browser session.
func (c *cdpConn) session(id string, handler func(method string, params json.RawMessage)) *cdpSession {
	s := &cdpSession{conn: c, id: id, handler: handler}
	c.mu.Lock()
	c.sessions[id] = s
	c.mu.Unlock()
	return s
}

// dropSession forgets a detached session and fails its outstanding calls.
func (c *cdpConn) dropSession(id string) {
	c.mu.Lock()
	delete(c.sessions, id)
	for callID, p := range c.pending {
		if p.session == id {
			delete(c.pending, callID)
			close(p.reply)
		}
	}
	c.mu.Unlock()
}

func (c *cdpConn) readLoop() {
	defer c.shutdown()
	for {
		data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg cdpMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		c.mu.Lock()
		if msg.ID != 0 {
			p, ok := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ok {
				p.reply <- msg
			}
			continue
		}
		c.queue = append(c.queue, msg)
		c.mu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *cdpConn) dispatchLoop() {
	for {
		c.mu.Lock()
		batch := c.queue
		c.queue = nil
		c.mu.Unlock()
		for _, msg := range batch {
			c.mu.Lock()
			s := c.sessions[msg.SessionID]
			c.mu.Unlock()
			if s != nil && s.handler != nil {
				s.handler(msg.Method, msg.Params)
			}
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-c.wake:
		case <-c.done:
			return
		}
	}
}

func (c *cdpConn) shutdown() {
	c.mu.Lock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	for id, p := range c.pending {
		delete(c.pending, id)
		close(p.reply)
	}
	c.mu.Unlock()
	c.ws.Close()
}

// Close drops the connection; outstanding calls fail with errConnClosed.
func (c *cdpConn) Close() { c.shutdown() }

// call issues a protocol command and decodes its result into result, which
// may be nil. It returns early if ctx ends; the browser still completes the
// command, its reply is simply discarded.
func (s *cdpSession) call(ctx context.Context, method string, params, result any) error {
	return s.send(method, params).wait(ctx, result)
}

// pendingCall is a command that has been written to the browser.
type pendingCall struct {
	conn   *cdpConn
	id     int64
	method string
	reply  chan cdpMessage
	err    error
}

// send writes a command without waiting for its reply, so that several can
// be pipelined in order.
func (s *cdpSession) send(method string, params any) *pendingCall {
	c := s.conn
	p := &pendingCall{conn: c, method: method, reply: make(chan cdpMessage, 1)}
	msg := cdpMessage{Method: method, SessionID: s.id}
	if params != nil {
		if msg.Params, p.err = json.Marshal(params); p.err != nil {
			return p
		}
	}
	c.mu.Lock()
	select {
	case <-c.done:
		p.err = errConnClosed
	default:
		if _, ok := c.sessions[s.id]; !ok {
			p.err = errTargetClosed
		}
	}
	if p.err != nil {
		c.mu.Unlock()
		return p
	}
	c.nextID++
	p.id, msg.ID = c.nextID, c.nextID
	c.pending[p.id] = cdpPending{session: s.id, reply: p.reply}
	data, err := json.Marshal(msg)
	if err == nil {
		// Written under the lock so that commands reach the browser in the
		// order they were sent.
		err = c.ws.WriteMessage(data)
	}
	if err != nil {
		delete(c.pending, p.id)
		p.err = errConnClosed
	}
	c.mu.Unlock()
	return p
}

// wait blocks for the reply and decodes it into result, which may be nil.
func (p *pendingCall) wait(ctx context.Context, result any) error {
	if p.err != nil {
		return p.err
	}
	c := p.conn
	select {
	case r, ok := <-p.reply:
		if !ok {
			select {
			case <-c.done:
				return errConnClosed
			default:
				return errTargetClosed
			}
		}
		if r.Error != nil {
			return fmt.Errorf("%s: %w", p.method, r.Error)
		}
		if result != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, p.id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// isClosedErr reports whether err means the page, frame or browser is gone
// rather than that the command itself was wrong.
func isClosedErr(err error) bool {
	return errors.Is(err, errConnClosed) || errors.Is(err, errTargetClosed) ||
		cdpErrorContains(err, "Target closed") ||
		cdpErrorContains(err, "Session with given id not found") ||
		cdpErrorContains(err, "No target with given id")
}

// obj is shorthand for building protocol parameters.
type obj = map[string]any
