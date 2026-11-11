package browser

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// originFilter enforces the network.allowedOrigins and blockedOrigins
// configuration. An origin on both lists is blocked.
type originFilter struct {
	allowed []originPattern
	blocked []originPattern
}

// originPattern matches request origins. Accepted spellings:
//
//	https://example.com:8443   exactly that origin
//	http://localhost:*         any port
//	example.com                any scheme and port
//	*.example.com              example.com and its subdomains
type originPattern struct {
	scheme string // empty matches any
	host   string
	sub    bool   // also match subdomains of host
	port   string // empty matches the scheme's default, "*" any
}

func parseOriginPattern(s string) originPattern {
	var p originPattern
	s = strings.TrimSuffix(strings.TrimSpace(s), "/")
	if scheme, rest, ok := strings.Cut(s, "://"); ok {
		p.scheme, s = strings.ToLower(scheme), rest
	} else {
		p.port = "*"
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.HasSuffix(s, "]") {
		s, p.port = s[:i], s[i+1:]
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s, p.sub = rest, true
	}
	p.host = strings.ToLower(s)
	return p
}

func (p originPattern) matches(u *url.URL) bool {
	if p.scheme != "" && p.scheme != u.Scheme {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != p.host && !(p.sub && strings.HasSuffix(host, "."+p.host)) && p.host != "*" {
		return false
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443"}[u.Scheme]
	}
	want := p.port
	if want == "" {
		want = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443"}[p.scheme]
	}
	return want == "*" || want == port
}

func newOriginFilter(cfg networkConfig) originFilter {
	var f originFilter
	for _, s := range cfg.AllowedOrigins {
		f.allowed = append(f.allowed, parseOriginPattern(s))
	}
	for _, s := range cfg.BlockedOrigins {
		f.blocked = append(f.blocked, parseOriginPattern(s))
	}
	return f
}

// active reports whether any rule is configured.
func (f originFilter) active() bool { return len(f.allowed)+len(f.blocked) > 0 }

// allows reports whether a request to rawURL may proceed. Only network
// schemes are filtered; about:, data: and blob: URLs always pass.
func (f originFilter) allows(rawURL string) bool {
	if !f.active() {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return true
	}
	for _, p := range f.blocked {
		if p.matches(u) {
			return false
		}
	}
	for _, p := range f.allowed {
		if p.matches(u) {
			return true
		}
	}
	return len(f.allowed) == 0
}

// onRequestPaused decides the fate of an intercepted request. Helper tabs
// get an empty document for everything, so they never reach the network.
func (s *session) onRequestPaused(tg *target, params json.RawMessage) {
	var ev struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	method, args := "Fetch.continueRequest", obj{"requestId": ev.RequestID}
	switch {
	case tg.tab.helper:
		method = "Fetch.fulfillRequest"
		args["responseCode"] = 200
		args["responseHeaders"] = []obj{{"name": "Content-Type", "value": "text/html"}}
		args["body"] = ""
	case !s.filter.allows(ev.Request.URL):
		method = "Fetch.failRequest"
		args["errorReason"] = "BlockedByClient"
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tg.sess.call(ctx, method, args, nil)
	}()
}
