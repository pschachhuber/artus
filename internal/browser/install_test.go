package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type zipEntry struct {
	name    string
	mode    fs.FileMode
	content string // file content, or the target of a symlink
}

func buildZip(t *testing.T, entries ...zipEntry) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(e.content))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "archive.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractZipKeepsModesAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs POSIX file modes and symlinks")
	}
	archive := buildZip(t,
		zipEntry{"app/", fs.ModeDir | 0o755, ""},
		zipEntry{"app/bin/tool", 0o755, "#!/bin/sh\necho hi\n"},
		zipEntry{"app/data.txt", 0o644, "data"},
		// The layout of a macOS framework bundle.
		zipEntry{"app/Framework/Versions/A/Lib", 0o755, "binary"},
		zipEntry{"app/Framework/Versions/Current", fs.ModeSymlink | 0o777, "A"},
		zipEntry{"app/Framework/Lib", fs.ModeSymlink | 0o777, "Versions/Current/Lib"},
		zipEntry{"app/Framework/Versions/Current/Extra", 0o644, "through a link"},
		zipEntry{"app/bin/up", fs.ModeSymlink | 0o777, "../data.txt"},
	)
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractZip(archive, dest); err != nil {
		t.Fatal(err)
	}
	mode := func(name string) fs.FileMode {
		info, err := os.Lstat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode()
	}
	if m := mode("app/bin/tool"); m.Perm()&0o111 == 0 {
		t.Errorf("executable bit lost: %v", m)
	}
	if m := mode("app/data.txt"); m.Perm()&0o111 != 0 {
		t.Errorf("plain file became executable: %v", m)
	}
	if m := mode("app/Framework/Versions/Current"); m&fs.ModeSymlink == 0 {
		t.Errorf("symlink was not preserved: %v", m)
	}
	for name, want := range map[string]string{
		"app/Framework/Lib": "binary", "app/Framework/Versions/A/Extra": "through a link", "app/bin/up": "data",
	} {
		if got, err := os.ReadFile(filepath.Join(dest, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if out, err := exec.Command(filepath.Join(dest, "app/bin/tool")).Output(); err != nil || string(out) != "hi\n" {
		t.Errorf("extracted tool does not run: %q, %v", out, err)
	}
}

func TestExtractZipRejectsEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	link := fs.ModeSymlink | 0o777
	cases := map[string][]zipEntry{
		"parent traversal":   {{"../evil.txt", 0o644, "x"}},
		"nested traversal":   {{"a/../../evil.txt", 0o644, "x"}},
		"absolute path":      {{"/tmp/evil.txt", 0o644, "x"}},
		"absolute link":      {{"link", link, "/etc"}},
		"link out of root":   {{"a/link", link, "../../outside"}},
		"link via dotdot":    {{"dir/x", 0o644, "x"}, {"link", link, "dir/../.."}},
		"write through link": {{"link", link, ".."}, {"link/evil.txt", 0o644, "x"}},
		// d resolves to the root, so a link "up two" from inside it would
		// leave the root although its path looks three levels deep.
		"link below a linked directory": {{"x/y/d", link, "../.."}, {"x/y/d/l", link, "../.."}, {"x/y/d/l/evil.txt", 0o644, "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "sub", "out")
			err := extractZip(buildZip(t, entries...), dest)
			if !errors.Is(err, errUnsafePath) {
				t.Errorf("extractZip = %v, want errUnsafePath", err)
			}
			filepath.WalkDir(parent, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !strings.HasPrefix(path, dest) && d.Name() == "evil.txt" {
					t.Errorf("wrote outside the destination: %s", path)
				}
				return nil
			})
		})
	}
}

// fakeChromeForTesting serves a download index and an archive whose
// "browser" is a script, as the real service would for this platform.
func fakeChromeForTesting(t *testing.T, version, script string) *int {
	t.Helper()
	platform, err := cftPlatform()
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("no scripted fake browser for this platform")
	}
	archive, err := os.ReadFile(buildZip(t, zipEntry{cftExecutable(platform), 0o755, script}))
	if err != nil {
		t.Fatal(err)
	}
	downloads := new(int)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chrome.zip" {
			*downloads++
			w.Write(archive)
			return
		}
		json.NewEncoder(w).Encode(obj{"channels": obj{"Stable": obj{"version": version, "downloads": obj{"chrome": []obj{
			{"platform": "some-other-os", "url": srv.URL + "/wrong.zip"},
			{"platform": platform, "url": srv.URL + "/chrome.zip"},
		}}}}})
	}))
	previous := cftIndexURL
	cftIndexURL = srv.URL + "/index.json"
	t.Cleanup(func() {
		cftIndexURL = previous
		srv.Close()
	})
	return downloads
}

func TestInstallCommand(t *testing.T) {
	downloads := fakeChromeForTesting(t, "999.0.1.2", "#!/bin/sh\necho Fake Chrome 999.0.1.2\n")
	dir := t.TempDir()
	// Ignore browsers installed on the machine running the tests.
	cfg := defaultConfig()
	cfg.Browser = "chrome-for-testing"
	if err := writeConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Options{Dir: dir})
	run := func(args ...string) string {
		t.Helper()
		res, err := m.Run(context.Background(), dir, args)
		if err != nil {
			t.Fatal(err)
		}
		return res.Text
	}
	mustContain(t, run("doctor"), "Browser: none", "Chrome for Testing is not downloaded yet", "`install` does it now")
	exe := filepath.Join(dir, "chrome", "999.0.1.2", filepath.FromSlash(cftExecutable(must(cftPlatform()))))
	mustContain(t, run("install"), "Installed Chrome for Testing 999.0.1.2 at "+exe)
	if info, err := os.Stat(exe); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed browser is not executable: %v", err)
	}
	mustContain(t, run("install"), "999.0.1.2 is already installed")
	if *downloads != 1 {
		t.Errorf("an up-to-date install downloaded again (%d downloads)", *downloads)
	}
	mustContain(t, run("install", "--force"), "Installed Chrome for Testing 999.0.1.2")
	mustContain(t, run("doctor"), "Browser: "+exe+" (chrome-for-testing, downloaded Chrome for Testing 999.0.1.2)", "Version: Fake Chrome 999.0.1.2")

	// A newer stable build replaces the old one.
	fakeChromeForTesting(t, "1000.0.0.0", "#!/bin/sh\necho Fake Chrome 1000\n")
	mustContain(t, run("install"), "Installed Chrome for Testing 1000.0.0.0")
	if _, err := os.Stat(filepath.Join(dir, "chrome", "999.0.1.2")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the previous build was not removed: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "chrome", "download-*")); len(entries) != 0 {
		t.Errorf("download leftovers: %v", entries)
	}
}

// TestBrowserIsDownloadedOnDemand covers a machine without a browser: the
// first command fetches Chrome for Testing and starts it. The fake download
// is a script that execs the real test browser.
func TestBrowserIsDownloadedOnDemand(t *testing.T) {
	real := testChrome(t)
	downloads := fakeChromeForTesting(t, "999.0.0.1", fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", real))
	e := &env{t: t, dir: t.TempDir(), cwd: t.TempDir(), site: newSite()}
	e.start(envOptions{config: func(cfg *config) { cfg.ExecutablePath, cfg.Browser = "", "chrome-for-testing" }})
	t.Cleanup(e.close)
	out := e.run("open", e.site.url("/"))
	mustContain(t, out, "- Page Title: Home")
	e.run("close")
	e.run("open")
	if *downloads != 1 {
		t.Errorf("the browser was downloaded %d times, want once", *downloads)
	}
	mustContain(t, e.run("doctor"), "downloaded Chrome for Testing 999.0.0.1", "System libraries: all present")
}

// TestRealChromeDownload downloads the real Chrome for Testing build. It
// needs network access and a few hundred megabytes, so it only runs when
// ARTUS_TEST_DOWNLOAD is set.
func TestRealChromeDownload(t *testing.T) {
	if os.Getenv("ARTUS_TEST_DOWNLOAD") == "" {
		t.Skip("set ARTUS_TEST_DOWNLOAD=1 to download Chrome for Testing from the network")
	}
	dir := t.TempDir()
	m := NewManager(Options{Dir: dir, Logf: t.Logf})
	res, err := m.Run(context.Background(), dir, []string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(res.Text)
	rec, err := readInstallRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	version := browserVersion(context.Background(), rec.executable(dir))
	if !strings.Contains(version, rec.Version) {
		t.Fatalf("the downloaded browser reports %q, want version %s", version, rec.Version)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
