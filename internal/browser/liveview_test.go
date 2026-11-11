package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

func TestHandoffResolvedByUser(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.run("open", e.site.url("/dialogs"))
	type reply struct {
		text string
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		text, err := e.try("handoff", "Please", "solve the CAPTCHA")
		done <- reply{text, err}
	}()
	ev := e.waitEvent("handoff")
	if ev.Session != "default" || ev.Reason != "Please solve the CAPTCHA" || ev.ID == "" || ev.URL != e.site.url("/dialogs") {
		t.Fatalf("unexpected hand-off event %+v", ev)
	}
	if st := e.m.Status(); len(st) != 1 || st[0].Handoff == nil || st[0].Handoff.ID != ev.ID {
		t.Fatalf("status does not show the hand-off: %+v", st)
	}
	mustContain(t, e.run("status"), "waiting for the user: Please solve the CAPTCHA")
	select {
	case r := <-done:
		t.Fatalf("handoff returned before being resolved: %+v", r)
	default:
	}
	// The user acts on the page while the agent waits; other commands still
	// work on the session.
	e.run("goto", e.site.url("/reset"))
	if e.m.ResolveHandoff("not-the-id") {
		t.Fatal("resolved an unknown hand-off")
	}
	if !e.m.ResolveHandoff(ev.ID) {
		t.Fatal("the pending hand-off was not found")
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, r.text, "The user completed the hand-off (Please solve the CAPTCHA).", "- Page Title: Reset", `heading "Reset your password"`)
	if done := e.waitEvent("handoff_done"); done.ID != ev.ID || done.Reason != "resolved" {
		t.Errorf("unexpected handoff_done event %+v", done)
	}
	if e.m.ResolveHandoff(ev.ID) || e.m.Status()[0].Handoff != nil {
		t.Error("the hand-off is still pending after it returned")
	}
}

func TestHandoffTimesOut(t *testing.T) {
	e := newEnv(t, envOptions{config: func(cfg *config) { cfg.Timeouts.Handoff = 250 }})
	e.run("open", e.site.url("/"))
	start := time.Now()
	out := e.run("handoff", "enter the SMS code")
	if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
		t.Errorf("handoff took %s", d)
	}
	mustContain(t, out, "The hand-off timed out after 250ms", "- Page Title: Home", "### Snapshot")
	if done := e.waitEvent("handoff_done"); done.Reason != "timeout" {
		t.Errorf("unexpected handoff_done event %+v", done)
	}
	// --timeout overrides the configured limit, and a cancelled command
	// leaves no hand-off behind.
	mustContain(t, e.run("handoff", "again", "--timeout", "100"), "timed out after 100ms")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for len(e.m.Status()) == 0 || e.m.Status()[0].Handoff == nil {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if _, err := e.m.Run(ctx, e.cwd, []string{"handoff", "cancel me", "--timeout", "60000"}); err == nil {
		t.Fatal("a cancelled hand-off reported success")
	}
	if e.m.Status()[0].Handoff != nil {
		t.Error("a cancelled hand-off is still pending")
	}
}

func TestScreencastAndLiveInput(t *testing.T) {
	e := newEnv(t, envOptions{})
	out := e.run("open", e.site.url("/form"))
	if err := e.m.Screencast(context.Background(), "nope", func([]byte) {}); err == nil {
		t.Fatal("screencast of a missing session should fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan []byte, 64)
	finished := make(chan error, 1)
	go func() {
		finished <- e.m.Screencast(ctx, "default", func(jpg []byte) {
			select {
			case frames <- jpg:
			default:
			}
		})
	}()
	var width, height int
	select {
	case frame := <-frames:
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("the frame is not a JPEG: %v", err)
		}
		width, height = cfg.Width, cfg.Height
	case <-time.After(15 * time.Second):
		t.Fatal("no screencast frame")
	}
	if width != 1280 || height != 720 {
		t.Logf("screencast frames are %dx%d", width, height)
	}

	// A user clicks into the e-mail field of the live view and types.
	var centre [2]float64
	rect := e.run("eval", "(() => { const r = document.querySelector('[name=email]').getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2]; })()")
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rect, "### Result\n")), &centre); err != nil {
		t.Fatalf("cannot parse %q: %v", rect, err)
	}
	// Positions are given in frame pixels and scaled to the page.
	x, y := centre[0]*float64(width)/1280, centre[1]*float64(height)/720
	for _, ev := range []InputEvent{
		{Type: "mousemove", X: x, Y: y},
		{Type: "mousedown", X: x, Y: y, Button: "left"},
		{Type: "mouseup", X: x, Y: y, Button: "left"},
		{Type: "keydown", Key: "h", Code: "KeyH"}, {Type: "keyup", Key: "h", Code: "KeyH"},
		{Type: "keydown", Key: "I", Code: "KeyI", Modifiers: modShift}, {Type: "keyup", Key: "I", Code: "KeyI", Modifiers: modShift},
		{Type: "text", Text: "@example.com"},
		{Type: "keydown", Key: "Tab", Code: "Tab"}, {Type: "keyup", Key: "Tab", Code: "Tab"},
		{Type: "wheel", X: x, Y: y, DeltaY: 50},
	} {
		if err := e.m.Input("default", ev); err != nil {
			t.Fatalf("input %+v: %v", ev, err)
		}
	}
	out = e.run("snapshot")
	mustContain(t, out, `textbox "Email"`, "]: hI@example.com", `textbox "Password" [active]`)
	if err := e.m.Input("default", InputEvent{Type: "bogus"}); err == nil {
		t.Error("an unknown input type was accepted")
	}
	if err := e.m.Input("nope", InputEvent{Type: "text", Text: "x"}); err == nil {
		t.Error("input to a missing session was accepted")
	}

	// The cast follows the current tab: a new tab produces a fresh frame.
	for len(frames) > 0 {
		<-frames
	}
	e.run("tab-new", e.site.url("/"))
	select {
	case <-frames:
	case <-time.After(15 * time.Second):
		t.Fatal("no frame after switching tabs")
	}
	cancel()
	if err := <-finished; err != context.Canceled {
		t.Errorf("Screencast returned %v", err)
	}
}
