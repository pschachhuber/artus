package agent

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

// waitAsk returns the question a chat is showing, waiting for one to appear.
func (f *fixture) waitAsk(chatID string) *Ask {
	f.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if live := f.e.Live(chatID); live != nil && live.Ask != nil {
			return live.Ask
		}
	}
	f.t.Fatal("no question was asked")
	return nil
}

// In confirm mode, starting a task run is approved like any command, since
// the run itself never asks.
func TestScheduleNeedsApprovalInConfirmMode(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.Autonomy = "confirm" })
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("schedule", `{"action":"set","target":"task:maintenance","when":"now","note":"run rm -rf"}`)}},
		{Text: "ok"},
	}
	meta, _ := f.e.NewChat(store.ChatMeta{})
	f.e.Send(meta.ID, Input{Text: "go"})
	if ask := f.waitAsk(meta.ID); ask.Tool != "schedule" {
		t.Fatalf("asked about %q", ask.Tool)
	} else {
		f.e.Answer(ask.ID, "deny")
	}
	f.wait(meta.ID)
	if res := f.results(meta.ID); !res[0].IsError || !strings.Contains(res[0].Text, "declined") {
		t.Errorf("result: %+v", res[0])
	}
	if got := f.e.Sched.List("task:maintenance"); len(got) != 0 {
		t.Errorf("a trigger was set without approval: %+v", got)
	}
}

// A command cannot hold the agent past its timeout, or past its own end,
// through processes it leaves behind.
func TestShellCannotOutliveItsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX job control")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("needs setsid")
	}
	f := newFixture(t)
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("bash", `{"command":"setsid sleep 30","timeout":1}`)}},
		{Calls: []llm.Block{call("bash", `{"command":"sleep 30 & echo started"}`)}},
		{Text: "ok"},
	}
	started := time.Now()
	id := f.say("", "go")
	if took := time.Since(started); took > 12*time.Second {
		t.Errorf("the run took %v", took)
	}
	res := f.results(id)
	if !res[0].IsError || !strings.Contains(res[0].Text, "timeout") {
		t.Errorf("timeout: %+v", res[0])
	}
	if res[1].IsError || !strings.Contains(res[1].Text, "started") || !strings.Contains(res[1].Text, "still runs") {
		t.Errorf("background job: %+v", res[1])
	}
}

// A transcript that ends in tool calls without results (a crash mid-tool)
// is closed before the next request, which providers would reject otherwise.
func TestDanglingToolCallsAreClosed(t *testing.T) {
	f := newFixture(t)
	id := f.say("", "hello")
	f.e.Home.Append(id, Record{T: "msg", Msg: &llm.Message{Role: "assistant", Model: "mock/echo",
		Blocks: []llm.Block{{Type: llm.ToolCall, ID: "call_lost", Name: "bash", Input: []byte(`{"command":"make"}`)}}}})
	f.mock.Steps = []llm.Step{{Text: "back"}}
	f.say(id, "are you there?")
	reqs := f.mock.Requests()
	msgs := reqs[len(reqs)-1].Messages
	var closed bool
	for i, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.ToolResult && b.ID == "call_lost" && b.IsError && i < len(msgs)-1 {
				closed = true
			}
		}
	}
	if !closed {
		t.Errorf("the dangling call was not answered before the new message: %+v", msgs)
	}
}

// Stopping a compaction leaves the conversation as it was before it.
func TestStoppedCompactionLeavesNothingBehind(t *testing.T) {
	f := newFixture(t)
	id := f.say("", "hello")
	f.mock.Delay = 30 * time.Millisecond
	f.mock.Steps = []llm.Step{{Text: strings.Repeat("summary ", 400)}}
	done := make(chan error, 1)
	go func() { done <- f.e.Compact(id) }()
	time.Sleep(300 * time.Millisecond)
	f.e.Stop(id)
	<-done
	meta, _ := f.e.Home.Chat(id)
	s := f.e.load(meta)
	last := s.messages[len(s.messages)-1]
	if last.Role != "assistant" || strings.Contains(last.TextOf(), "summary") {
		t.Errorf("the stopped compaction left %q (%s) at the end", last.TextOf(), last.Role)
	}
}

// Questions from parallel sub-agents are shown one after the other.
func TestQuestionsTakeTurns(t *testing.T) {
	f := newFixture(t)
	root := &run{chat: "root", root: "root", cancel: func() {}, done: make(chan struct{})}
	f.e.mu.Lock()
	f.e.runs["root"] = root
	f.e.mu.Unlock()
	defer func() {
		f.e.mu.Lock()
		delete(f.e.runs, "root")
		f.e.mu.Unlock()
	}()
	answers := make(chan string, 2)
	for _, name := range []string{"a", "b"} {
		child := &run{chat: "child-" + name, root: "root", done: make(chan struct{})}
		go func() { answers <- f.e.ask(context.Background(), child, Ask{Kind: "approval", Tool: name}) }()
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ask := f.waitAsk("root")
		seen[ask.Tool] = true
		f.e.Answer(ask.ID, "allow")
		if got := <-answers; got != "allow" {
			t.Fatalf("answer %d = %q", i, got)
		}
		for live := f.e.Live("root"); live != nil && live.Ask != nil && live.Ask.ID == ask.ID; live = f.e.Live("root") {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("questions shown: %v", seen)
	}
}

func pngBytes(t *testing.T) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 4, 4)))
	return b.Bytes()
}

// Only files a provider accepts are attached; a file read is frozen as seen.
func TestAttachmentsAreCheckedAndFrozen(t *testing.T) {
	f := newFixture(t)
	dir := f.e.Home.Path("uploads", "x")
	os.MkdirAll(dir, 0o700)
	svg, broken := dir+"/a.svg", dir+"/b.png"
	os.WriteFile(svg, []byte("<svg/>"), 0o600)
	os.WriteFile(broken, []byte("not a png"), 0o600)
	msg := userMessage(Input{Text: "look", Files: []Attachment{
		{Path: svg, Name: "a.svg", Type: "image/svg+xml", Size: 6},
		{Path: broken, Name: "b.png", Type: "image/png", Size: 9},
	}}, f.e.Home)
	for _, b := range msg.Blocks {
		if b.Type == llm.Image {
			t.Errorf("%s was attached as an image", b.Path)
		}
	}
	if text := msg.TextOf(); strings.Count(text, "not shown to you") != 2 {
		t.Errorf("the model is not told why: %q", text)
	}

	plot := f.e.Home.Path("topics", "plot.png")
	os.WriteFile(plot, pngBytes(t), 0o600)
	f.mock.Steps = []llm.Step{{Calls: []llm.Block{call("read", `{"path":"topics/plot.png"}`)}}, {Text: "a grey square"}}
	id := f.say("", "what is in topics/plot.png?")
	os.WriteFile(plot, []byte("replaced"), 0o600)
	var shown string
	for _, rec := range f.e.Records(id) {
		if rec.Msg != nil {
			for _, b := range rec.Msg.Blocks {
				if b.Type == llm.Image {
					shown = b.Path
				}
			}
		}
	}
	if b, err := os.ReadFile(shown); err != nil || !bytes.Equal(b, pngBytes(t)) || !strings.Contains(shown, "uploads") {
		t.Errorf("the image the model saw is not kept as it was: %s, %v", shown, err)
	}
}

// Turning web search on or off applies from the next context segment, so
// the current one keeps its exact prefix.
func TestWebSearchIsFixedPerSegment(t *testing.T) {
	f := newFixture(t)
	f.e.Home.UpdateConfig(func(c *store.Config) { c.WebSearch = true })
	id := f.say("", "one")
	f.e.Home.UpdateConfig(func(c *store.Config) { c.WebSearch = false })
	f.say(id, "two")
	f.say("", "three")
	reqs := f.mock.Requests()
	if !reqs[1].WebSearch || reqs[2].WebSearch {
		t.Errorf("web search per request: %v %v %v", reqs[0].WebSearch, reqs[1].WebSearch, reqs[2].WebSearch)
	}
}

// A tool manifest that would not be offered says so when written.
func TestBrokenToolManifestIsReported(t *testing.T) {
	f := newFixture(t)
	f.mock.Steps = []llm.Step{
		{Calls: []llm.Block{call("write", `{"path":"tools/bad tool/tool.json","content":"{\"name\":\"bad tool\",\"description\":\"x\",\"command\":[\"./run\"]}"}`)}},
		{Text: "ok"},
	}
	id := f.say("", "make a tool")
	if res := f.results(id); !strings.Contains(res[0].Text, "will not be offered") {
		t.Errorf("result: %q", res[0].Text)
	}
}

// A turn the model's provider declined is discarded: its tools never run
// and nothing of it is replayed.
func TestRefusedTurnIsDiscarded(t *testing.T) {
	f := newFixture(t)
	f.mock.Steps = []llm.Step{
		{Thinking: "half a thought", Text: "Sure, writing", Calls: []llm.Block{call("write", `{"path":"a.txt","content":"x"}`)}, Stop: llm.StopRefusal},
		{Text: "fine"},
	}
	id := f.say("", "do it")
	if _, err := os.Stat(f.e.Home.Path("a.txt")); err == nil {
		t.Error("a tool of the declined turn ran")
	}
	var noted bool
	for _, rec := range f.e.Records(id) {
		if rec.Msg != nil && rec.Msg.Role == "assistant" {
			t.Errorf("the declined turn was stored: %+v", rec.Msg)
		}
		noted = noted || (rec.T == "note" && strings.Contains(rec.Note, "declined"))
	}
	if !noted {
		t.Error("the user is not told")
	}
	f.say(id, "then something else")
	reqs := f.mock.Requests()
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "assistant" {
			t.Errorf("the declined turn was replayed: %+v", m)
		}
	}
}
