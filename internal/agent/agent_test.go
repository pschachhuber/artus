package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pschachhuber/artus/defaults"
	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/store"
)

type fixture struct {
	t      *testing.T
	e      *Engine
	mock   *llm.Mock
	mu     sync.Mutex
	events []Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home, err := store.Open(t.TempDir(), defaults.Home())
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.Open(home.Path(store.SecretsFile), home.Path("pass-session"), func(b []byte) error {
		return home.WriteLocked(store.SecretsFile, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	mock := &llm.Mock{}
	reg := llm.NewRegistry()
	reg.Register("mock", mock)
	home.UpdateConfig(func(c *store.Config) { c.Model = "mock/echo" })
	f := &fixture{t: t, mock: mock}
	f.e = New(home, sec, reg, defaults.Prompt)
	f.e.Emit = func(ev Event) {
		f.mu.Lock()
		f.events = append(f.events, ev)
		f.mu.Unlock()
	}
	f.e.Sched, err = sched.New(home.Path(store.ScheduleFile), func(b []byte) error {
		return home.WriteLocked(store.ScheduleFile, b)
	}, f.e.Fire)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.e.Shutdown)
	return f
}

// say sends a message to a new or existing chat and waits for the run to end.
func (f *fixture) say(chatID, text string) string {
	f.t.Helper()
	if chatID == "" {
		meta, err := f.e.NewChat(store.ChatMeta{})
		if err != nil {
			f.t.Fatal(err)
		}
		chatID = meta.ID
	}
	if err := f.e.Send(chatID, Input{Text: text}); err != nil {
		f.t.Fatal(err)
	}
	f.wait(chatID)
	return chatID
}

func (f *fixture) wait(chatID string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.e.Wait(ctx, chatID)
	if ctx.Err() != nil {
		f.t.Fatal("run did not finish")
	}
}

func call(name, input string) llm.Block {
	return llm.Block{Name: name, Input: json.RawMessage(input)}
}

// results returns the text of every tool result in a chat, in order.
func (f *fixture) results(chatID string) []llm.Block {
	var out []llm.Block
	for _, rec := range f.e.Records(chatID) {
		if rec.Msg != nil {
			for _, b := range rec.Msg.Blocks {
				if b.Type == llm.ToolResult {
					out = append(out, b)
				}
			}
		}
	}
	return out
}

func (f *fixture) lastText(chatID string) string {
	recs := f.e.Records(chatID)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Msg != nil && recs[i].Msg.Role == "assistant" {
			return recs[i].Msg.TextOf()
		}
	}
	return ""
}

func TestTurnWithToolsAndFrozenPrompt(t *testing.T) {
	f := newFixture(t)
	home := f.e.Home
	os.WriteFile(home.Path("memory.md"), []byte("The user is called Ada."), 0o600)
	os.WriteFile(home.Path("topics", "garden.md"), []byte("# Tomatoes need staking\nDetails."), 0o600)
	os.MkdirAll(home.Path("skills", "invoices"), 0o700)
	os.WriteFile(home.Path("skills", "invoices", "SKILL.md"), []byte("---\nname: invoices\ndescription: File the monthly invoices.\n---\nSteps."), 0o600)
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("write", `{"path":"topics/tea.md","content":"Oolong at 90C"}`)}},
		{Calls: []llm.Block{call("read", `{"path":"topics/tea.md"}`), call("grep", `{"pattern":"staking","path":"topics"}`)}},
		{Text: "Done."},
		{Text: "Second answer."},
	}
	id := f.say("", "note my tea preference")

	reqs := f.mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("want 3 requests, got %d", len(reqs))
	}
	sys := reqs[0].System
	for _, want := range []string{"The user is called Ada.", "topics/garden.md — Tomatoes need staking", "invoices — File the monthly invoices. (skills/invoices/SKILL.md)", home.Dir} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(sys, "{{") {
		t.Error("system prompt has an unfilled placeholder")
	}
	res := f.results(id)
	if len(res) != 3 || res[0].IsError || !strings.Contains(res[1].Text, "Oolong at 90C") || !strings.Contains(res[2].Text, "topics/garden.md:1:") {
		t.Fatalf("unexpected tool results: %+v", res)
	}
	if got := f.lastText(id); got != "Done." {
		t.Fatalf("final text = %q", got)
	}

	// The prompt and tools stay byte-identical within a segment even though a topic was added.
	f.say(id, "again")
	reqs = f.mock.Requests()
	last := reqs[len(reqs)-1]
	if last.System != sys {
		t.Error("system prompt changed inside a segment")
	}
	if a, _ := json.Marshal(last.Tools); string(a) != string(mustJSON(reqs[0].Tools)) {
		t.Error("tool list changed inside a segment")
	}
	// History is append-only: each request extends the previous one.
	for i := 1; i < len(reqs); i++ {
		prev, cur := mustJSON(reqs[i-1].Messages), mustJSON(reqs[i].Messages)
		if !strings.HasPrefix(string(cur[1:]), string(prev[1:len(prev)-1])) {
			t.Fatalf("request %d rewrote earlier history", i)
		}
	}
	meta, _ := home.Chat(id)
	if meta.Title != "note my tea preference" || meta.TokensIn == 0 {
		t.Errorf("meta not updated: %+v", meta)
	}
	if changes := home.Changes(5); len(changes) == 0 || !contains(changes[0].Files, "topics/tea.md") {
		t.Errorf("the edit was not recorded in the change log: %+v", changes)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestLockedFilesAndGuard(t *testing.T) {
	f := newFixture(t)
	home := f.e.Home
	f.e.Secrets.Set("SHOP_PASSWORD", "hunter2-very-secret")
	before, _ := os.ReadFile(home.Path(store.ConfigFile))
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{
			call("write", `{"path":"config.json","content":"{}"}`),
			call("edit", `{"path":".env","old_string":"SHOP_PASSWORD","new_string":"x"}`),
			call("write", `{"path":"chats/x.jsonl","content":"x"}`),
			call("read", `{"path":".env"}`),
		}},
		{Calls: []llm.Block{call("bash", `{"command":"echo '{\"model\":\"evil/model\"}' > config.json; cat .env"}`)}},
		{Text: "ok"},
	}
	id := f.say("", "try to change locked files")
	res := f.results(id)
	for i := 0; i < 3; i++ {
		if !res[i].IsError || !strings.Contains(res[i].Text, "locked") {
			t.Errorf("result %d should refuse a locked path: %+v", i, res[i])
		}
	}
	if strings.Contains(res[3].Text, "hunter2") || !strings.Contains(res[3].Text, "SHOP_PASSWORD=********") {
		t.Errorf(".env was not masked: %q", res[3].Text)
	}
	if strings.Contains(res[4].Text, "hunter2") || !strings.Contains(res[4].Text, "[secret:SHOP_PASSWORD]") {
		t.Errorf("cat .env leaked the secret: %q", res[4].Text)
	}
	after, _ := os.ReadFile(home.Path(store.ConfigFile))
	if string(after) != string(before) {
		t.Error("config.json was not restored after a shell command changed it")
	}
	if home.Config().Model != "mock/echo" {
		t.Error("in-memory config changed")
	}
	warned := false
	for _, rec := range f.e.Records(id) {
		if rec.Msg != nil && strings.Contains(rec.Msg.TextOf(), "has been restored") {
			warned = true
		}
	}
	if !warned {
		t.Error("the model was not told the file was restored")
	}
	for _, rec := range f.e.Records(id) {
		if strings.Contains(string(mustJSON(rec)), "hunter2") {
			t.Fatal("secret value reached the transcript")
		}
	}
}

func TestBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	f := newFixture(t)
	f.e.Secrets.Set("API_TOKEN", "tok_1234567890abcdef")
	sub := filepath.Join(f.e.Home.Dir, "topics")
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("bash", `{"command":"cd topics && pwd"}`)}},
		{Calls: []llm.Block{call("bash", `{"command":"pwd; echo token=$API_TOKEN; echo home=$ARTUS_HOME; echo key=${ANTHROPIC_API_KEY:-unset}"}`)}},
		{Calls: []llm.Block{call("bash", `{"command":"echo boom >&2; exit 3"}`)}},
		{Calls: []llm.Block{call("bash", `{"command":"sleep 30","timeout":1}`)}},
		{Calls: []llm.Block{call("bash", `{"command":"seq 1 200000"}`)}},
		{Text: "ok"},
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	started := time.Now()
	id := f.say("", "run things")
	if time.Since(started) > 20*time.Second {
		t.Error("the timeout did not stop the command")
	}
	res := f.results(id)
	if !strings.Contains(res[0].Text, sub) {
		t.Errorf("cd: %q", res[0].Text)
	}
	if !strings.HasPrefix(res[1].Text, sub) {
		t.Errorf("working directory did not persist: %q", res[1].Text)
	}
	if !strings.Contains(res[1].Text, "token=[secret:API_TOKEN]") || !strings.Contains(res[1].Text, "home="+f.e.Home.Dir) || !strings.Contains(res[1].Text, "key=unset") {
		t.Errorf("environment: %q", res[1].Text)
	}
	if !res[2].IsError || !strings.Contains(res[2].Text, "boom") || !strings.Contains(res[2].Text, "[exit code 3]") {
		t.Errorf("exit code: %+v", res[2])
	}
	if !res[3].IsError || !strings.Contains(res[3].Text, "timeout") {
		t.Errorf("timeout: %+v", res[3])
	}
	big := res[4].Text
	if len(big) > 40000 || !strings.Contains(big, "Full output") || !strings.HasPrefix(big, "1\n2\n") || !strings.HasSuffix(strings.TrimSpace(big), "200000") {
		t.Errorf("large output was not capped with head and tail kept (len %d)", len(big))
	}
	spill := big[strings.Index(big, "tmp/output-"):]
	spill = spill[:strings.Index(spill, ".txt")+4]
	if b, err := os.ReadFile(f.e.Home.Path(spill)); err != nil || !strings.Contains(string(b), "\n123456\n") {
		t.Errorf("full output not saved at %s: %v", spill, err)
	}
}

func TestSubAgentsRunInParallel(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.AllowedModels = []string{"mock/small"} })
	var mu sync.Mutex
	running, peak := 0, 0
	f.mock.Reply = func(req llm.Request) llm.Step {
		last := req.Messages[len(req.Messages)-1]
		if strings.Contains(req.System, "You are a sub-agent") {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			for _, tool := range req.Tools {
				if tool.Name == "agent" {
					return llm.Step{Text: "ERROR: sub-agent can delegate"}
				}
			}
			return llm.Step{Text: "report for: " + strings.TrimSpace(last.Blocks[len(last.Blocks)-1].Text)}
		}
		if len(req.Messages) == 1 {
			return llm.Step{Calls: []llm.Block{
				call("agent", `{"prompt":"research A","model":"mock/small"}`),
				call("agent", `{"prompt":"research B"}`),
				call("agent", `{"prompt":"research C","model":"other/forbidden"}`),
			}}
		}
		return llm.Step{Text: "combined"}
	}
	id := f.say("", "fan out")
	res := f.results(id)
	if res[0].Text != "report for: research A" || res[1].Text != "report for: research B" {
		t.Fatalf("sub-agent reports: %q / %q", res[0].Text, res[1].Text)
	}
	if !res[2].IsError || !strings.Contains(res[2].Text, "allowlist") {
		t.Errorf("a model off the allowlist must be refused: %+v", res[2])
	}
	if peak < 2 {
		t.Errorf("sub-agents did not run concurrently (peak %d)", peak)
	}
	chats, _ := f.e.Home.Chats()
	children := 0
	for _, c := range chats {
		if c.Kind == store.KindAgent && c.Parent == id && c.Depth == 1 {
			children++
		}
	}
	if children != 2 {
		t.Errorf("want 2 child chats, got %d", children)
	}
}

func TestCompaction(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.Context.CompactMaxTokens = 500 })
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("read", `{"path":"memory.md"}`)}, Usage: llm.Usage{Input: 300, Output: 10}},
		{Text: "long answer", Usage: llm.Usage{Input: 700, Output: 50}}, // over the 500 threshold
		{Calls: []llm.Block{call("write", `{"path":"memory.md","content":"User likes tea."}`)}, Usage: llm.Usage{Input: 800, Output: 5}},
		{Text: "## Goal\nKeep tea notes.\n## Next\nNothing.", Usage: llm.Usage{Input: 820, Output: 30}},
		{Text: "after compaction", Usage: llm.Usage{Input: 120, Output: 5}},
	}
	os.WriteFile(f.e.Home.Path("memory.md"), []byte("old"), 0o600)
	id := f.say("", "talk a lot")

	recs := f.e.Records(id)
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.T)
	}
	joined := strings.Join(kinds, " ")
	if !strings.HasSuffix(joined, "compact system msg") {
		t.Fatalf("transcript should end with a compaction and a new segment: %s", joined)
	}
	if b, _ := os.ReadFile(f.e.Home.Path("memory.md")); string(b) != "User likes tea." {
		t.Error("the pre-compaction save did not run")
	}
	f.say(id, "continue")
	reqs := f.mock.Requests()
	last := reqs[len(reqs)-1]
	if len(last.Messages) != 2 {
		t.Fatalf("after compaction the request should hold the summary and the new message, got %d messages", len(last.Messages))
	}
	if first := last.Messages[0].TextOf(); !strings.Contains(first, "Keep tea notes.") || !strings.Contains(first, "chats/"+id+".jsonl") {
		t.Errorf("summary message: %q", first)
	}
	if !strings.Contains(last.System, "User likes tea.") {
		t.Error("the new segment should pick up the updated memory")
	}
	if len(f.e.Records(id)) <= len(recs) {
		t.Error("the transcript on disk must keep everything")
	}
	info, err := f.e.Context(id)
	_, hasMemory := info.Breakdown["memory"]
	if err != nil || info.Threshold != 500 || len(info.Messages) != 3 || !hasMemory || info.Breakdown["tools"] == 0 {
		t.Errorf("context info: %+v %v", info.Breakdown, err)
	}
}

func TestQueuedMessageIsDeliveredAtToolBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	f := newFixture(t)
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("bash", `{"command":"sleep 0.6; echo slow"}`)}},
		{Text: "saw both"},
	}
	meta, _ := f.e.NewChat(store.ChatMeta{})
	f.e.Send(meta.ID, Input{Text: "start"})
	time.Sleep(200 * time.Millisecond)
	if live := f.e.Live(meta.ID); live == nil || live.State != "tool" {
		t.Fatalf("expected a running tool, got %+v", live)
	}
	f.e.Send(meta.ID, Input{Text: "also do this"})
	if live := f.e.Live(meta.ID); live == nil || len(live.Queue) != 1 {
		t.Fatalf("message was not queued: %+v", live)
	}
	f.wait(meta.ID)
	reqs := f.mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	msgs := reqs[1].Messages
	if got := msgs[len(msgs)-1].TextOf(); !strings.Contains(got, "also do this") {
		t.Errorf("queued message missing from the next request: %q", got)
	}
	if f.e.Live(meta.ID) != nil {
		t.Error("run should be idle")
	}
}

func TestApprovalAndSecretRequest(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.Autonomy = "confirm" })
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("write", `{"path":"a.txt","content":"1"}`)}},
		{Calls: []llm.Block{call("write", `{"path":"b.txt","content":"2"}`)}},
		{Calls: []llm.Block{call("write", `{"path":"c.txt","content":"3"}`)}},
		{Calls: []llm.Block{call("ask_secret", `{"name":"BANK_PIN","description":"Your PIN"}`)}},
		{Text: "done"},
	}
	meta, _ := f.e.NewChat(store.ChatMeta{})
	f.e.Send(meta.ID, Input{Text: "go"})
	answers := []string{"deny", "always", "pin-987654"} // c.txt needs no answer after "always"
	kinds := []string{}
	for _, ans := range answers {
		var ask *Ask
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if live := f.e.Live(meta.ID); live != nil && live.Ask != nil {
				ask = live.Ask
				break
			}
		}
		if ask == nil {
			t.Fatal("no question was asked")
		}
		kinds = append(kinds, ask.Kind)
		if !f.e.Answer(ask.ID, ans) {
			t.Fatal("answer was not accepted")
		}
		for f.e.Live(meta.ID) != nil && f.e.Live(meta.ID).Ask != nil && f.e.Live(meta.ID).Ask.ID == ask.ID {
			time.Sleep(5 * time.Millisecond)
		}
	}
	f.wait(meta.ID)
	if strings.Join(kinds, ",") != "approval,approval,secret" {
		t.Fatalf("questions asked: %v", kinds)
	}
	res := f.results(meta.ID)
	if !res[0].IsError || !strings.Contains(res[0].Text, "declined") {
		t.Errorf("denied write should fail: %+v", res[0])
	}
	if _, err := os.Stat(f.e.Home.Path("a.txt")); err == nil {
		t.Error("a denied write still happened")
	}
	for _, name := range []string{"b.txt", "c.txt"} {
		if _, err := os.Stat(f.e.Home.Path(name)); err != nil {
			t.Errorf("%s should have been written", name)
		}
	}
	if v, err := f.e.Secrets.Get(context.Background(), "BANK_PIN", ""); err != nil || v != "pin-987654" {
		t.Errorf("secret not stored: %q %v", v, err)
	}
	for _, rec := range f.e.Records(meta.ID) {
		if strings.Contains(string(mustJSON(rec)), "pin-987654") {
			t.Fatal("the secret value reached the transcript")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if strings.Contains(string(mustJSON(ev)), "pin-987654") {
			t.Fatal("the secret value reached the event stream")
		}
	}
}

func TestTaskRunAndSchedule(t *testing.T) {
	f := newFixture(t)
	home := f.e.Home
	home.SaveTask(store.Task{ID: "digest", Name: "News digest", Status: "active", Description: "Summarise the news."})
	os.WriteFile(home.Path("tasks", "digest", "NOTES.md"), []byte("Last run: nothing new."), 0o600)
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{
			call("schedule", `{"action":"set","when":"cron 0 7 * * 1-5","note":"morning run"}`),
			call("schedule", `{"action":"set","when":"every 5s"}`),
			call("schedule", `{"action":"set","when":"in 2h","target":"task:missing"}`),
			call("ask_secret", `{"name":"NEWS_KEY","description":"API key"}`),
		}},
		{Calls: []llm.Block{call("schedule", `{"action":"list"}`)}},
		{Text: "Report: scheduled."},
	}
	id, err := f.e.RunTask("digest", "Started by the user.")
	if err != nil {
		t.Fatal(err)
	}
	f.wait(id)
	reqs := f.mock.Requests()
	first := reqs[0].Messages[0].TextOf()
	for _, want := range []string{"Started by the user.", "Summarise the news.", "Last run: nothing new."} {
		if !strings.Contains(first, want) {
			t.Errorf("task input lacks %q", want)
		}
	}
	if !strings.Contains(reqs[0].System, `unattended as task "digest"`) {
		t.Error("task prompt section missing")
	}
	res := f.results(id)
	if res[0].IsError || !strings.Contains(res[0].Text, "task:digest") || !strings.Contains(res[0].Text, "weekdays at 07:00") {
		t.Errorf("schedule set: %+v", res[0])
	}
	if !res[1].IsError || !res[2].IsError || !res[3].IsError {
		t.Errorf("a too-short interval, a missing task and an unattended secret request must all fail: %q | %q | %q", res[1].Text, res[2].Text, res[3].Text)
	}
	if !strings.Contains(res[4].Text, "morning run") {
		t.Errorf("schedule list: %q", res[4].Text)
	}
	if triggers := f.e.Sched.List("task:digest"); len(triggers) != 1 {
		t.Errorf("want one stored trigger, got %d", len(triggers))
	}
	meta, _ := home.Chat(id)
	if meta.Kind != store.KindTask || !meta.Unread || meta.Task != "digest" {
		t.Errorf("task run meta: %+v", meta)
	}
	if _, err := f.e.RunTask("nope", ""); err == nil {
		t.Error("running a missing task should fail")
	}

	// A chat wake-up delivers the note into the existing conversation.
	chat := f.say("", "remind me later")
	f.mock.Steps = []llm.Step{{Text: "Reminder: stretch."}}
	f.e.Fire(sched.Trigger{ID: "t1", Target: "chat:" + chat, When: "every 1h", Note: "stretch"})
	time.Sleep(50 * time.Millisecond)
	f.wait(chat)
	if got := f.lastText(chat); got != "Reminder: stretch." {
		t.Errorf("wake-up reply = %q", got)
	}
	if meta, _ := home.Chat(chat); !meta.Unread {
		t.Error("a woken chat should be marked unread")
	}
}

func TestExternalTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	f := newFixture(t)
	dir := f.e.Home.Path("tools", "shout")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "tool.json"), []byte(`{"name":"shout","description":"Upper-case text.","parameters":{"type":"object","properties":{"text":{"type":"string"}}},"command":["sh","run.sh"],"secrets":["SHOUT_KEY"]}`), 0o600)
	os.WriteFile(filepath.Join(dir, "run.sh"), []byte("tr a-z A-Z; echo \" key=$SHOUT_KEY\""), 0o700)
	f.e.Secrets.Set("SHOUT_KEY", "k-abcdef123456")
	f.mock.Steps = []llm.Step{{Calls: []llm.Block{call("shout", `{"text":"hello"}`)}}, {Text: "ok"}}
	id := f.say("", "shout")
	reqs := f.mock.Requests()
	found := false
	for _, tool := range reqs[0].Tools {
		found = found || tool.Name == "shout"
	}
	if !found {
		t.Fatal("the agent-built tool was not declared")
	}
	if got := f.results(id)[0].Text; !strings.Contains(got, `{"TEXT":"HELLO"}`) || !strings.Contains(got, "key=[secret:SHOUT_KEY]") {
		t.Errorf("external tool result: %q", got)
	}
}

func TestStopAndErrors(t *testing.T) {
	f := newFixture(t)
	f.mock.Delay = 20 * time.Millisecond
	f.mock.Steps = []llm.Step{{Text: strings.Repeat("word ", 500)}}
	meta, _ := f.e.NewChat(store.ChatMeta{})
	f.e.Send(meta.ID, Input{Text: "talk"})
	time.Sleep(150 * time.Millisecond)
	if live := f.e.Live(meta.ID); live == nil || !strings.Contains(live.Text, "word") {
		t.Fatalf("no streamed text visible: %+v", live)
	}
	f.e.Stop(meta.ID)
	if f.e.Live(meta.ID) != nil {
		t.Fatal("still running after Stop")
	}
	recs := f.e.Records(meta.ID)
	if last := recs[len(recs)-1]; last.T != "note" || last.Note != "Stopped." {
		t.Errorf("want a Stopped note, got %+v", last)
	}

	f.mock.Delay = 0
	f.mock.Steps = []llm.Step{{Err: &llm.Error{Status: 401, Message: "invalid api key"}}}
	id := f.say("", "hi")
	recs = f.e.Records(id)
	if last := recs[len(recs)-1]; last.Level != "error" || !strings.Contains(last.Note, "invalid api key") {
		t.Errorf("provider error should be shown: %+v", last)
	}
	if err := f.e.Send("missing", Input{Text: "x"}); err == nil {
		t.Error("sending to a missing chat should fail")
	}
	if err := f.e.Send(id, Input{}); err == nil {
		t.Error("an empty message should be rejected")
	}
}

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.md", "topics/a/b.md", true},
		{"*.md", "x.txt", false},
		{"topics/**/*.md", "topics/a/b/c.md", true},
		{"topics/**/*.md", "topics/c.md", true},
		{"topics/*.md", "topics/a/c.md", false},
		{"**/*.{go,md}", "internal/x/y.go", true},
		{"**/*.{go,md}", "internal/x/y.txt", false},
		{"skills/*/SKILL.md", "skills/browser/SKILL.md", true},
		{"**", "any/thing", true},
		{"a/**/z", "a/z", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v", c.pattern, c.path, got)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	src := `<html><head><title>x</title><style>p{}</style></head><body><script>evil()</script>
<h2>Prices &amp; plans</h2><p>See <a href="/pricing">the  list</a>.<br>Second line</p>
<ul><li>One</li><li>Two <b>bold</b></li></ul><!-- hidden --><table><tr><td>a</td><td>b</td></tr></table></body></html>`
	got := htmlToText(src)
	for _, want := range []string{"## Prices & plans", "See [the list](/pricing).", "Second line", "- One", "- Two bold", "a | b"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"evil", "hidden", "p{}", "<"} {
		if strings.Contains(got, bad) {
			t.Errorf("should not contain %q:\n%s", bad, got)
		}
	}
}

func TestHistorySearch(t *testing.T) {
	f := newFixture(t)
	f.mock.Steps = []llm.Step{{Text: "Noted, locker 4471."}}
	old := f.say("", "The gym moved me to locker 4471 today.\nThe padlock is <blue> & new.")
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{
			call("grep", `{"pattern":"locker","path":"chats"}`),
			call("grep", `{"pattern":"(?i)PADLOCK is <blue> &"}`),
			call("grep", `{"pattern":"personal agent"}`),
			call("glob", `{"pattern":"chats/*"}`),
		}},
		{Text: "4471"},
	}
	id := f.say("", "what was my locker number?")
	res := f.results(id)
	day := time.Now().Format("2006-01-02")
	if !strings.Contains(res[0].Text, "chats/"+old+".jsonl:") || !strings.Contains(res[0].Text, "["+day+" user] The gym moved me to locker 4471 today.") ||
		!strings.Contains(res[0].Text, "["+day+" assistant] Noted, locker 4471.") {
		t.Errorf("history search should show who said what:\n%s", res[0].Text)
	}
	for i, r := range res[:3] {
		if strings.Contains(r.Text, id) {
			t.Errorf("search %d found the conversation it was run from:\n%s", i, r.Text)
		}
		if strings.Contains(r.Text, `{"t":`) || strings.Contains(r.Text, "\\u003c") {
			t.Errorf("search %d returned raw transcript JSON:\n%s", i, r.Text)
		}
	}
	if !strings.Contains(res[1].Text, "padlock is <blue> & new") {
		t.Errorf("a match after a line break should be shown in context:\n%s", res[1].Text)
	}
	if res[2].Text != "No matches." {
		t.Errorf("the stored system prompt must not be searched:\n%s", res[2].Text)
	}
	if res[3].Text != "chats/"+old+".jsonl\n" {
		t.Errorf("listing chats should show past transcripts only:\n%s", res[3].Text)
	}
	raw, _ := os.ReadFile(f.e.Home.Path("chats", old+".jsonl"))
	if !strings.Contains(string(raw), "<blue> & new") {
		t.Error("transcripts should keep <, > and & unescaped so ordinary tools can search them")
	}
}

func TestCheckpointAndTurnLimit(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.Limits.Turns = 45 })
	f.mock.Reply = func(llm.Request) llm.Step {
		return llm.Step{Calls: []llm.Block{call("glob", `{"pattern":"*.none"}`)}} // never finishes on its own
	}
	id := f.say("", "hunt forever")
	reqs := f.mock.Requests()
	if len(reqs) != 45 {
		t.Fatalf("the turn limit should stop the run at 45 requests, got %d", len(reqs))
	}
	checkpoints := 0
	for _, m := range reqs[len(reqs)-1].Messages {
		if strings.Contains(m.TextOf(), "<harness>Checkpoint: ") {
			checkpoints++
		}
	}
	if checkpoints != 2 {
		t.Errorf("want a checkpoint after 20 and 40 requests, got %d", checkpoints)
	}
	recs := f.e.Records(id)
	if last := recs[len(recs)-1]; last.Level != "error" || !strings.Contains(last.Note, "limits.turns") {
		t.Errorf("the user should be told why the run stopped: %+v", last)
	}
}
