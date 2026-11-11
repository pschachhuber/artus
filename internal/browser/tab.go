package browser

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// tab is a page target. Its mutable state is guarded by the session's mutex.
type tab struct {
	s        *session
	targetID string
	openerID string
	sess     *cdpSession
	ready    chan struct{} // closed once the page session is configured
	helper   bool          // internal tab, hidden from the tab list

	closed    bool
	crashed   bool
	mainFrame string
	frames    map[string]*frame
	frameSeq  int
	navSeq    int // bumped whenever the main frame's URL changes
	dialog    *dialog
	chooser   *fileChooser
	dragData  json.RawMessage // data of an intercepted drag
	refs      map[string]nodeRef
	refSeq    int // last ref number used in the main frame

	console         []consoleEntry
	consoleReported int
	requests        []*request
	requestsByID    map[string]*request
	docStatus       int             // HTTP status of the main document
	inflight        map[string]bool // tracked requests started by the current action
	activity        time.Time       // last network or navigation event

	// Pointer and keyboard state of the commands' virtual input devices.
	mouseX, mouseY float64
	buttons        int
	modifiers      int

	cast screencastMeta
}

// frame is a node of a tab's frame tree.
type frame struct {
	id       string
	parentID string
	url      string
	loaderID string
	sess     *cdpSession // session of the process rendering the frame
	seq      int         // ref prefix ordinal; 0 for the main frame

	lifecycle  map[string]bool // lifecycle events seen, keyed by loader id + name
	loading    bool
	pendingNav bool // a navigation was requested and has not committed yet
	requested  time.Time
	committed  time.Time

	refSeq   int
	nodeRefs map[int]assignedRef
}

// frameRef is an immutable copy of the frame fields needed outside the lock.
type frameRef struct {
	id       string
	parentID string
	url      string
	loaderID string
	sess     *cdpSession
	seq      int
}

// dialog is an open JavaScript dialog; page scripts are blocked until it is
// handled.
type dialog struct {
	sess          *cdpSession
	kind          string
	message       string
	defaultPrompt string
}

// fileChooser is a native file picker the page asked for and we intercepted.
type fileChooser struct {
	sess      *cdpSession
	backendID int
	multiple  bool
}

func newTab(s *session, targetID, openerID string) *tab {
	return &tab{
		s: s, targetID: targetID, openerID: openerID,
		ready:        make(chan struct{}),
		frames:       map[string]*frame{},
		requestsByID: map[string]*request{},
		inflight:     map[string]bool{},
		refs:         map[string]nodeRef{},
	}
}

// frame returns the record for id, creating it if needed. Callers hold s.mu.
func (t *tab) frame(id string) *frame {
	f := t.frames[id]
	if f == nil {
		f = &frame{id: id, lifecycle: map[string]bool{}, nodeRefs: map[int]assignedRef{}}
		t.frames[id] = f
	}
	return f
}

// seedFrames records frames reported by Page.getFrameTree without clobbering
// anything newer already learned from events. Callers hold s.mu.
func (t *tab) seedFrames(tg *target, node frameTreeNode) {
	f := t.frame(node.Frame.ID)
	if f.loaderID == "" {
		t.setFrameInfo(f, tg, node.Frame)
	}
	for _, child := range node.ChildFrames {
		t.seedFrames(tg, child)
	}
}

func (t *tab) setFrameInfo(f *frame, tg *target, info frameInfo) {
	f.sess = tg.sess
	f.url = info.URL + info.URLFragment
	if info.LoaderID != f.loaderID {
		f.loaderID = info.LoaderID
		for key := range f.lifecycle {
			if !strings.HasPrefix(key, info.LoaderID) {
				delete(f.lifecycle, key)
			}
		}
	}
	t.adopt(f, tg, info.ParentID)
}

// adopt places f in the tree. A frame without a parent is the main frame
// when reported by the page session, and the root of an out-of-process
// iframe otherwise; the latter keeps the parent its embedder reported.
func (t *tab) adopt(f *frame, tg *target, parentID string) {
	if parentID != "" {
		f.parentID = parentID
	} else if tg.page {
		t.mainFrame = f.id
	}
	if f.parentID != "" && f.seq == 0 {
		t.frameSeq++
		f.seq = t.frameSeq
	}
}

func (t *tab) removeFrame(id string) {
	delete(t.frames, id)
	for childID, child := range t.frames {
		if child.parentID == id {
			t.removeFrame(childID)
		}
	}
}

// main returns the main frame record, or nil before the first navigation.
// Callers hold s.mu.
func (t *tab) main() *frame { return t.frames[t.mainFrame] }

// frameRefs returns the frames in parent-before-child order.
func (t *tab) frameRefs() []frameRef {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	var out []frameRef
	var walk func(id string)
	walk = func(id string) {
		f := t.frames[id]
		if f == nil || f.sess == nil {
			return
		}
		out = append(out, f.ref())
		for _, childID := range sortedKeys(t.frames) {
			if t.frames[childID].parentID == id {
				walk(childID)
			}
		}
	}
	walk(t.mainFrame)
	return out
}

func (f *frame) ref() frameRef {
	return frameRef{id: f.id, parentID: f.parentID, url: f.url, loaderID: f.loaderID, sess: f.sess, seq: f.seq}
}

// frameRef returns a copy of the frame with the given id.
func (t *tab) frameRef(id string) (frameRef, bool) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	f := t.frames[id]
	if f == nil || f.sess == nil {
		return frameRef{}, false
	}
	return f.ref(), true
}

// url returns the main frame's URL.
func (t *tab) url() string {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if f := t.main(); f != nil {
		return f.url
	}
	return ""
}

// urlAndTitle returns the tab's URL and title. The page itself is asked for
// the title; when its scripts are blocked by a dialog or do not answer, the
// browser process tells what it knows, which can lag behind the page.
func (t *tab) urlAndTitle(ctx context.Context) (url, title string) {
	if url = t.url(); url != "" && t.pendingDialog() == nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var res evalResult
		err := t.sess.call(ctx, "Runtime.evaluate", obj{"expression": "document.title", "returnByValue": true}, &res)
		if err == nil && json.Unmarshal(res.Result.Value, &title) == nil {
			return url, title
		}
	}
	var info struct {
		TargetInfo struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"targetInfo"`
	}
	if err := t.s.root.call(ctx, "Target.getTargetInfo", obj{"targetId": t.targetID}, &info); err != nil {
		return url, ""
	}
	url, title = info.TargetInfo.URL, info.TargetInfo.Title
	// Before a page sets a title the browser reports its URL instead.
	if title == url || "http://"+title == url || "https://"+title == url {
		title = ""
	}
	return url, title
}

// pendingDialog returns the open JavaScript dialog, if any.
func (t *tab) pendingDialog() *dialog {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.dialog
}

// onTargetEvent handles protocol events of a page or iframe session. It runs
// on the connection's dispatch goroutine and must not block.
func (s *session) onTargetEvent(tg *target, method string, params json.RawMessage) {
	switch method {
	case "Target.attachedToTarget":
		s.onAttached(tg, params)
		return
	case "Target.detachedFromTarget":
		s.onDetached(params)
		return
	case "Page.screencastFrame":
		s.onScreencastFrame(tg, params)
		return
	case "Fetch.requestPaused":
		s.onRequestPaused(tg, params)
		return
	}
	domain, _, _ := strings.Cut(method, ".")
	s.mu.Lock()
	defer s.mu.Unlock()
	t := tg.tab
	switch domain {
	case "Page":
		t.onPageEvent(tg, method, params)
	case "Network":
		t.onNetworkEvent(method, params)
	case "Runtime", "Log":
		t.onConsoleEvent(method, params)
	case "Input":
		if method == "Input.dragIntercepted" {
			var ev struct {
				Data json.RawMessage `json:"data"`
			}
			json.Unmarshal(params, &ev)
			t.dragData = ev.Data
		}
	case "Inspector":
		if method == "Inspector.targetCrashed" {
			t.crashed = true
		}
	default:
		return
	}
	s.notify()
}

type pageEvent struct {
	FrameID       string    `json:"frameId"`
	ParentFrameID string    `json:"parentFrameId"`
	Frame         frameInfo `json:"frame"`
	URL           string    `json:"url"`
	Reason        string    `json:"reason"`
	Disposition   string    `json:"disposition"`
	LoaderID      string    `json:"loaderId"`
	Name          string    `json:"name"`
	// javascriptDialogOpening
	Message       string `json:"message"`
	Type          string `json:"type"`
	DefaultPrompt string `json:"defaultPrompt"`
	// frameStartedNavigating
	NavigationType string `json:"navigationType"`
	// fileChooserOpened
	Mode          string `json:"mode"`
	BackendNodeID int    `json:"backendNodeId"`
}

func (t *tab) onPageEvent(tg *target, method string, params json.RawMessage) {
	var ev pageEvent
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	now := time.Now()
	switch method {
	case "Page.frameAttached":
		f := t.frame(ev.FrameID)
		f.sess = tg.sess
		t.adopt(f, tg, ev.ParentFrameID)
	case "Page.frameNavigated":
		f := t.frame(ev.Frame.ID)
		t.setFrameInfo(f, tg, ev.Frame)
		f.pendingNav, f.committed, t.activity = false, now, now
		if ev.Type == "BackForwardCacheRestore" {
			// A restored document is complete and fires no lifecycle events.
			f.lifecycle[f.loaderID+"DOMContentLoaded"], f.lifecycle[f.loaderID+"load"] = true, true
		}
		if f.id == t.mainFrame {
			t.navSeq++
			t.chooser, t.crashed = nil, false
			t.console, t.consoleReported = nil, 0
		}
	case "Page.navigatedWithinDocument":
		f := t.frame(ev.FrameID)
		f.url = ev.URL
		if f.id == t.mainFrame {
			t.navSeq++
		}
	case "Page.frameDetached":
		// A "swap" means the frame moved to another process, not that it
		// went away.
		if ev.Reason != "swap" {
			t.removeFrame(ev.FrameID)
		}
	case "Page.frameStartedLoading":
		t.frame(ev.FrameID).loading = true
		t.activity = now
	case "Page.frameStoppedLoading":
		f := t.frame(ev.FrameID)
		f.loading, f.pendingNav = false, false
		t.activity = now
	case "Page.frameRequestedNavigation":
		if ev.Disposition == "currentTab" {
			f := t.frame(ev.FrameID)
			f.pendingNav, f.requested, t.activity = true, now, now
		}
	case "Page.frameStartedNavigating":
		if !strings.Contains(strings.ToLower(ev.NavigationType), "samedocument") {
			f := t.frame(ev.FrameID)
			f.pendingNav, f.requested, t.activity = true, now, now
		}
	case "Page.lifecycleEvent":
		t.frame(ev.FrameID).lifecycle[ev.LoaderID+ev.Name] = true
		t.activity = now
	case "Page.javascriptDialogOpening":
		if ev.Type == "beforeunload" {
			// Never let a "leave site?" prompt stall a navigation.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				tg.sess.call(ctx, "Page.handleJavaScriptDialog", obj{"accept": true}, nil)
			}()
			return
		}
		t.dialog = &dialog{sess: tg.sess, kind: ev.Type, message: ev.Message, defaultPrompt: ev.DefaultPrompt}
	case "Page.javascriptDialogClosed":
		t.dialog = nil
	case "Page.fileChooserOpened":
		t.chooser = &fileChooser{sess: tg.sess, backendID: ev.BackendNodeID, multiple: ev.Mode == "selectMultiple"}
	}
}

// loaded reports whether the frame's current document reached the named
// lifecycle event. Callers hold s.mu.
func (f *frame) loaded(event string) bool { return f.lifecycle[f.loaderID+event] }
