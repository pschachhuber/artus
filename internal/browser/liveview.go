package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"time"
)

// screencastMeta relates screencast image pixels to CSS pixels of the page.
type screencastMeta struct {
	cssWidth, cssHeight float64
	imgWidth, imgHeight float64
}

// toCSS converts a position in the last screencast frame to viewport
// coordinates.
func (m screencastMeta) toCSS(x, y float64) point {
	if m.imgWidth == 0 || m.imgHeight == 0 || m.cssWidth == 0 || m.cssHeight == 0 {
		return point{x, y}
	}
	return point{x * m.cssWidth / m.imgWidth, y * m.cssHeight / m.imgHeight}
}

// liveInput is the pointer state of the user driving a session through the
// live view.
type liveInput struct {
	buttons  int
	lastDown time.Time
	lastAt   point
	clicks   int
}

// Screencast streams JPEG frames of the session's current tab to onFrame,
// following the current tab as it changes. It blocks until ctx is done or
// the session ends, and returns the reason. Frames are delivered from the
// calling goroutine; when onFrame is slow, intermediate frames are skipped.
func (m *Manager) Screencast(ctx context.Context, session string, onFrame func(jpeg []byte)) error {
	s := m.lookup(session)
	if s == nil {
		return fmt.Errorf("browser session %q is not running", session)
	}
	frames := make(chan []byte, 1)
	s.mu.Lock()
	s.viewerSeq++
	id := s.viewerSeq
	s.viewers[id] = frames
	if s.lastFrame != nil {
		frames <- s.lastFrame
	}
	s.mu.Unlock()
	s.syncScreencast()
	defer func() {
		s.mu.Lock()
		delete(s.viewers, id)
		s.mu.Unlock()
		s.syncScreencast()
	}()
	for {
		select {
		case frame := <-frames:
			onFrame(frame)
		case <-ctx.Done():
			return ctx.Err()
		case <-s.conn.done:
			return errConnClosed
		}
	}
}

// syncScreencast makes the browser cast the current tab exactly when someone
// is watching.
func (s *session) syncScreencast() {
	s.castMu.Lock()
	defer s.castMu.Unlock()
	s.mu.Lock()
	want := s.current
	if len(s.viewers) == 0 || s.closing {
		want = nil
	}
	have := s.casting
	s.casting = want
	if have != want {
		s.lastFrame = nil
	}
	s.mu.Unlock()
	if have == want {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if have != nil {
		have.sess.call(ctx, "Page.stopScreencast", nil, nil)
	}
	if want != nil {
		want.sess.call(ctx, "Page.startScreencast", obj{
			"format": "jpeg", "quality": 70, "maxWidth": 1600, "maxHeight": 1600, "everyNthFrame": 1,
		}, nil)
	}
}

func (s *session) onScreencastFrame(tg *target, params json.RawMessage) {
	var ev struct {
		Data     string `json:"data"`
		Metadata struct {
			DeviceWidth  float64 `json:"deviceWidth"`
			DeviceHeight float64 `json:"deviceHeight"`
		} `json:"metadata"`
		SessionID int `json:"sessionId"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tg.sess.call(ctx, "Page.screencastFrameAck", obj{"sessionId": ev.SessionID}, nil)
	}()
	frame, err := base64.StdEncoding.DecodeString(ev.Data)
	if err != nil {
		return
	}
	meta := screencastMeta{cssWidth: ev.Metadata.DeviceWidth, cssHeight: ev.Metadata.DeviceHeight}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame)); err == nil {
		meta.imgWidth, meta.imgHeight = float64(cfg.Width), float64(cfg.Height)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tg.tab != s.casting {
		return
	}
	tg.tab.cast, s.lastFrame = meta, frame
	for _, viewer := range s.viewers {
		// Replace a frame the viewer has not picked up yet.
		select {
		case <-viewer:
		default:
		}
		viewer <- frame
	}
}

// Input forwards a user input event from a live view to the session's
// current tab. Mouse positions are in pixels of the latest screencast frame.
func (m *Manager) Input(session string, ev InputEvent) error {
	s := m.lookup(session)
	if s == nil {
		return fmt.Errorf("browser session %q is not running", session)
	}
	s.mu.Lock()
	t := s.current
	if t == nil {
		s.mu.Unlock()
		return errNoTab
	}
	pt := t.cast.toCSS(ev.X, ev.Y)
	live := &s.live
	button := ev.Button
	if button == "" {
		button = "left"
	}
	held := "none"
	for name, bit := range mouseButtons {
		if live.buttons&bit != 0 {
			held = name
		}
	}
	switch ev.Type {
	case "mousedown":
		if time.Since(live.lastDown) < 500*time.Millisecond && live.lastAt == pt {
			live.clicks++
		} else {
			live.clicks = 1
		}
		live.lastDown, live.lastAt = time.Now(), pt
		live.buttons |= mouseButtons[button]
	case "mouseup":
		live.buttons &^= mouseButtons[button]
	}
	buttons, clicks := live.buttons, live.clicks
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch ev.Type {
	case "mousemove":
		return dispatchMouse(ctx, t.sess, "mouseMoved", pt, held, buttons, 0, ev.Modifiers, nil)
	case "mousedown":
		return dispatchMouse(ctx, t.sess, "mousePressed", pt, button, buttons, clicks, ev.Modifiers, nil)
	case "mouseup":
		return dispatchMouse(ctx, t.sess, "mouseReleased", pt, button, buttons, clicks, ev.Modifiers, nil)
	case "wheel":
		return dispatchMouse(ctx, t.sess, "mouseWheel", pt, "none", buttons, 0, ev.Modifiers, obj{"deltaX": ev.DeltaX, "deltaY": ev.DeltaY})
	case "keydown", "keyup":
		def, ok := keyboard[ev.Key]
		if !ok {
			def, ok = keyboard[ev.Code]
		}
		if !ok {
			if len([]rune(ev.Key)) != 1 {
				return fmt.Errorf("unknown key %q", ev.Key)
			}
			def = keyDef{key: ev.Key, code: ev.Code, text: ev.Key}
		}
		return dispatchKey(ctx, t.sess, ev.Type == "keydown", def, ev.Modifiers)
	case "text":
		return t.sess.call(ctx, "Input.insertText", obj{"text": ev.Text}, nil)
	}
	return fmt.Errorf("unknown input event type %q", ev.Type)
}
