package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/pschachhuber/artus/internal/update"
)

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		reply(w, update.Status{Current: s.Version, Blocked: "This Artus was started for a single command and does not update itself."})
		return
	}
	reply(w, s.Updates.Status())
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		refuse(w, http.StatusNotFound, errors.New("updates are not available in this mode"))
		return
	}
	reply(w, s.Updates.Check(r.Context()))
}

// updateInstall starts installing the latest release. It answers at once;
// progress, failure and the restart arrive as "update" events and a
// reconnect of the event stream.
func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		refuse(w, http.StatusNotFound, errors.New("updates are not available in this mode"))
		return
	}
	st := s.Updates.Status()
	switch {
	case st.Blocked != "":
		refuse(w, http.StatusConflict, errors.New(st.Blocked))
		return
	case !st.Available:
		refuse(w, http.StatusConflict, errors.New("there is no newer version to install"))
		return
	case st.Installing:
		refuse(w, http.StatusConflict, errors.New("an update is already being installed"))
		return
	}
	go s.Updates.Install(context.Background())
	reply(w, map[string]bool{"ok": true})
}
