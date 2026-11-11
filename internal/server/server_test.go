package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pschachhuber/artus/defaults"
	"github.com/pschachhuber/artus/internal/agent"
	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/store"
)

type fakeBrowser struct{ ran [][]string }

func (b *fakeBrowser) Run(_ context.Context, _ string, args []string) (string, []string, error) {
	b.ran = append(b.ran, args)
	return "ran " + strings.Join(args, " "), nil, nil
}
func (b *fakeBrowser) Status() any                { return []string{} }
func (b *fakeBrowser) ResolveHandoff(string) bool { return false }
func (b *fakeBrowser) Screencast(context.Context, string, func([]byte)) error {
	return nil
}
func (b *fakeBrowser) Input(string, json.RawMessage) error { return nil }

type fixture struct {
	t       *testing.T
	srv     *Server
	ts      *httptest.Server
	token   string
	mock    *llm.Mock
	browser *fakeBrowser
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home, err := store.Open(t.TempDir(), defaults.Home())
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := secrets.Open(home.Path(store.SecretsFile), home.Path("pass-session"), func(b []byte) error {
		return home.WriteLocked(store.SecretsFile, b)
	})
	mock := &llm.Mock{}
	reg := llm.NewRegistry()
	reg.Register("mock", mock)
	home.UpdateConfig(func(c *store.Config) { c.Model = "mock/echo" })
	engine := agent.New(home, sec, reg, defaults.Prompt)
	scheduler, _ := sched.New(home.Path(store.ScheduleFile), func(b []byte) error {
		return home.WriteLocked(store.ScheduleFile, b)
	}, engine.Fire)
	engine.Sched = scheduler
	f := &fixture{t: t, mock: mock, browser: &fakeBrowser{}, token: home.Config().Server.Token}
	ui := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Artus</title>")}}
	f.srv = New(Server{Home: home, Engine: engine, Secrets: sec, LLM: reg, Sched: scheduler, Browser: f.browser, UI: ui, Version: "test"})
	f.ts = httptest.NewServer(f.srv.Handler())
	t.Cleanup(func() { engine.Shutdown(); f.ts.Close() })
	return f
}

// do sends a request as the signed-in user unless headers say otherwise.
func (f *fixture) do(method, path string, body any, headers ...string) (*http.Response, []byte) {
	f.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	default:
		enc, _ := json.Marshal(b)
		r = bytes.NewReader(enc)
	}
	req, _ := http.NewRequest(method, f.ts.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+f.token)
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
		} else if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// call expects success and decodes the JSON reply.
func (f *fixture) call(method, path string, body, out any) {
	f.t.Helper()
	resp, data := f.do(method, path, body)
	if resp.StatusCode != 200 {
		f.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			f.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
}

func (f *fixture) status(method, path string, body any, headers ...string) int {
	f.t.Helper()
	resp, _ := f.do(method, path, body, headers...)
	return resp.StatusCode
}

func (f *fixture) idle(chatID string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f.srv.Engine.Wait(ctx, chatID)
	if ctx.Err() != nil {
		f.t.Fatal("run did not finish")
	}
}

func TestAccessControl(t *testing.T) {
	f := newFixture(t)
	if got := f.status("GET", "/api/state", nil, "Authorization", ""); got != 401 {
		t.Errorf("API without a token: %d", got)
	}
	if got := f.status("GET", "/api/state", nil, "Authorization", "Bearer wrong"); got != 401 {
		t.Errorf("API with a wrong token: %d", got)
	}
	resp, body := f.do("GET", "/", nil, "Authorization", "")
	if resp.StatusCode != 401 || !strings.Contains(string(body), "not signed in") {
		t.Errorf("the interface without a token: %d %s", resp.StatusCode, body)
	}
	// The sign-in link sets a strict, script-invisible cookie and drops the token from the URL.
	resp, _ = f.do("GET", "/?token="+f.token, nil, "Authorization", "")
	cookie := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" || !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Fatalf("sign-in: %d %q %q", resp.StatusCode, resp.Header.Get("Location"), cookie)
	}
	if got := f.status("GET", "/api/state", nil, "Authorization", "", "Cookie", strings.SplitN(cookie, ";", 2)[0]); got != 200 {
		t.Errorf("cookie sign-in failed: %d", got)
	}
	if got := f.status("GET", "/?token=nope", nil, "Authorization", ""); got != 401 {
		t.Errorf("wrong sign-in token: %d", got)
	}
	// DNS rebinding: a page on another name resolving to 127.0.0.1 is refused even with a valid token.
	if got := f.status("GET", "/api/state", nil, "Host", "evil.example:7717"); got != 403 {
		t.Errorf("foreign Host header: %d", got)
	}
	// Cross-site request forgery: a state-changing request from another origin is refused.
	if got := f.status("POST", "/api/chats", map[string]string{}, "Origin", "https://evil.example"); got != 403 {
		t.Errorf("cross-origin POST: %d", got)
	}
	if got := f.status("POST", "/api/chats", map[string]string{}, "Origin", f.ts.URL); got != 200 {
		t.Errorf("same-origin POST: %d", got)
	}
	// The token given to commands the agent runs reaches the agent API and nothing else.
	agentAuth := []string{"Authorization", "Bearer " + f.srv.AgentToken}
	if got := f.status("POST", "/api/agent/browser", map[string]any{"args": []string{"status"}}, agentAuth...); got != 200 {
		t.Errorf("agent token on the agent API: %d", got)
	}
	for _, path := range []string{"/api/state", "/api/secrets", "/api/chats"} {
		if got := f.status("GET", path, nil, agentAuth...); got != 401 {
			t.Errorf("agent token must not open %s: %d", path, got)
		}
	}
	if got := f.status("PUT", "/api/config", map[string]string{"autonomy": "auto"}, agentAuth...); got != 401 {
		t.Errorf("agent token must not change settings: %d", got)
	}
	resp, _ = f.do("GET", "/", nil)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("interface CSP = %q", csp)
	}
}

func TestChatOverHTTPAndEventStream(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("GET", f.ts.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	events := make(chan agent.Event, 512)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var ev agent.Event
				if json.Unmarshal([]byte(data), &ev) == nil && ev.Type != "" {
					events <- ev
				}
			}
		}
		close(events)
	}()

	var chat store.ChatMeta
	f.call("POST", "/api/chats", map[string]string{}, &chat)
	if chat.Model != "mock/echo" || chat.Effort != "medium" {
		t.Fatalf("defaults not applied: %+v", chat)
	}
	f.call("POST", "/api/chats/"+chat.ID+"/messages", map[string]string{"text": "hello there"}, nil)
	f.idle(chat.ID)

	var state struct {
		Meta    store.ChatMeta
		Records []agent.Record
		Live    *agent.Live
		Seq     int64
	}
	f.call("GET", "/api/chats/"+chat.ID, nil, &state)
	if state.Live != nil || state.Meta.Title != "hello there" || state.Seq == 0 {
		t.Fatalf("state: live=%v title=%q seq=%d", state.Live, state.Meta.Title, state.Seq)
	}
	var kinds []string
	for _, r := range state.Records {
		kinds = append(kinds, r.T)
		if r.T == "system" && (r.System != "" || r.Tools != nil) {
			t.Error("the prompt and tool schemas should not be sent with the transcript")
		}
	}
	if strings.Join(kinds, " ") != "system msg msg" {
		t.Fatalf("records: %v", kinds)
	}
	if got := state.Records[2].Msg.TextOf(); got != "You said: hello there" {
		t.Errorf("reply = %q", got)
	}

	seen := map[string]int{}
	var text string
	var last int64
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case ev := <-events:
			if ev.Seq <= last {
				t.Fatalf("sequence went backwards: %d after %d", ev.Seq, last)
			}
			last = ev.Seq
			seen[ev.Type]++
			if ev.Type == "delta" {
				b, _ := json.Marshal(ev.Data)
				var d llm.Delta
				json.Unmarshal(b, &d)
				if d.Kind == llm.Text {
					text += d.Text
				}
			}
			if ev.Seq >= state.Seq {
				break collect
			}
		case <-deadline:
			t.Fatalf("event stream stalled; saw %v", seen)
		}
	}
	if text != "You said: hello there" || seen["record"] != 3 || seen["live"] == 0 || seen["chat"] == 0 {
		t.Errorf("streamed text %q, events %v", text, seen)
	}

	// A client that reconnects with the last sequence it saw gets only what it missed.
	f.call("PATCH", "/api/chats/"+chat.ID, map[string]any{"title": "Renamed", "pinned": true}, nil)
	req2, _ := http.NewRequest("GET", f.ts.URL+"/api/events", nil)
	req2.Header.Set("Authorization", "Bearer "+f.token)
	req2.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resumed, err := http.DefaultClient.Do(req2.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Body.Close()
	sc := bufio.NewScanner(resumed.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first, ready := "", false
	for sc.Scan() {
		line := sc.Text()
		if first == "" && strings.HasPrefix(line, "id: ") {
			first = line
		}
		if line == "event: ready" {
			ready = true
			break
		}
	}
	if first != "id: 2" || !ready {
		t.Errorf("resume started at %q, ready=%v", first, ready)
	}
}

func TestUploadsForkAndFiles(t *testing.T) {
	f := newFixture(t)
	var chat store.ChatMeta
	f.call("POST", "/api/chats", map[string]string{}, &chat)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	var att agent.Attachment
	f.call("POST", "/api/chats/"+chat.ID+"/uploads?name=..%2F..%2Fmy+photo.png", png, &att)
	if att.Name != "my_photo.png" || att.Type != "image/png" || !strings.Contains(att.Path, "uploads") {
		t.Fatalf("upload: %+v", att)
	}
	var again agent.Attachment
	f.call("POST", "/api/chats/"+chat.ID+"/uploads?name=my+photo.png", png, &again)
	if again.Name != "my_photo-2.png" {
		t.Errorf("a second upload must not overwrite the first: %q", again.Name)
	}
	if got := f.status("POST", "/api/chats/"+chat.ID+"/messages", map[string]any{"text": "x", "files": []agent.Attachment{{Path: "/etc/passwd", Name: "passwd", Type: "text/plain"}}}); got != 400 {
		t.Errorf("an attachment outside the chat's uploads must be refused: %d", got)
	}
	f.call("POST", "/api/chats/"+chat.ID+"/messages", map[string]any{"text": "look", "files": []agent.Attachment{att}}, nil)
	f.idle(chat.ID)
	reqs := f.mock.Requests()
	blocks := reqs[len(reqs)-1].Messages[0].Blocks
	if len(blocks) != 3 || blocks[1].Type != llm.Image || !strings.Contains(blocks[2].Text, "uploads/"+chat.ID+"/my_photo.png (image/png, 72 bytes)") {
		t.Fatalf("message blocks: %+v", blocks)
	}

	// Files are served inert, and private files not at all.
	home := f.srv.Home
	os.WriteFile(home.Path("topics", "page.html"), []byte("<script>alert(1)</script>"), 0o600)
	os.WriteFile(home.Path(store.SecretsFile), []byte("K=secret-value\n"), 0o600)
	resp, body := f.do("GET", "/api/file?path=topics/page.html", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(string(body), "<script>") {
		t.Errorf("html must come back as inert text: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Security-Policy"))
	}
	resp, _ = f.do("GET", "/api/file?path="+att.Path, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("image: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, p := range []string{".env", "config.json", "schedule.json", "/etc/passwd", "../outside", ".git/config", "browser/profiles/default/Cookies", ""} {
		if got := f.status("GET", "/api/file?path="+p, nil); got != 403 {
			t.Errorf("GET file %q: %d, want 403", p, got)
		}
		if got := f.status("GET", "/api/fs/file?path="+p, nil); got != 403 {
			t.Errorf("read %q: %d, want 403", p, got)
		}
	}

	// Fork keeps the first records and nothing after them.
	var forked store.ChatMeta
	f.call("POST", "/api/chats/"+chat.ID+"/fork", map[string]int{"records": 1}, &forked)
	var state struct{ Records []agent.Record }
	f.call("GET", "/api/chats/"+forked.ID, nil, &state)
	if len(state.Records) != 1 || state.Records[0].T != "system" || forked.Model != chat.Model {
		t.Errorf("fork: %d records, model %q", len(state.Records), forked.Model)
	}
	if got := f.status("POST", "/api/chats/"+chat.ID+"/fork", map[string]int{"records": 999}); got != 400 {
		t.Errorf("fork past the end: %d", got)
	}
	// An attachment taken out of the composer can be deleted; one that was
	// sent cannot.
	if got := f.status("DELETE", "/api/chats/"+chat.ID+"/uploads/"+again.Name, nil); got != 200 {
		t.Errorf("removing an unsent upload: %d", got)
	}
	if _, err := os.Stat(again.Path); err == nil {
		t.Error("the removed upload is still on disk")
	}
	if got := f.status("DELETE", "/api/chats/"+chat.ID+"/uploads/"+att.Name, nil); got != 409 {
		t.Errorf("removing a sent attachment: %d, want 409", got)
	}

	// A fork gets its own copy of the attachments it refers to, so it
	// survives the original being deleted.
	f.call("GET", "/api/chats/"+chat.ID, nil, &state)
	var whole store.ChatMeta
	f.call("POST", "/api/chats/"+chat.ID+"/fork", map[string]int{"records": len(state.Records)}, &whole)
	f.call("DELETE", "/api/chats/"+chat.ID, nil, nil)
	if _, err := os.Stat(att.Path); err == nil {
		t.Error("deleting a chat should remove its uploads")
	}
	f.call("GET", "/api/chats/"+whole.ID, nil, &state)
	var carried string
	for _, rec := range state.Records {
		if rec.Msg == nil {
			continue
		}
		for _, b := range rec.Msg.Blocks {
			if b.Type == llm.Image {
				carried = b.Path
			}
		}
	}
	if !strings.Contains(filepath.ToSlash(carried), "uploads/"+whole.ID+"/") {
		t.Fatalf("the fork still points at the original's upload: %q", carried)
	}
	if _, err := os.Stat(carried); err != nil {
		t.Errorf("the fork lost its attachment: %v", err)
	}
}

func TestEditingTheHome(t *testing.T) {
	f := newFixture(t)
	home := f.srv.Home
	f.call("PUT", "/api/fs/file?path=topics/tea.md", "# Tea\nOolong.", nil)
	f.call("PUT", "/api/fs/file?path=instructions.md", "Always answer in haiku.", nil)
	if got := home.Restore(); len(got) != 0 {
		t.Errorf("an edit by the user is not tampering: %v", got)
	}
	var doc struct {
		Content string
		Exists  bool
	}
	f.call("GET", "/api/fs/file?path=instructions.md", nil, &doc)
	if doc.Content != "Always answer in haiku." {
		t.Errorf("instructions = %q", doc.Content)
	}
	f.call("GET", "/api/fs/file?path=memory.md", nil, &doc)
	if doc.Exists {
		t.Error("a missing file should read as empty, not fail")
	}
	for _, p := range []string{"config.json", ".env", "chats/x.jsonl", "uploads/x", "schedule.json", "browser/profiles/a"} {
		if got := f.status("PUT", "/api/fs/file?path="+p, "x"); got != 403 {
			t.Errorf("writing %s through the editor: %d, want 403", p, got)
		}
	}
	var index struct{ Topics, Skills []store.Entry }
	f.call("GET", "/api/fs", nil, &index)
	if len(index.Topics) != 1 || index.Topics[0].Description != "Tea" {
		t.Errorf("index: %+v", index)
	}
	if got := f.status("DELETE", "/api/fs/file?path=topics", nil); got != 403 {
		t.Errorf("deleting a top-level folder: %d", got)
	}
	f.call("DELETE", "/api/fs/file?path=topics/tea.md", nil, nil)

	var changes struct{ Changes []store.Change }
	f.call("GET", "/api/changes", nil, &changes)
	if len(changes.Changes) < 3 || !strings.HasPrefix(changes.Changes[0].Message, "Deleted by you") {
		t.Fatalf("change log: %+v", changes.Changes)
	}
	f.call("POST", "/api/changes/"+changes.Changes[0].Hash+"/revert", nil, nil)
	if _, err := os.Stat(home.Path("topics", "tea.md")); err != nil {
		t.Error("revert did not bring the topic back")
	}
}

func TestSettingsSecretsAndTasks(t *testing.T) {
	f := newFixture(t)
	var state struct {
		Config    store.Config
		Providers []struct {
			Name   string
			HasKey bool
		}
	}
	f.call("GET", "/api/state", nil, &state)
	if state.Config.Server.Token != "" {
		t.Fatal("the access token must never be sent to the interface")
	}
	cfg := state.Config
	cfg.Autonomy, cfg.Effort, cfg.AllowedModels = "confirm", "high", []string{"mock/small"}
	cfg.Server.Token = "attacker-chosen"
	f.call("PUT", "/api/config", cfg, nil)
	saved := f.srv.Home.Config()
	if saved.Autonomy != "confirm" || saved.Effort != "high" || saved.Server.Token != f.token {
		t.Errorf("config after save: %+v", saved)
	}
	for _, broken := range []map[string]any{{"effort": "extreme"}, {"autonomy": "yolo"}, {"model": "nope/x"}, {"allowedModels": []string{"plain"}}} {
		if got := f.status("PUT", "/api/config", broken); got != 400 {
			t.Errorf("config %v accepted: %d", broken, got)
		}
	}

	f.call("PUT", "/api/secrets/SHOP_TOKEN", map[string]string{"value": "shp_abcdefghijklmnop"}, nil)
	_, raw := f.do("GET", "/api/secrets", nil)
	if strings.Contains(string(raw), "shp_") || !strings.Contains(string(raw), "SHOP_TOKEN") {
		t.Fatalf("secret listing: %s", raw)
	}
	agentAuth := []string{"Authorization", "Bearer " + f.srv.AgentToken}
	if got := f.status("POST", "/api/agent/secret", map[string]string{"name": "NEW_KEY", "value": "generated-123456"}, agentAuth...); got != 200 {
		t.Errorf("agent adding a secret: %d", got)
	}
	for _, name := range []string{"SHOP_TOKEN", "OPENROUTER_API_KEY"} {
		if got := f.status("POST", "/api/agent/secret", map[string]string{"name": name, "value": "replaced-123456"}, agentAuth...); got != 400 {
			t.Errorf("agent replacing %s: %d, want 400", name, got)
		}
	}
	f.call("DELETE", "/api/secrets/SHOP_TOKEN", nil, nil)
	if f.srv.Secrets.Has("SHOP_TOKEN") {
		t.Error("secret not deleted")
	}

	f.mock.Reply = func(req llm.Request) llm.Step {
		if len(req.Messages) == 1 {
			return llm.Step{Calls: []llm.Block{{Name: "schedule", Input: json.RawMessage(`{"action":"set","when":"cron 0 8 * * *"}`)}}}
		}
		return llm.Step{Text: "Scheduled for every morning."}
	}
	var created struct {
		Task store.Task
		Chat string
	}
	f.call("POST", "/api/tasks", map[string]string{"name": "Morning News!", "description": "Summarise the headlines."}, &created)
	if created.Task.ID != "morning-news" {
		t.Fatalf("task id = %q", created.Task.ID)
	}
	f.idle(created.Chat)
	var list struct {
		Tasks []struct {
			store.Task
			Triggers []struct{ Text string }
			Runs     []store.ChatMeta
			Unread   int
		}
	}
	f.call("GET", "/api/tasks", nil, &list)
	var mine *struct {
		store.Task
		Triggers []struct{ Text string }
		Runs     []store.ChatMeta
		Unread   int
	}
	for i := range list.Tasks {
		if list.Tasks[i].ID == "morning-news" {
			mine = &list.Tasks[i]
		}
	}
	if mine == nil || len(mine.Triggers) != 1 || mine.Triggers[0].Text != "daily at 08:00" || len(mine.Runs) != 1 || mine.Unread != 1 {
		t.Fatalf("task view: %+v", mine)
	}
	if len(list.Tasks) != 2 {
		t.Errorf("the seeded maintenance task should be listed too: %d tasks", len(list.Tasks))
	}
	f.call("PATCH", "/api/tasks/morning-news", map[string]string{"status": "paused"}, nil)
	if got := f.status("POST", "/api/tasks/morning-news/run", nil); got != 400 {
		t.Errorf("running a paused task: %d", got)
	}
	if got := f.status("PATCH", "/api/tasks/morning-news", map[string]string{"status": "exploded"}); got != 400 {
		t.Errorf("bad status: %d", got)
	}
	f.call("DELETE", "/api/tasks/morning-news", nil, nil)
	if len(f.srv.Sched.List("task:morning-news")) != 0 {
		t.Error("deleting a task must remove its triggers")
	}
	if _, err := f.srv.Home.Chat(created.Chat); err == nil {
		t.Error("deleting a task must remove its runs")
	}
}
