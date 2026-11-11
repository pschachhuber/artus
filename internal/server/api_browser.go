package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Browser is what the server needs from the browser driver.
type Browser interface {
	// Run executes one `artus browser …` command line.
	Run(ctx context.Context, cwd string, args []string) (text string, files []string, err error)
	// Status describes the open browser sessions.
	Status() any
	// ResolveHandoff tells a waiting hand-off that the user is done.
	ResolveHandoff(id string) bool
	// Screencast streams JPEG frames of a session until ctx ends.
	Screencast(ctx context.Context, session string, onFrame func(jpeg []byte)) error
	// Input forwards a mouse or keyboard event from the live view.
	Input(session string, event json.RawMessage) error
}

func (s *Server) browserStatus(w http.ResponseWriter, r *http.Request) {
	reply(w, map[string]any{"sessions": s.Browser.Status()})
}

// browserView streams the live view of a session as motion JPEG, which an
// <img> element renders directly.
func (s *Server) browserView(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	session := r.URL.Query().Get("session")
	if session == "" {
		session = "default"
	}
	const boundary = "artusframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	err := s.Browser.Screencast(r.Context(), session, func(jpeg []byte) {
		fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(jpeg))
		w.Write(jpeg)
		io.WriteString(w, "\r\n")
		flusher.Flush()
	})
	if err != nil && r.Context().Err() == nil {
		s.Publish("alert", "Live view: "+err.Error())
	}
}

func (s *Server) browserInput(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session string
		Event   json.RawMessage
	}
	if !body(w, r, &in) {
		return
	}
	if in.Session == "" {
		in.Session = "default"
	}
	if err := s.Browser.Input(in.Session, in.Event); err != nil {
		bad(w, "%v", err)
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) browserHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.Browser.ResolveHandoff(r.PathValue("id")) {
		refuse(w, http.StatusGone, errors.New("this hand-off is no longer waiting"))
		return
	}
	reply(w, map[string]bool{"ok": true})
}

// agentBrowser runs a browser command for the `artus browser` client.
func (s *Server) agentBrowser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Args []string
		Cwd  string
	}
	if !body(w, r, &in) {
		return
	}
	text, files, err := s.Browser.Run(r.Context(), in.Cwd, in.Args)
	if err != nil {
		reply(w, map[string]any{"text": strings.TrimSpace(text + "\n" + err.Error()), "files": files, "error": true})
		return
	}
	reply(w, map[string]any{"text": text, "files": files})
}

// agentSecret stores a new secret for the `artus secret add` client. It can
// add but never replace, and cannot touch the harness's own credentials.
func (s *Server) agentSecret(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Value string }
	if !body(w, r, &in) {
		return
	}
	if err := s.Secrets.Add(strings.TrimSpace(in.Name), strings.TrimSpace(in.Value)); err != nil {
		bad(w, "%v", err)
		return
	}
	s.Publish("secrets", nil)
	reply(w, map[string]bool{"ok": true})
}
