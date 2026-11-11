package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// site is a stand-in for GitHub Releases.
type site struct {
	*httptest.Server
	version string // "" = nothing published
	binary  []byte
	sum     string // checksum to publish; defaults to the real one
	binURL  string // where the release says the build is; defaults to this server
	hits    atomic.Int32
}

func newSite(t *testing.T, version string) *site {
	s := &site{version: version, binary: []byte("#!new program " + version)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.version == "" {
			http.NotFound(w, r)
			return
		}
		binURL := s.binURL
		if binURL == "" {
			binURL = s.URL + "/dl/" + AssetName()
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + s.version, "html_url": s.URL + "/release", "body": "## What changed\n- Things.",
			"published_at": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			"assets": []map[string]string{
				{"name": "artus_plan9_mips", "browser_download_url": s.URL + "/dl/other"},
				{"name": AssetName(), "browser_download_url": binURL},
				{"name": "checksums.txt", "browser_download_url": s.URL + "/dl/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("GET /dl/"+AssetName(), func(w http.ResponseWriter, r *http.Request) { w.Write(s.binary) })
	mux.HandleFunc("GET /dl/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		sum := s.sum
		if sum == "" {
			h := sha256.Sum256(s.binary)
			sum = hex.EncodeToString(h[:])
		}
		w.Write([]byte(strings.Repeat("0", 64) + "  artus_plan9_mips\n" + sum + "  " + AssetName() + "\n"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// updater returns an updater for a pretend installation of the given version.
func (s *site) updater(t *testing.T, current string) *Updater {
	exe := filepath.Join(t.TempDir(), "artus")
	if err := os.WriteFile(exe, []byte("old program"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := New(current)
	u.Exe, u.api, u.local = exe, s.URL, true
	u.probe = func(_ context.Context, path string) string {
		b, _ := os.ReadFile(path)
		return strings.TrimPrefix(string(b), "#!new program ")
	}
	return u
}

func content(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVersionOrder(t *testing.T) {
	order := []string{"0.1.0-rc1", "0.1.0", "v0.1.1", "0.2", "0.10.0", "1.0.0-beta", "1.0.0"}
	for i := range order {
		for j := range order {
			a, okA := parseVersion(order[i])
			b, okB := parseVersion(order[j])
			if !okA || !okB {
				t.Fatalf("cannot parse %q or %q", order[i], order[j])
			}
			if got := a.less(b); got != (i < j) {
				t.Errorf("%s < %s = %v", order[i], order[j], got)
			}
		}
	}
	for _, bad := range []string{"dev", "", "1.x", "latest"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q should not parse as a version", bad)
		}
	}
}

func TestCheckAndInstall(t *testing.T) {
	s := newSite(t, "0.2.0")
	u := s.updater(t, "0.1.0")
	rel, err := u.Latest(t.Context())
	if err != nil || rel.Version != "0.2.0" || !strings.Contains(rel.Notes, "What changed") || !u.Newer(rel) {
		t.Fatalf("latest = %+v, %v", rel, err)
	}
	if err := u.Install(t.Context(), rel); err != nil {
		t.Fatal(err)
	}
	if got := content(t, u.Exe); got != "#!new program 0.2.0" {
		t.Errorf("program file = %q", got)
	}
	if got := content(t, u.Exe+".old"); got != "old program" {
		t.Errorf("the previous program should be kept until the next start: %q", got)
	}
	if st, _ := os.Stat(u.Exe); st.Mode().Perm()&0o100 == 0 && filepath.Separator == '/' {
		t.Errorf("the new program is not executable: %v", st.Mode())
	}
	u.Cleanup()
	left, _ := os.ReadDir(filepath.Dir(u.Exe))
	if len(left) != 1 {
		t.Errorf("leftovers after cleanup: %v", left)
	}

	// The same version, or an older one, is not an update.
	for _, current := range []string{"0.2.0", "0.3.0", "dev"} {
		if s.updater(t, current).Newer(rel) {
			t.Errorf("%s should not be offered 0.2.0", current)
		}
	}
	if why := s.updater(t, "dev").Blocked(); !strings.Contains(why, "development build") {
		t.Errorf("a development build must not replace itself: %q", why)
	}
}

func TestNothingIsInstalledUnlessItChecksOut(t *testing.T) {
	cases := map[string]func(s *site, u *Updater){
		"does not match its published checksum": func(s *site, u *Updater) { s.sum = strings.Repeat("ab", 32) },
		"does not run here":                     func(s *site, u *Updater) { u.probe = func(context.Context, string) string { return "" } },
		"reports version":                       func(s *site, u *Updater) { s.binary = []byte("#!new program 9.9.9") },
		"refusing to fetch an update from":      func(s *site, u *Updater) { s.binURL = "https://evil.example/artus" },
	}
	for want, breakIt := range cases {
		s := newSite(t, "0.2.0")
		u := s.updater(t, "0.1.0")
		breakIt(s, u)
		rel, err := u.Latest(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		err = u.Install(t.Context(), rel)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
		if got := content(t, u.Exe); got != "old program" {
			t.Errorf("%s: the program was replaced anyway: %q", want, got)
		}
		if left, _ := os.ReadDir(filepath.Dir(u.Exe)); len(left) != 1 {
			t.Errorf("%s: a partial download was left behind: %v", want, left)
		}
	}
}

func TestRedirectsAndHosts(t *testing.T) {
	real := New("0.1.0")
	for addr, ok := range map[string]bool{
		"https://api.github.com/repos/x/y/releases/latest": true,
		"https://github.com/pschachhuber/artus/releases/x": true,
		"https://release-assets.githubusercontent.com/abc": true,
		"http://github.com/pschachhuber/artus/releases/x":  false,
		"https://github.com.evil.example/x":                false,
		"https://evilgithubusercontent.com/x":              false,
		"https://example.com/artus":                        false,
		"file:///etc/passwd":                               false,
	} {
		req, _ := http.NewRequest("GET", addr, nil)
		if err := real.allow(req.URL); (err == nil) != ok {
			t.Errorf("allow(%s) = %v, want allowed=%v", addr, err, ok)
		}
	}
	// A release asset outside the project's releases is refused even on GitHub.
	err := real.Install(t.Context(), Release{Version: "0.2.0", binary: "https://github.com/someone/else/releases/download/v1/artus", sums: "https://github.com/someone/else/releases/download/v1/checksums.txt"})
	if real.Blocked() == "" && (err == nil || !strings.Contains(err.Error(), "outside the project's releases")) {
		t.Errorf("foreign release: %v", err)
	}
}

func TestNoReleaseYet(t *testing.T) {
	s := newSite(t, "")
	if _, err := s.updater(t, "0.1.0").Latest(t.Context()); !errors.Is(err, ErrNoRelease) {
		t.Errorf("want ErrNoRelease, got %v", err)
	}
}

func service(s *site, u *Updater, mode string) (*Service, *atomic.Int32, *atomic.Bool) {
	var restarts atomic.Int32
	var idle atomic.Bool
	idle.Store(true)
	var mu sync.Mutex
	svc := &Service{
		Updater: u,
		Mode:    func() string { mu.Lock(); defer mu.Unlock(); return mode },
		Idle:    idle.Load,
		Restart: func() { restarts.Add(1) },
	}
	return svc, &restarts, &idle
}

func TestServiceStatusAndManualInstall(t *testing.T) {
	s := newSite(t, "0.2.0")
	svc, restarts, _ := service(s, s.updater(t, "0.1.0"), Notify)
	var seen []Status
	svc.Changed = func(st Status) { seen = append(seen, st) }
	if st := svc.Status(); st.Available || st.Checked != nil || st.Current != "0.1.0" || st.Mode != Notify {
		t.Fatalf("before any check: %+v", st)
	}
	if err := svc.Install(t.Context()); err == nil {
		t.Error("nothing is known yet, so nothing can be installed")
	}
	st := svc.Check(t.Context())
	if !st.Available || st.Latest == nil || st.Latest.Version != "0.2.0" || st.Checked == nil || st.Error != "" || st.Blocked != "" {
		t.Fatalf("after check: %+v", st)
	}
	if err := svc.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	if restarts.Load() != 1 {
		t.Errorf("restarts = %d", restarts.Load())
	}
	var sawInstalling bool
	for _, st := range seen {
		sawInstalling = sawInstalling || st.Installing
	}
	if !sawInstalling || !svc.Status().Installing {
		t.Errorf("the interface should see the install start, and no end before the restart: %+v", seen)
	}

	// A failed check is reported, not fatal.
	s.Close()
	if st := svc.Check(t.Context()); !strings.Contains(st.Error, "Could not check for updates") {
		t.Errorf("unreachable GitHub: %+v", st)
	}
}

func TestAutomaticModeWaitsUntilIdle(t *testing.T) {
	defer func(a, b, c time.Duration) { firstCheck, wakeEvery, retryIdle = a, b, c }(firstCheck, wakeEvery, retryIdle)
	firstCheck, wakeEvery, retryIdle = 5*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond

	run := func(mode string, idle bool, d time.Duration, breakIt func(*site)) (restarts int32, checks int32, st Status) {
		s := newSite(t, "0.2.0")
		if breakIt != nil {
			breakIt(s)
		}
		svc, n, quiet := service(s, s.updater(t, "0.1.0"), mode)
		quiet.Store(idle)
		ctx, cancel := context.WithTimeout(t.Context(), d)
		defer cancel()
		svc.Run(ctx)
		return n.Load(), s.hits.Load(), svc.Status()
	}
	if n, checks, _ := run(Off, true, 150*time.Millisecond, nil); n != 0 || checks != 0 {
		t.Errorf("off: %d restarts, %d requests to GitHub", n, checks)
	}
	if n, checks, st := run(Notify, true, 150*time.Millisecond, nil); n != 0 || checks != 1 || !st.Available {
		t.Errorf("notify: %d restarts, %d checks, %+v", n, checks, st)
	}
	if n, checks, _ := run(Auto, false, 200*time.Millisecond, nil); n != 0 || checks != 1 {
		t.Errorf("auto while busy: %d restarts, %d checks", n, checks)
	}
	if n, _, _ := run(Auto, true, 300*time.Millisecond, nil); n != 1 {
		t.Errorf("auto while idle: %d restarts", n)
	}
	// A release that cannot be installed is tried once, not in a loop.
	n, checks, st := run(Auto, true, 300*time.Millisecond, func(s *site) { s.sum = strings.Repeat("cd", 32) })
	if n != 0 || checks != 1 || !strings.Contains(st.Error, "not installed") {
		t.Errorf("auto with a bad release: %d restarts, %d checks, %+v", n, checks, st)
	}
}
