// Package update finds newer releases of Artus on GitHub and replaces the
// running program with one.
//
// A release is trusted because it comes from this project's GitHub Releases
// over TLS and matches the checksum published with it. Nothing is installed
// that fails either test, or that cannot print its own version.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "pschachhuber/artus"

// source replaces GitHub in tests: a build made with
// -ldflags "-X github.com/pschachhuber/artus/internal/update.source=http://127.0.0.1:9000"
// looks for releases there instead.
var source = ""

const (
	maxBinary = 200 << 20
	maxText   = 1 << 20
)

// Release is a published version.
type Release struct {
	Version   string    `json:"version"` // without the leading v
	URL       string    `json:"url"`     // its page, for reading and manual download
	Notes     string    `json:"notes"`   // release notes, Markdown
	Published time.Time `json:"published"`

	binary, sums string // download addresses of this platform's build and the checksums
}

// ErrNoRelease means nothing has been published yet.
var ErrNoRelease = errors.New("no release has been published yet")

// Updater checks for and installs releases.
type Updater struct {
	Current string // the running version; "dev" for a build that is not a release
	Exe     string // the program file to replace

	client *http.Client
	api    string                                        // base of the releases API
	local  bool                                          // releases come from a test server
	probe  func(ctx context.Context, path string) string // version a downloaded program reports
}

// New returns an updater for the running program.
func New(current string) *Updater {
	u := &Updater{Current: current, api: "https://api.github.com", probe: reportedVersion}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		u.Exe = exe
	}
	if source != "" {
		u.api, u.local = strings.TrimRight(source, "/"), true
	}
	u.client = &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return u.allow(req.URL)
		},
	}
	return u
}

// allow accepts only addresses a release can legitimately live at.
func (u *Updater) allow(addr *url.URL) error {
	host := strings.ToLower(addr.Hostname())
	switch {
	case u.local:
		if base, err := url.Parse(u.api); err == nil && addr.Host == base.Host {
			return nil
		}
	case addr.Scheme == "https" && (host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com")):
		return nil
	}
	return fmt.Errorf("refusing to fetch an update from %s", addr.Host)
}

// AssetName is the file name of this platform's build in a release.
func AssetName() string {
	name := "artus_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Blocked says why this program cannot replace itself, or "" if it can.
func (u *Updater) Blocked() string {
	if _, ok := parseVersion(u.Current); !ok {
		return "This is a development build, which does not update itself."
	}
	if u.Exe == "" {
		return "Artus cannot tell where its program file is."
	}
	probe, err := os.CreateTemp(filepath.Dir(u.Exe), ".artus-update-*")
	if err != nil {
		return fmt.Sprintf("Artus may not write to %s, so it cannot replace itself there. Download the new version by hand, or move Artus to a folder of your own.", filepath.Dir(u.Exe))
	}
	probe.Close()
	os.Remove(probe.Name())
	return ""
}

// Latest returns the newest published release.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	var raw struct {
		Tag       string    `json:"tag_name"`
		Page      string    `json:"html_url"`
		Body      string    `json:"body"`
		Published time.Time `json:"published_at"`
		Assets    []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	body, status, err := u.get(ctx, u.api+"/repos/"+Repo+"/releases/latest", maxText)
	switch {
	case err != nil:
		return Release{}, err
	case status == http.StatusNotFound:
		return Release{}, ErrNoRelease
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return Release{}, errors.New("GitHub is limiting requests from this address; try again later")
	case status != http.StatusOK:
		return Release{}, fmt.Errorf("GitHub answered %d", status)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Release{}, fmt.Errorf("unreadable answer from GitHub: %w", err)
	}
	rel := Release{Version: strings.TrimPrefix(raw.Tag, "v"), URL: raw.Page, Notes: strings.TrimSpace(raw.Body), Published: raw.Published}
	if _, ok := parseVersion(rel.Version); !ok {
		return Release{}, fmt.Errorf("the latest release has an unexpected version %q", raw.Tag)
	}
	if len(rel.Notes) > 20_000 {
		rel.Notes = rel.Notes[:20_000] + "\n…"
	}
	for _, a := range raw.Assets {
		switch a.Name {
		case AssetName():
			rel.binary = a.URL
		case "checksums.txt":
			rel.sums = a.URL
		}
	}
	return rel, nil
}

// Newer reports whether rel is newer than the running version. A development
// build is never considered older than a release.
func (u *Updater) Newer(rel Release) bool {
	current, ok := parseVersion(u.Current)
	latest, ok2 := parseVersion(rel.Version)
	return ok && ok2 && current.less(latest)
}

// Install downloads rel, verifies it and puts it in place of the program
// file. The running process is not affected; Restart switches to the new one.
func (u *Updater) Install(ctx context.Context, rel Release) error {
	if why := u.Blocked(); why != "" {
		return errors.New(why)
	}
	if rel.binary == "" || rel.sums == "" {
		return fmt.Errorf("release %s has no build for %s/%s", rel.Version, runtime.GOOS, runtime.GOARCH)
	}
	for _, addr := range []string{rel.binary, rel.sums} {
		parsed, err := url.Parse(addr)
		if err != nil {
			return err
		}
		if err := u.allow(parsed); err != nil {
			return err
		}
		if !u.local && !strings.HasPrefix(addr, "https://github.com/"+Repo+"/releases/download/") {
			return fmt.Errorf("refusing to fetch an update from outside the project's releases: %s", addr)
		}
	}
	sums, status, err := u.get(ctx, rel.sums, maxText)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("cannot fetch the checksums (%d): %v", status, err)
	}
	want := checksumFor(string(sums), AssetName())
	if want == "" {
		return fmt.Errorf("release %s publishes no checksum for %s", rel.Version, AssetName())
	}

	// Download next to the program, so that putting it in place is a rename
	// on the same file system.
	pattern := ".artus-update-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Exe), pattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	got, err := u.download(ctx, rel.binary, tmp)
	tmp.Close()
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if got != want {
		return fmt.Errorf("the download does not match its published checksum (got %s, want %s); nothing was installed", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if reported := u.probe(ctx, tmp.Name()); reported != rel.Version {
		return fmt.Errorf("the downloaded program does not run here (it reports version %q, expected %q); nothing was installed", reported, rel.Version)
	}

	// The running program file cannot be overwritten on Windows, but it can
	// be renamed everywhere. The old file stays until the next start.
	old := u.Exe + ".old"
	os.Remove(old)
	if err := os.Rename(u.Exe, old); err != nil {
		return fmt.Errorf("cannot move the current program aside: %w", err)
	}
	if err := os.Rename(tmp.Name(), u.Exe); err != nil {
		os.Rename(old, u.Exe)
		return fmt.Errorf("cannot put the new program in place: %w", err)
	}
	return nil
}

// Cleanup removes what an earlier update left behind.
func (u *Updater) Cleanup() {
	if u.Exe == "" {
		return
	}
	os.Remove(u.Exe + ".old")
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(u.Exe), ".artus-update-*")); len(left) > 0 {
		for _, f := range left {
			if st, err := os.Stat(f); err == nil && time.Since(st.ModTime()) > time.Hour {
				os.Remove(f)
			}
		}
	}
}

func (u *Updater) request(ctx context.Context, addr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artus-updater")
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	return u.client.Do(req)
}

func (u *Updater) get(ctx context.Context, addr string, limit int64) ([]byte, int, error) {
	resp, err := u.request(ctx, addr)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return body, resp.StatusCode, err
}

// download writes addr to w and returns the SHA-256 of what it wrote.
func (u *Updater) download(ctx context.Context, addr string, w io.Writer) (string, error) {
	resp, err := u.request(ctx, addr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, sum), io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return "", err
	}
	if n > maxBinary {
		return "", errors.New("the file is implausibly large")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// checksumFor finds name in a checksums file ("<sha256>  <name>" per line).
func checksumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && len(fields[0]) == 64 {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// reportedVersion runs `<path> version` and returns the version it prints.
func reportedVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// version is a release number: dotted integers and an optional pre-release tag.
type version struct {
	nums []int
	pre  string
}

func parseVersion(s string) (version, bool) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(s), "v"), "-")
	core, _, _ = strings.Cut(core, "+")
	var v version
	for _, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.nums = append(v.nums, n)
	}
	v.pre = pre
	return v, true
}

// less orders versions; a pre-release comes before the release it leads to.
func (a version) less(b version) bool {
	for i := 0; i < max(len(a.nums), len(b.nums)); i++ {
		var x, y int
		if i < len(a.nums) {
			x = a.nums[i]
		}
		if i < len(b.nums) {
			y = b.nums[i]
		}
		if x != y {
			return x < y
		}
	}
	switch {
	case a.pre == b.pre:
		return false
	case a.pre == "":
		return false
	case b.pre == "":
		return true
	}
	return a.pre < b.pre
}
