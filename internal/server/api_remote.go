package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pschachhuber/artus/internal/store"
)

// getRemote tells the remote access page how Artus is reached from other
// devices, and what Tailscale on this machine says about it right now.
func (s *Server) getRemote(w http.ResponseWriter, r *http.Request) {
	cfg := s.Home.Config().Remote
	out := map[string]any{"url": cfg.URL, "users": list(cfg.Users), "access": accessOf(r), "installed": false}
	if cli, ok := s.Tailscale(); ok {
		out["installed"] = true
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		st, err := cli.Status(ctx)
		if err != nil {
			out["error"] = err.Error()
		} else {
			out["tailscale"] = st
			if p, ok, err := cli.Published(ctx, s.Engine.ServerURL); err == nil && ok {
				out["published"] = p
			}
		}
	}
	reply(w, out)
}

// putRemote records where Tailscale serves Artus and which Tailscale accounts
// are signed in there. `artus setup` calls it on the server. It takes the
// access token, which lives on the server: someone who came in through
// Tailscale cannot let anyone else in.
func (s *Server) putRemote(w http.ResponseWriter, r *http.Request) {
	if accessOf(r).Via != "link" {
		refuse(w, http.StatusForbidden, errors.New("who may use Artus from other devices is decided on the server: run `artus setup` there"))
		return
	}
	var in store.Remote
	if !body(w, r, &in) {
		return
	}
	remote, err := in.Clean()
	if err != nil {
		bad(w, "%v", err)
		return
	}
	if err := s.Home.UpdateConfig(func(c *store.Config) { c.Remote = remote }); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.Publish("config", nil)
	reply(w, remote)
}
