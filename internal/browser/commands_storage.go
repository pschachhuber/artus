package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// cookie uses the field names shared by the DevTools protocol and
// Playwright's storage state files.
type cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

func (ck cookie) String() string {
	attrs := []string{"domain: " + ck.Domain, "path: " + ck.Path}
	if ck.Expires > 0 {
		attrs = append(attrs, "expires: "+time.Unix(int64(ck.Expires), 0).UTC().Format(time.RFC3339))
	}
	if ck.HTTPOnly {
		attrs = append(attrs, "httpOnly")
	}
	if ck.Secure {
		attrs = append(attrs, "secure")
	}
	if ck.SameSite != "" {
		attrs = append(attrs, "sameSite: "+ck.SameSite)
	}
	return fmt.Sprintf("%s=%s (%s)", ck.Name, ck.Value, strings.Join(attrs, ", "))
}

// cookies returns all cookies of the browser, optionally limited to a domain
// and its subdomains.
func (s *session) cookies(ctx context.Context, domain string) ([]cookie, error) {
	var res struct {
		Cookies []cookie `json:"cookies"`
	}
	if err := s.root.call(ctx, "Storage.getCookies", nil, &res); err != nil {
		return nil, err
	}
	domain = strings.TrimPrefix(domain, ".")
	out := res.Cookies[:0]
	for _, ck := range res.Cookies {
		host := strings.TrimPrefix(ck.Domain, ".")
		if domain == "" || host == domain || strings.HasSuffix(host, "."+domain) {
			out = append(out, ck)
		}
	}
	return out, nil
}

func cmdCookieList(c *command) error {
	cookies, err := c.s.cookies(c.ctx, c.inv.flags["domain"])
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		c.out.say("No cookies.")
	}
	for _, ck := range cookies {
		c.out.say("%s", ck)
	}
	return nil
}

func cmdCookieClear(c *command) error {
	if err := c.s.root.call(c.ctx, "Storage.clearCookies", nil, nil); err != nil {
		return err
	}
	c.out.say("Cleared all cookies")
	return nil
}

// storageState is the content of a state file; the format is Playwright's
// storageState, so files can be exchanged with it.
type storageState struct {
	Cookies []cookie      `json:"cookies"`
	Origins []originState `json:"origins"`
}

type originState struct {
	Origin       string      `json:"origin"`
	LocalStorage []nameValue `json:"localStorage"`
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func cmdStateSave(c *command) error {
	cookies, err := c.s.cookies(c.ctx, "")
	if err != nil {
		return err
	}
	state := storageState{Cookies: cookies, Origins: []originState{}}
	for i := range state.Cookies {
		if state.Cookies[i].Expires <= 0 {
			state.Cookies[i].Expires = -1 // a session cookie, in Playwright's notation
		}
	}
	// localStorage can only be read from a document of its origin, so the
	// state covers the origins of the frames that are open right now.
	seen := map[string]bool{}
	tabs, _ := c.s.tabList()
	for _, t := range tabs {
		if t.pendingDialog() != nil {
			continue
		}
		for _, f := range t.frameRefs() {
			origin := urlOrigin(f.url)
			if origin == "" || seen[origin] {
				continue
			}
			seen[origin] = true
			var res struct {
				Items []nameValue `json:"items"`
			}
			if t.callFrame(c.ctx, f, "localStorage", nil, &res) == nil && len(res.Items) > 0 {
				state.Origins = append(state.Origins, originState{Origin: origin, LocalStorage: res.Items})
			}
		}
	}
	file, err := c.outputFile(c.inv.arg(0), "state", "json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	c.out.say("Saved %d cookie(s) and localStorage of %d origin(s) to %s", len(state.Cookies), len(state.Origins), c.show(file))
	c.out.files = append(c.out.files, file)
	return nil
}

func cmdStateLoad(c *command) error {
	file := c.resolvePath(c.inv.arg(0))
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var state storageState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("%s is not a state file: %w", file, err)
	}
	for i := range state.Cookies {
		if state.Cookies[i].Expires < 0 {
			state.Cookies[i].Expires = 0
		}
	}
	if len(state.Cookies) > 0 {
		if err := c.s.root.call(c.ctx, "Storage.setCookies", obj{"cookies": state.Cookies}, nil); err != nil {
			return err
		}
	}
	if err := c.s.seedLocalStorage(c.ctx, state.Origins); err != nil {
		return fmt.Errorf("restoring localStorage: %w", err)
	}
	c.out.say("Loaded %d cookie(s) and localStorage of %d origin(s) from %s", len(state.Cookies), len(state.Origins), c.show(file))
	return nil
}

// seedLocalStorage writes localStorage entries for origins that may not be
// open anywhere. It visits each origin in a hidden helper tab whose requests
// are answered locally with an empty page, so nothing touches the network.
func (s *session) seedLocalStorage(ctx context.Context, origins []originState) error {
	if len(origins) == 0 {
		return nil
	}
	t, err := s.openTab(ctx, true)
	if err != nil {
		return err
	}
	defer s.root.call(context.WithoutCancel(ctx), "Target.closeTarget", obj{"targetId": t.targetID}, nil)
	if err := t.sess.call(ctx, "Fetch.enable", obj{"patterns": []obj{{"urlPattern": "*"}}}, nil); err != nil {
		return err
	}
	for _, origin := range origins {
		if urlOrigin(origin.Origin) == "" {
			continue
		}
		if err := t.navigate(ctx, origin.Origin+"/", 15*time.Second); err != nil {
			return err
		}
		items, _ := json.Marshal(origin.LocalStorage)
		var res evalResult
		err := t.sess.call(ctx, "Runtime.evaluate", obj{
			"expression": "for (const item of " + string(items) + ") localStorage.setItem(item.name, item.value)",
		}, &res)
		if err != nil {
			return err
		}
		if res.ExceptionDetails != nil {
			return fmt.Errorf("%s: %s", origin.Origin, res.ExceptionDetails.message())
		}
	}
	return nil
}

// urlOrigin returns scheme://host[:port] for http(s) URLs and "" otherwise.
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
