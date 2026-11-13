// Package server is the local web server: the embedded interface, the JSON
// API it talks to, the live event stream, and the small API that commands run
// by the agent use to call back in.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
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
	"github.com/pschachhuber/artus/internal/tailscale"
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

	// Tailscale finds the Tailscale client on this machine, for the remote
	// access page. Tests replace it.
	Tailscale func() (*tailscale.CLI, bool)

	// AgentToken authorises commands the agent runs (`artus browser`, `artus
	// secret add`). It is scoped to /api/agent/ and changes on every start, so
	// it cannot be used to change settings.
	AgentToken string

	hub *hub
}

// New returns a server. Its event hub is attached to the engine.
func New(s Server) *Server {
	s.hub = newHub()
	if s.Tailscale == nil {
		s.Tailscale = tailscale.Find
	}
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

// access says how a request proved it comes from the user.
type access struct {
	Via  string `json:"via"`            // "link": the sign-in link's token; "tailscale": the Tailscale account
	User string `json:"user,omitempty"` // the Tailscale login
}

type accessKey struct{}

// accessOf returns how the request was signed in.
func accessOf(r *http.Request) access {
	a, _ := r.Context().Value(accessKey{}).(access)
	return a
}

// open are the files a phone fetches without credentials when the interface
// is added to its home screen. They are the same in every Artus.
var open = map[string]bool{"/manifest.json": true, "/icon.svg": true, "/icon-192.png": true, "/icon-512.png": true, "/icon-maskable-512.png": true, "/apple-touch-icon.png": true}

// guard rejects requests that do not come from the user's own browser or
// from a command the agent started:
//   - the Host header must name this machine or the address Tailscale serves
//     Artus under, which defeats DNS rebinding;
//   - requests another site makes a browser send are refused;
//   - every API call needs the access token (cookie or bearer), or arrives
//     through `tailscale serve` from a Tailscale account `artus setup` allowed.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Home.Config()
		remote := remoteHost(cfg.Remote.URL) != "" && canonHost(r.Host) == remoteHost(cfg.Remote.URL)
		if !remote && !hostAllowed(r.Host, cfg.Server.Host) {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		// Tailscale Funnel would put the agent on the public internet behind nothing but this check.
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			deny(w, http.StatusForbidden, "Artus is not offered on the public internet. Turn Funnel off for it (<code>tailscale funnel reset</code>) and open it from a device on your tailnet.")
			return
		}
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		// Browsers say where a request comes from. The interface only ever calls its own origin.
		if site := r.Header.Get("Sec-Fetch-Site"); api && site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")

		if token := r.URL.Query().Get("token"); token != "" && r.URL.Path == "/" {
			if !same(token, cfg.Server.Token) {
				http.Error(w, "wrong token", http.StatusUnauthorized)
				return
			}
			secure := loopbackPeer(r) && r.Header.Get("X-Forwarded-Proto") == "https"
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		cookie, _ := r.Cookie(cookieName)
		var who access
		login := ""
		if remote {
			login = tailscaleLogin(r)
		}
		// A browser on this machine that opens the Tailscale address is the owner's
		// device to Tailscale. It may be the browser the agent drives, which must
		// not be able to use Artus as the user.
		here := login != "" && fromThisMachine(r)
		switch {
		case same(bearer, cfg.Server.Token) || (cookie != nil && same(cookie.Value, cfg.Server.Token)):
			who = access{Via: "link"}
		case login != "" && !here && allowed(cfg.Remote.Users, login):
			// The account comes with every request, also with one another site makes
			// the browser send, so a change must carry this origin (checked above).
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") == "" {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			who = access{Via: "tailscale", User: login}
		case r.URL.Path == "/api/hello" && r.Method == http.MethodGet: // proves who this is without a token
		case api && strings.HasPrefix(r.URL.Path, "/api/agent/") && same(bearer, s.AgentToken):
		case r.Method == http.MethodGet && open[r.URL.Path]:
		case api:
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		case here:
			deny(w, http.StatusUnauthorized, "This is the machine Artus runs on. Your Tailscale account signs in your other devices; here, run <code>artus url</code> for a link that signs this browser in.")
			return
		case login != "":
			deny(w, http.StatusForbidden, "You are signed in to Tailscale as <b>"+html.EscapeString(login)+"</b>, and this Artus is set up for "+html.EscapeString(accounts(cfg.Remote.Users))+
				". To let this account in, run <code>artus setup --user "+html.EscapeString(login)+"</code> on the server.")
			return
		case remote:
			deny(w, http.StatusUnauthorized, "Tailscale did not say whose device this is: it is a shared or tagged device. Open Artus from a device signed in to Tailscale as "+
				html.EscapeString(accounts(cfg.Remote.Users))+", or run <code>artus url</code> on the server for a link that signs this browser in.")
			return
		default:
			deny(w, http.StatusUnauthorized, "This browser is not signed in. Open the link Artus printed when it started, or run <code>artus url</code> to print it again.")
			return
		}
		if !api {
			// The interface loads nothing from anywhere else and runs no inline script,
			// so text from a model or a web page can never execute in it.
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessKey{}, who)))
	})
}

// deny answers a browser that may not come in with a page that says why.
func deny(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light dark"><title>Artus</title>`+
		`<body style="font:16px/1.5 system-ui;max-width:32em;margin:15vh auto;padding:0 1.2em"><h1>Artus</h1><p>`+message+`</p>`)
}

func same(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// tailscaleLogin returns the Tailscale account a request arrives from.
// `tailscale serve` runs on this machine, removes these headers from what a
// client sends and sets them itself from the device the connection comes
// from. A request that reaches Artus from anywhere else carries none it
// trusts.
func tailscaleLogin(r *http.Request) string {
	if !loopbackPeer(r) {
		return ""
	}
	login := r.Header.Get("Tailscale-User-Login")
	if dec, err := new(mime.WordDecoder).DecodeHeader(login); err == nil { // names outside ASCII arrive encoded
		login = dec
	}
	return strings.TrimSpace(login)
}

// fromThisMachine reports whether the device Tailscale forwards a request
// for is this machine itself: `tailscale serve` names the device's address,
// and it is one of ours.
func fromThisMachine(r *http.Request) bool {
	device := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Forwarded-For")))
	if device == nil {
		return false
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(device) {
			return true
		}
	}
	return false
}

func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowed(users []string, login string) bool {
	for _, u := range users {
		if strings.EqualFold(strings.TrimSpace(u), login) {
			return true
		}
	}
	return false
}

func accounts(users []string) string {
	if len(users) == 0 {
		return "no Tailscale account yet"
	}
	return strings.Join(users, ", ")
}

// remoteHost returns the host of the address Tailscale serves Artus under,
// as canonHost spells it, or "" when there is none.
func remoteHost(address string) string {
	u, err := url.Parse(address)
	if address == "" || err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ""
	}
	return canonHost(u.Host)
}

// canonHost spells a host the way a browser sends it over HTTPS: lower case,
// without the default port.
func canonHost(hostport string) string {
	host := strings.ToLower(hostport)
	if h, port, err := net.SplitHostPort(host); err == nil && port == "443" {
		host = h
	}
	return strings.TrimSuffix(host, ".")
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
	mux.HandleFunc("GET /api/remote", s.getRemote)
	mux.HandleFunc("PUT /api/remote", s.putRemote)
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
		"access":    accessOf(r),
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
