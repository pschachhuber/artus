package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// target is a protocol session belonging to a tab: the page itself or one of
// its out-of-process iframes.
type target struct {
	sess *cdpSession
	tab  *tab
	id   string // target id; for an iframe this is also its frame id
	page bool
}

type attachedEvent struct {
	SessionID  string `json:"sessionId"`
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		URL      string `json:"url"`
		OpenerID string `json:"openerId"`
	} `json:"targetInfo"`
}

func (s *session) onBrowserEvent(method string, params json.RawMessage) {
	switch method {
	case "Target.attachedToTarget":
		s.onAttached(nil, params)
	case "Target.detachedFromTarget":
		s.onDetached(params)
	case "Browser.downloadWillBegin", "Browser.downloadProgress":
		s.onDownload(method, params)
	}
}

// onAttached adopts a new page or out-of-process iframe. Every other target
// kind (workers, browser UI surfaces) is resumed and left alone.
func (s *session) onAttached(parent *target, params json.RawMessage) {
	var ev attachedEvent
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	info := ev.TargetInfo
	tg := &target{id: info.TargetID}
	switch {
	case parent == nil && info.Type == "page" && !strings.HasPrefix(info.URL, "devtools://"):
		tg.page = true
		tg.tab = newTab(s, info.TargetID, info.OpenerID)
	case parent != nil && info.Type == "iframe":
		tg.tab = parent.tab
	default:
		via := s.root
		if parent != nil {
			via = parent.sess
		}
		sess := s.conn.session(ev.SessionID, nil)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sess.call(ctx, "Runtime.runIfWaitingForDebugger", nil, nil)
			via.call(ctx, "Target.detachFromTarget", obj{"sessionId": ev.SessionID}, nil)
			s.conn.dropSession(ev.SessionID)
		}()
		return
	}
	tg.sess = s.conn.session(ev.SessionID, func(method string, params json.RawMessage) {
		s.onTargetEvent(tg, method, params)
	})
	s.mu.Lock()
	s.targets[ev.SessionID] = tg
	if t := tg.tab; tg.page {
		t.sess = tg.sess
		switch {
		case s.helpers > 0 && info.OpenerID == "":
			s.helpers--
			t.helper = true
			s.helperTabs = append(s.helperTabs, t)
		case s.current == nil:
			s.tabs, s.current = append(s.tabs, t), t
		default:
			s.tabs, s.opened = append(s.tabs, t), append(s.opened, t)
		}
	}
	s.notify()
	s.mu.Unlock()
	go s.initTarget(tg)
}

func (s *session) onDetached(params json.RawMessage) {
	var ev struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	s.mu.Lock()
	tg := s.targets[ev.SessionID]
	delete(s.targets, ev.SessionID)
	if tg != nil && tg.page {
		t := tg.tab
		t.closed = true
		s.tabs, s.opened, s.helperTabs = removeTab(s.tabs, t), removeTab(s.opened, t), removeTab(s.helperTabs, t)
		if s.current == t {
			s.current = nil
			for _, other := range s.tabs {
				if other.targetID == t.openerID {
					s.current = other
				}
			}
			if s.current == nil && len(s.tabs) > 0 {
				s.current = s.tabs[len(s.tabs)-1]
			}
		}
	}
	s.notify()
	s.mu.Unlock()
	s.conn.dropSession(ev.SessionID)
	if tg != nil && tg.page {
		go s.syncScreencast()
	}
}

type frameTreeNode struct {
	Frame       frameInfo       `json:"frame"`
	ChildFrames []frameTreeNode `json:"childFrames"`
}

type frameInfo struct {
	ID          string `json:"id"`
	ParentID    string `json:"parentId"`
	LoaderID    string `json:"loaderId"`
	URL         string `json:"url"`
	URLFragment string `json:"urlFragment"`
}

// initTarget configures a newly attached page or iframe session and then
// lets it run; targets start paused so that nothing loads before this. The
// commands are pipelined rather than awaited one by one: some are only
// answered once the target runs.
func (s *session) initTarget(tg *target) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, t := tg.sess, tg.tab
	var calls []*pendingCall
	send := func(method string, params any) {
		calls = append(calls, sess.send(method, params))
	}
	send("Page.enable", nil)
	frameTree := sess.send("Page.getFrameTree", nil)
	send("Page.setLifecycleEventsEnabled", obj{"enabled": true})
	send("Runtime.enable", nil)
	send("Network.enable", nil)
	send("Target.setAutoAttach", obj{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	for _, source := range s.initScripts {
		send("Page.addScriptToEvaluateOnNewDocument", obj{"source": source})
	}
	if s.filter.active() {
		send("Fetch.enable", obj{"patterns": []obj{{"urlPattern": "*"}}})
	}
	if tg.page {
		send("Log.enable", nil)
		send("Page.setInterceptFileChooserDialog", obj{"enabled": true})
		send("Emulation.setFocusEmulationEnabled", obj{"enabled": true})
		s.mu.Lock()
		vp, cfg := s.viewport, s.cfg
		s.mu.Unlock()
		if vp != nil {
			send("Emulation.setDeviceMetricsOverride", vp.metrics())
		}
		if cfg.Locale != "" {
			send("Emulation.setLocaleOverride", obj{"locale": cfg.Locale})
		}
		if cfg.Timezone != "" {
			send("Emulation.setTimezoneOverride", obj{"timezoneId": cfg.Timezone})
		}
	}
	send("Runtime.runIfWaitingForDebugger", nil)

	var tree struct {
		FrameTree frameTreeNode `json:"frameTree"`
	}
	if frameTree.wait(ctx, &tree) == nil {
		s.mu.Lock()
		t.seedFrames(tg, tree.FrameTree)
		s.notify()
		s.mu.Unlock()
	}
	for _, call := range calls {
		if err := call.wait(ctx, nil); err != nil && !isClosedErr(err) {
			s.m.logf("browser: %s: %v", call.method, err)
		}
	}
	if tg.page {
		close(t.ready)
	}
}

// metrics returns Emulation.setDeviceMetricsOverride parameters.
func (v viewport) metrics() obj {
	return obj{"width": v.Width, "height": v.Height, "deviceScaleFactor": 0, "mobile": false}
}

// note is an event to mention in the next reply; path, when set, is a file
// the note is about.
type note struct {
	text string
	path string
}

// download tracks a file the browser is saving.
type download struct {
	name string
	url  string
	done bool
}

func (s *session) onDownload(method string, params json.RawMessage) {
	var ev struct {
		GUID              string `json:"guid"`
		URL               string `json:"url"`
		SuggestedFilename string `json:"suggestedFilename"`
		State             string `json:"state"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.notify()
	s.downloadActivity = time.Now()
	if method == "Browser.downloadWillBegin" {
		s.downloads[ev.GUID] = &download{name: ev.SuggestedFilename, url: ev.URL}
		return
	}
	d := s.downloads[ev.GUID]
	if d == nil || d.done || ev.State == "inProgress" {
		return
	}
	d.done = true
	if ev.State != "completed" {
		s.notes = append(s.notes, note{text: fmt.Sprintf("Download of %s was cancelled", d.name)})
		return
	}
	dir := s.cfg.downloadsDir(s.m.opts.Dir)
	path := uniquePath(filepath.Join(dir, sanitizeFilename(d.name)))
	if err := os.Rename(filepath.Join(dir, ev.GUID), path); err != nil {
		s.notes = append(s.notes, note{text: fmt.Sprintf("Download of %s finished but could not be moved into place: %v", d.name, err)})
		return
	}
	s.notes = append(s.notes, note{text: "Downloaded file " + d.name + " to", path: path})
	s.files = append(s.files, path)
	go s.m.emit(Event{Type: "download", Session: s.name, URL: d.url, Path: path})
}

// downloading reports the names of downloads still in progress. Callers hold
// s.mu.
func (s *session) downloading() []string {
	var names []string
	for _, guid := range sortedKeys(s.downloads) {
		if d := s.downloads[guid]; !d.done {
			names = append(names, d.name)
		}
	}
	return names
}

// sanitizeFilename keeps a server-suggested file name inside its directory.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "download"
	}
	return name
}

// uniquePath appends a counter to path until it names no existing file.
func uniquePath(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		if _, err := os.Lstat(path); err != nil {
			return path
		}
		path = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
}
