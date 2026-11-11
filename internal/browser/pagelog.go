package browser

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	maxConsoleEntries = 500
	maxRequests       = 500
)

// consoleLevels orders console levels from most to least severe.
var consoleLevels = []string{"error", "warning", "info", "debug"}

type consoleEntry struct {
	level string
	text  string
	where string
}

func (e consoleEntry) String() string {
	line := "[" + strings.ToUpper(e.level) + "] " + e.text
	if e.where != "" {
		line += " @ " + e.where
	}
	return line
}

// request is one network request of a tab.
type request struct {
	method string
	url    string
	kind   string
	status int
	failed string
	done   bool
}

func (r *request) String() string {
	line := fmt.Sprintf("[%s] %s", r.method, r.url)
	switch {
	case r.failed != "":
		line += " => [FAILED] " + r.failed
	case r.status != 0:
		line += fmt.Sprintf(" => [%d]", r.status)
	case !r.done:
		line += " => [pending]"
	}
	return line
}

type remoteObject struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
	Preview     *struct {
		Overflow   bool `json:"overflow"`
		Properties []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"properties"`
	} `json:"preview"`
}

// text renders a console argument the way DevTools would print it.
func (o remoteObject) text() string {
	if o.Type == "string" {
		var s string
		if json.Unmarshal(o.Value, &s) == nil {
			return s
		}
	}
	if len(o.Value) > 0 {
		return string(o.Value)
	}
	if o.Preview == nil || o.Subtype == "error" {
		if o.Description != "" {
			return o.Description
		}
		return o.Type
	}
	parts := make([]string, 0, len(o.Preview.Properties)+1)
	for _, p := range o.Preview.Properties {
		value := p.Value
		if p.Type == "string" {
			value = strconv.Quote(value)
		}
		if o.Subtype != "array" {
			value = p.Name + ": " + value
		}
		parts = append(parts, value)
	}
	if o.Preview.Overflow {
		parts = append(parts, "…")
	}
	if o.Subtype == "array" {
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

type exceptionDetails struct {
	Text       string `json:"text"`
	URL        string `json:"url"`
	LineNumber int    `json:"lineNumber"`
	Exception  *struct {
		Description string          `json:"description"`
		Value       json.RawMessage `json:"value"`
	} `json:"exception"`
}

// message returns the first line of the exception, e.g. "TypeError: x is not
// a function".
func (d exceptionDetails) message() string {
	msg := d.Text
	if d.Exception != nil {
		switch {
		case d.Exception.Description != "":
			msg = d.Exception.Description
		case len(d.Exception.Value) > 0:
			msg = d.Text + " " + string(d.Exception.Value)
		}
	}
	if i := strings.Index(msg, "\n    at "); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// onConsoleEvent records console output and uncaught errors. Callers hold
// s.mu.
func (t *tab) onConsoleEvent(method string, params json.RawMessage) {
	var entry consoleEntry
	switch method {
	case "Runtime.consoleAPICalled":
		var ev struct {
			Type       string         `json:"type"`
			Args       []remoteObject `json:"args"`
			StackTrace struct {
				CallFrames []struct {
					URL        string `json:"url"`
					LineNumber int    `json:"lineNumber"`
				} `json:"callFrames"`
			} `json:"stackTrace"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		parts := make([]string, len(ev.Args))
		for i, arg := range ev.Args {
			parts[i] = arg.text()
		}
		entry.text = strings.Join(parts, " ")
		switch ev.Type {
		case "error", "assert":
			entry.level = "error"
		case "warning":
			entry.level = "warning"
		case "debug", "trace":
			entry.level = "debug"
		default:
			entry.level = "info"
		}
		if frames := ev.StackTrace.CallFrames; len(frames) > 0 && frames[0].URL != "" {
			entry.where = frames[0].URL + ":" + strconv.Itoa(frames[0].LineNumber+1)
		}
	case "Runtime.exceptionThrown":
		var ev struct {
			Details exceptionDetails `json:"exceptionDetails"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		entry.level, entry.text = "error", ev.Details.message()
		if ev.Details.URL != "" {
			entry.where = ev.Details.URL + ":" + strconv.Itoa(ev.Details.LineNumber+1)
		}
	case "Log.entryAdded":
		var ev struct {
			Entry struct {
				Level      string `json:"level"`
				Text       string `json:"text"`
				URL        string `json:"url"`
				LineNumber int    `json:"lineNumber"`
			} `json:"entry"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		entry.level, entry.text, entry.where = ev.Entry.Level, ev.Entry.Text, ev.Entry.URL
		if entry.level == "verbose" {
			entry.level = "debug"
		}
	default:
		return
	}
	t.console = append(t.console, entry)
	if drop := len(t.console) - maxConsoleEntries; drop > 0 {
		t.console = t.console[drop:]
		t.consoleReported = max(0, t.consoleReported-drop)
	}
}

// onNetworkEvent maintains the request log and the set of requests the
// current action is waiting on. Callers hold s.mu.
func (t *tab) onNetworkEvent(method string, params json.RawMessage) {
	var ev struct {
		RequestID string `json:"requestId"`
		LoaderID  string `json:"loaderId"`
		FrameID   string `json:"frameId"`
		Type      string `json:"type"`
		Request   struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
		Response struct {
			Status int `json:"status"`
		} `json:"response"`
		ErrorText string `json:"errorText"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	t.activity = time.Now()
	switch method {
	case "Network.requestWillBeSent":
		if ev.Type == "Document" && ev.FrameID == t.mainFrame && ev.RequestID == ev.LoaderID {
			// A new main document: start the log afresh.
			t.requests, t.requestsByID, t.docStatus = nil, map[string]*request{}, 0
		}
		r := t.requestsByID[ev.RequestID]
		if r == nil { // otherwise a redirect hop of a known request
			r = &request{}
			t.requests = append(t.requests, r)
			t.requestsByID[ev.RequestID] = r
			if drop := len(t.requests) - maxRequests; drop > 0 {
				t.requests = t.requests[drop:]
			}
		}
		r.method, r.url, r.kind = ev.Request.Method, ev.Request.URL, ev.Type
		switch ev.Type {
		case "Document", "Stylesheet", "Script", "XHR", "Fetch":
			t.inflight[ev.RequestID] = true
		}
	case "Network.responseReceived":
		if r := t.requestsByID[ev.RequestID]; r != nil {
			r.status = ev.Response.Status
		}
		if ev.Type == "Document" && ev.FrameID == t.mainFrame && ev.RequestID == ev.LoaderID {
			t.docStatus = ev.Response.Status
		}
	case "Network.loadingFinished", "Network.loadingFailed":
		if r := t.requestsByID[ev.RequestID]; r != nil {
			r.done, r.failed = true, ev.ErrorText
			delete(t.requestsByID, ev.RequestID)
		}
		delete(t.inflight, ev.RequestID)
	}
}

// consoleSince counts errors and warnings logged since the last response.
// Callers hold s.mu.
func (t *tab) consoleSince() (errors, warnings int) {
	for _, e := range t.console[min(t.consoleReported, len(t.console)):] {
		switch e.level {
		case "error":
			errors++
		case "warning":
			warnings++
		}
	}
	t.consoleReported = len(t.console)
	return errors, warnings
}
