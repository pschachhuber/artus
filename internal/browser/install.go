package browser

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cftIndexURL lists the current Chrome for Testing builds per channel.
var cftIndexURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// cftPlatform returns the Chrome for Testing name of this OS and CPU.
func cftPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux64", nil
	case "linux/arm64":
		return "linux-arm64", nil
	case "darwin/arm64":
		return "mac-arm64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "windows/amd64", "windows/arm64":
		return "win64", nil
	case "windows/386":
		return "win32", nil
	}
	return "", fmt.Errorf("Chrome for Testing is not built for %s/%s; install Chromium yourself and set executablePath", runtime.GOOS, runtime.GOARCH)
}

// cftExecutable returns the browser binary's path inside the extracted
// archive for a platform.
func cftExecutable(platform string) string {
	switch {
	case strings.HasPrefix(platform, "mac"):
		return "chrome-" + platform + "/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
	case strings.HasPrefix(platform, "win"):
		return "chrome-" + platform + "/chrome.exe"
	}
	return "chrome-" + platform + "/chrome"
}

// httpGetJSON fetches a JSON document. The default transport honours
// HTTPS_PROXY and friends.
func httpGetJSON(ctx context.Context, url string, v any) error {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// installChrome downloads the stable Chrome for Testing build into
// <dir>/chrome unless that version is already there. It reports whether
// anything was downloaded.
func (m *Manager) installChrome(ctx context.Context, force bool) (rec installRecord, fresh bool, err error) {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	platform, err := cftPlatform()
	if err != nil {
		return rec, false, err
	}
	var index struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := httpGetJSON(ctx, cftIndexURL, &index); err != nil {
		return rec, false, fmt.Errorf("fetching the Chrome for Testing index: %w", err)
	}
	stable := index.Channels["Stable"]
	url := ""
	for _, d := range stable.Downloads["chrome"] {
		if d.Platform == platform {
			url = d.URL
		}
	}
	if url == "" {
		return rec, false, fmt.Errorf("Chrome for Testing has no stable build for %s; install Chromium yourself and set executablePath", platform)
	}
	dir := chromeDir(m.opts.Dir)
	if current, err := readInstallRecord(m.opts.Dir); err == nil && current.Version == stable.Version && !force {
		return current, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return rec, false, err
	}
	m.logf("browser: downloading Chrome for Testing %s for %s", stable.Version, platform)
	archive, err := downloadFile(ctx, url, dir)
	if err != nil {
		return rec, false, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer os.Remove(archive)
	staging := filepath.Join(dir, stable.Version+".tmp")
	final := filepath.Join(dir, stable.Version)
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := extractZip(archive, staging); err != nil {
		return rec, false, fmt.Errorf("extracting %s: %w", filepath.Base(url), err)
	}
	rec = installRecord{Version: stable.Version, Platform: platform, Executable: stable.Version + "/" + cftExecutable(platform)}
	if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(cftExecutable(platform)))); err != nil {
		return rec, false, fmt.Errorf("the downloaded archive does not contain %s", cftExecutable(platform))
	}
	if err := os.RemoveAll(final); err != nil {
		return rec, false, err
	}
	if err := os.Rename(staging, final); err != nil {
		return rec, false, err
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "installed.json"), append(data, '\n'), 0o644); err != nil {
		return rec, false, err
	}
	// Older builds are of no further use.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && e.Name() != stable.Version {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	m.logf("browser: installed Chrome for Testing %s", stable.Version)
	return rec, true, nil
}

// downloadFile saves url to a temporary file in dir and returns its path.
func downloadFile(ctx context.Context, url, dir string) (string, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir, "download-*.zip")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && resp.ContentLength > 0 && n != resp.ContentLength {
		err = fmt.Errorf("incomplete download: got %d of %d bytes", n, resp.ContentLength)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extractZip unpacks an archive into dest, keeping executable bits and
// symbolic links (macOS application bundles need both). Entries that would
// land outside dest, directly or through a link, are rejected.
func extractZip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return err
	}
	for _, f := range r.File {
		if err := extractEntry(f, root); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}

var errUnsafePath = errors.New("unsafe path in archive")

func extractEntry(f *zip.File, root string) error {
	name := filepath.FromSlash(strings.TrimSuffix(f.Name, "/"))
	if name == "" || !filepath.IsLocal(name) {
		return errUnsafePath
	}
	target := filepath.Join(root, name)
	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// An earlier entry may have made a parent directory a link; resolve it
	// to see where the file would really go.
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	depth, ok := depthBelow(root, parent)
	if !ok {
		return errUnsafePath
	}
	target = filepath.Join(parent, filepath.Base(target))
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	if f.Mode()&fs.ModeSymlink != 0 {
		raw, err := io.ReadAll(io.LimitReader(src, 4096))
		if err != nil {
			return err
		}
		link := string(raw)
		if !safeLink(link, depth) {
			return fmt.Errorf("%w: link to %q", errUnsafePath, link)
		}
		return os.Symlink(filepath.FromSlash(link), target)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, f.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// depthBelow returns how many directories path lies below root.
func depthBelow(root, path string) (int, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return 0, false
	}
	if rel == "." {
		return 0, true
	}
	return strings.Count(rel, string(filepath.Separator)) + 1, true
}

// safeLink reports whether a link target stays inside the extraction root
// when the link is created depth directories below it. Only relative
// targets are accepted, and ".." only as leading elements that do not climb
// past the root; everything after them then descends through entries that
// were themselves checked.
func safeLink(link string, depth int) bool {
	if link == "" || strings.HasPrefix(link, "/") || strings.Contains(link, `\`) || filepath.VolumeName(link) != "" {
		return false
	}
	elems := strings.Split(link, "/")
	i := 0
	for i < len(elems) && elems[i] == ".." {
		i++
	}
	if i > depth {
		return false
	}
	for _, e := range elems[i:] {
		if e == ".." {
			return false
		}
	}
	return true
}
