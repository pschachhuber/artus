// Package server is the local web server: the embedded interface, the JSON
// API it talks to, the live event stream, and the small API that commands run
// by the agent use to call back in.
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/agent"
	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/store"
	"github.com/pschachhuber/artus/internal/update"
)

// Server wires the HTTP API to the rest of the harness.
type Server struct {
	Home    *store.Home
	Engine  *agent.Engine
	Secrets *secrets.Store
	LLM     *llm.Registry
	Sched   *sched.Scheduler
	Browser Browser
	UI      fs.FS
	Version string
	Updates *update.Service // nil when this process does not update itself

	// AgentToken authorises commands the agent runs (`artus browser`, `artus
	// secret add`). It is scoped to /api/agent/ and changes on every start, so
	// it cannot be used to change settings.
	AgentToken string

	hub *hub
}

// New returns a server. Its event hub is attached to the engine.
func New(s Server) *Server {
	s.hub = newHub()
	s.AgentToken = store.RandomID(24)
	s.Engine.Emit = s.hub.publish
	s.Engine.AgentToken = s.AgentToken
	return &s
}

// Publish sends an event to every connected interface.
func (s *Server) Publish(typ string, data any) {
	s.hub.publish(agent.Event{Type: typ, Data: data})
}

// Handler returns the HTTP handler with authentication applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.guard(mux)
}

const cookieName = "artus_token"

// guard rejects requests that do not come from the user's own browser or
// from a command the agent started:
//   - the Host header must name this machine, which defeats DNS rebinding;
//   - state-changing requests must not come from another origin;
//   - every API call needs the access token (cookie or bearer).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Home.Config().Server
		if !hostAllowed(r.Host, cfg.Host) {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")

		if token := r.URL.Query().Get("token"); token != "" && r.URL.Path == "/" {
			if !same(token, cfg.Token) {
				http.Error(w, "wrong token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		cookie, _ := r.Cookie(cookieName)
		user := same(bearer, cfg.Token) || (cookie != nil && same(cookie.Value, cfg.Token))
		switch {
		case user:
		case r.URL.Path == "/api/hello" && r.Method == http.MethodGet: // proves who this is without a token
		case strings.HasPrefix(r.URL.Path, "/api/agent/") && same(bearer, s.AgentToken):
		case strings.HasPrefix(r.URL.Path, "/api/"):
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Artus</title><body style="font:16px system-ui;max-width:32em;margin:15vh auto;padding:0 1em">`+
				`<h1>Artus</h1><p>This browser is not signed in. Open the link Artus printed when it started, or run <code>artus url</code> to print it again.</p>`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			// The interface loads nothing from anywhere else and runs no inline script,
			// so text from a model or a web page can never execute in it.
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func same(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hostAllowed accepts loopback names always, and anything when the user has
// deliberately bound the server to a non-loopback address.
func hostAllowed(hostport, bound string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || host == bound {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	if ip := net.ParseIP(bound); ip != nil && !ip.IsLoopback() {
		return true
	}
	return false
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("GET /", http.FileServerFS(s.UI))
	mux.HandleFunc("GET /api/events", s.hub.serve)
	mux.HandleFunc("GET /api/state", s.getState)
	mux.HandleFunc("GET /api/hello", s.hello)

	mux.HandleFunc("GET /api/chats", s.listChats)
	mux.HandleFunc("POST /api/chats", s.createChat)
	mux.HandleFunc("GET /api/chats/{id}", s.getChat)
	mux.HandleFunc("PATCH /api/chats/{id}", s.patchChat)
	mux.HandleFunc("DELETE /api/chats/{id}", s.deleteChat)
	mux.HandleFunc("POST /api/chats/{id}/messages", s.postMessage)
	mux.HandleFunc("POST /api/chats/{id}/stop", s.stopChat)
	mux.HandleFunc("POST /api/chats/{id}/compact", s.compactChat)
	mux.HandleFunc("POST /api/chats/{id}/fork", s.forkChat)
	mux.HandleFunc("GET /api/chats/{id}/context", s.chatContext)
	mux.HandleFunc("POST /api/chats/{id}/uploads", s.upload)
	mux.HandleFunc("DELETE /api/chats/{id}/uploads/{name}", s.removeUpload)
	mux.HandleFunc("POST /api/asks/{id}", s.answerAsk)
	mux.HandleFunc("GET /api/file", s.serveFile)

	mux.HandleFunc("GET /api/fs", s.fsList)
	mux.HandleFunc("GET /api/fs/file", s.fsRead)
	mux.HandleFunc("PUT /api/fs/file", s.fsWrite)
	mux.HandleFunc("DELETE /api/fs/file", s.fsDelete)

	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.patchTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.deleteTask)
	mux.HandleFunc("POST /api/tasks/{id}/run", s.runTask)
	mux.HandleFunc("DELETE /api/triggers/{id}", s.deleteTrigger)

	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("GET /api/models", s.listModels)
	mux.HandleFunc("GET /api/secrets", s.listSecrets)
	mux.HandleFunc("PUT /api/secrets/{name}", s.putSecret)
	mux.HandleFunc("DELETE /api/secrets/{name}", s.deleteSecret)
	mux.HandleFunc("GET /api/changes", s.listChanges)
	mux.HandleFunc("GET /api/changes/{hash}", s.getChange)
	mux.HandleFunc("POST /api/changes/{hash}/revert", s.revertChange)

	mux.HandleFunc("GET /api/update", s.updateStatus)
	mux.HandleFunc("POST /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/install", s.updateInstall)

	mux.HandleFunc("GET /api/browser", s.browserStatus)
	mux.HandleFunc("GET /api/browser/view", s.browserView)
	mux.HandleFunc("POST /api/browser/input", s.browserInput)
	mux.HandleFunc("POST /api/browser/handoff/{id}", s.browserHandoff)

	mux.HandleFunc("POST /api/agent/browser", s.agentBrowser)
	mux.HandleFunc("POST /api/agent/secret", s.agentSecret)
}

// reply writes v as JSON.
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// refuse writes an error as JSON with the given status.
func refuse(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func bad(w http.ResponseWriter, format string, args ...any) {
	refuse(w, http.StatusBadRequest, fmt.Errorf(format, args...))
}

// body decodes a JSON request body of at most 8 MB.
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		bad(w, "invalid JSON body: %v", err)
		return false
	}
	return true
}

// state is everything the interface needs to draw its frame.
func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	cfg := s.Home.Config()
	cfg.Server.Token = ""
	type provider struct {
		Name   string `json:"name"`
		Key    string `json:"key"` // name of the secret holding its API key
		HasKey bool   `json:"hasKey"`
	}
	var providers []provider
	for _, name := range s.LLM.Names() {
		key := strings.ToUpper(name) + "_API_KEY"
		providers = append(providers, provider{name, key, name == "mock" || s.Secrets.Has(key)})
	}
	reply(w, map[string]any{
		"version":   s.Version,
		"home":      s.Home.Dir,
		"config":    cfg,
		"providers": providers,
		"efforts":   llm.Efforts,
		"running":   list(s.Engine.Running()),
		"seq":       s.hub.current(),
		"time":      time.Now(),
	})
}

// list returns s, or an empty list for nil, so that JSON clients always get
// an array.
func list[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// hello answers a challenge with an HMAC of it under the access token. The
// command line uses it to tell this Artus from another program that took
// its port after a crash, without sending that program the token.
func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		bad(w, "nonce required")
		return
	}
	reply(w, map[string]string{"proof": Proof(s.Home.Config().Server.Token, nonce)})
}

// Proof is the answer to a hello challenge.
func Proof(token, nonce string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("artus-hello:" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}
