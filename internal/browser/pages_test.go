package browser

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIframesSameAndCrossOrigin(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/frames"))
	mustContain(t, out, `- iframe "same" [ref=`, `- iframe "cross" [ref=`)
	// Refs inside frames carry a frame prefix; the two frames differ.
	sameButton := ref(t, out[strings.Index(out, `iframe "same"`):], `button "Inner button"`)
	crossButton := ref(t, out[strings.Index(out, `iframe "cross"`):], `button "Inner button"`)
	crossField := ref(t, out[strings.Index(out, `iframe "cross"`):], `textbox "Inner field"`)
	if !strings.HasPrefix(sameButton, "f") || !strings.HasPrefix(crossButton, "f") || sameButton[:2] == crossButton[:2] {
		t.Fatalf("unexpected frame refs %s and %s", sameButton, crossButton)
	}

	out = e.run("click", sameButton)
	same := out[strings.Index(out, `iframe "same"`):strings.Index(out, `iframe "cross"`)]
	mustContain(t, same, "inner clicked")
	mustNotContain(t, out[strings.Index(out, `iframe "cross"`):], "inner clicked")

	// The cross-origin frame lives in another process, with its own
	// coordinate space; a real click and real typing must still land.
	out = e.run("click", crossButton)
	mustContain(t, out[strings.Index(out, `iframe "cross"`):], "inner clicked")
	out = e.run("fill", crossField, "across origins")
	mustContain(t, out[strings.Index(out, `iframe "cross"`):], "]: across origins", "typed across origins")
	e.run("click", crossField)
	e.run("press", "End")
	out = e.run("type", "!")
	mustContain(t, out, "typed across origins!")

	out = e.run("wait", "--text", "typed across origins!")
	mustContain(t, out, `Text "typed across origins!" appeared`)
	mustContain(t, e.run("eval", "el => el.ownerDocument.location.host", crossButton), "localhost")
	shot := e.run("screenshot", crossButton, "--filename", "cross.png")
	mustContain(t, shot, `Screenshot of button "Inner button"`)
}

func TestShadowDOM(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/shadow"))
	out = e.run("click", ref(t, out, `button "Shadow button"`))
	mustContain(t, out, "shadow clicked")
	out = e.run("fill", ref(t, out, `textbox "Shadow field"`), "deep")
	mustContain(t, out, "shadow typed deep")
}

func TestDialogs(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/dialogs"))
	alert, confirm, prompt := ref(t, out, `button "Alert"`), ref(t, out, `button "Confirm"`), ref(t, out, `button "Prompt"`)

	out = e.run("click", alert)
	mustContain(t, out, "### Modal state", `["alert" dialog with message "Heads up"]`, "dialog-accept")
	mustNotContain(t, out, "### Snapshot")
	// Everything else is refused while the page is blocked.
	mustContain(t, e.fail("click", confirm), `a "alert" dialog is open`, "Heads up")
	mustContain(t, e.fail("snapshot"), "dialog-accept")
	out = e.run("dialog-accept")
	mustContain(t, out, `Accepted the "alert" dialog`, "alert done", "### Snapshot")

	e.run("click", confirm)
	mustContain(t, e.run("dialog-dismiss"), `Dismissed the "confirm" dialog`, "confirm false")
	e.run("click", confirm)
	mustContain(t, e.run("dialog-accept"), "confirm true")

	out = e.run("click", prompt)
	mustContain(t, out, `["prompt" dialog with message "Your name?"]`)
	mustContain(t, e.run("dialog-accept", "Ada"), "prompt Ada")
	e.run("click", prompt)
	mustContain(t, e.run("dialog-accept"), "prompt nobody") // the default text
	e.run("click", prompt)
	mustContain(t, e.run("dialog-dismiss"), "prompt null")
	mustContain(t, e.fail("dialog-accept"), "no dialog is open")

	// The page has a beforeunload handler; it must not stall navigation.
	mustContain(t, e.run("goto", e.site.url("/")), "- Page Title: Home")
}

func TestPopupsAndTabs(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/popup"))
	out = e.run("click", ref(t, out, `link "Open in new tab"`))
	// The opener stays current; the new tab is announced.
	mustContain(t, out, "### Open tabs", "- 0: (current) [Opener]", "- 1: [Reset]("+e.site.url("/reset")+")",
		"A new tab opened (tab 1)", "`tab-select 1`", "- Page Title: Opener")

	// The opener, now in the background, still takes real input.
	out = e.run("click", ref(t, out, `link "Open in new tab"`))
	mustContain(t, out, "A new tab opened (tab 2)")
	e.run("tab-close", "2")

	out = e.run("tab-select", "1")
	mustContain(t, out, "Switched to tab 1", "- 1: (current) [Reset]", `heading "Reset your password"`)
	out = e.run("click", ref(t, out, `link "Home"`))
	mustContain(t, out, "- Page Title: Home")

	out = e.run("tab-new", e.site.url("/form"))
	mustContain(t, out, "Opened a new tab", "- 2: (current) [Sign in]", `button "Submit"`)
	list := e.run("tab-list")
	mustContain(t, list, "- 0: [Opener]", "- 1: [Home]", "- 2: (current) [Sign in]")
	mustNotContain(t, list, "### Snapshot")
	if st := e.m.Status(); len(st) != 1 || st[0].Tabs != 3 || st[0].Title != "Sign in" || !st[0].Headless {
		t.Errorf("unexpected status %+v", st)
	}

	mustContain(t, e.fail("tab-select", "7"), `there is no tab "7"`)
	out = e.run("tab-close", "0")
	mustContain(t, out, "Closed the tab", "- 0: [Home]", "- 1: (current) [Sign in]")
	out = e.run("tab-close")
	mustContain(t, out, "- 0: (current) [Home]", "- Page Title: Home")
	mustNotContain(t, e.run("snapshot"), "### Open tabs") // a single tab needs no list
}

func TestUpload(t *testing.T) {
	e := shared(t)
	e.writeFile("report.txt", "quarterly numbers")
	e.writeFile("a.txt", "first")
	e.writeFile("b.txt", "second")
	out := e.run("goto", e.site.url("/upload-form"))
	out = e.run("upload", ref(t, out, `button "Document"`), "report.txt")
	mustContain(t, out, `Uploaded report.txt to button "Document"`, "report.txt")

	// A button that opens a file chooser for a hidden input: the chooser is
	// intercepted and reported, then answered by `upload` without a ref.
	out = e.run("click", ref(t, out, `button "Choose extras"`))
	mustContain(t, out, "### Modal state", "[File chooser]", "upload <file...>")
	e.run("upload", "a.txt", "b.txt")
	mustContain(t, e.fail("upload", "a.txt"), "no file chooser is open")
	mustContain(t, e.fail("upload", "e1", "missing.txt"), "cannot upload missing.txt")

	out = e.run("click", ref(t, e.run("snapshot"), `button "Send"`))
	mustContain(t, out, "- Page Title: Uploaded")
	e.site.mu.Lock()
	defer e.site.mu.Unlock()
	for name, want := range map[string]string{"report.txt": "quarterly numbers", "a.txt": "first", "b.txt": "second"} {
		if e.site.uploads[name] != want {
			t.Errorf("server received %q for %s, want %q", e.site.uploads[name], name, want)
		}
	}
}

func TestUploadThroughClickedChooser(t *testing.T) {
	e := shared(t)
	e.writeFile("c.txt", "third")
	out := e.run("goto", e.site.url("/upload-form"))
	// Passing the ref of a non-input clicks it and answers the chooser.
	e.run("upload", ref(t, out, `button "Choose extras"`), "c.txt")
	mustContain(t, e.run("eval", "document.getElementById('hidden-input').files[0].name"), `"c.txt"`)
}

func TestDownload(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/download-page"))
	text, err := e.try("click", ref(t, out, `link "Get report"`))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(e.dir, "downloads", "report.txt")
	mustContain(t, text, "### Events", "Downloaded file report.txt to", "report.txt", "- Page Title: Download")
	ev := e.waitEvent("download")
	if !strings.HasPrefix(ev.Path, want[:len(want)-len(".txt")]) || ev.URL != e.site.url("/download") || ev.Session != "default" {
		t.Errorf("unexpected download event %+v", ev)
	}
	data, err := os.ReadFile(ev.Path)
	if err != nil || string(data) != "downloaded content" {
		t.Fatalf("downloaded file: %q, %v", data, err)
	}
	// A direct navigation to a download is not an error either.
	mustContain(t, e.run("goto", e.site.url("/download")), "Downloaded file report.txt")
}

func TestScreenshot(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/sticky"))
	decode := func(args ...string) (w, h int) {
		t.Helper()
		res, err := e.m.Run(t.Context(), e.cwd, args)
		if err != nil || len(res.Files) != 1 || !filepath.IsAbs(res.Files[0]) {
			t.Fatalf("%v: files %v, error %v", args, res.Files, err)
		}
		data, err := os.ReadFile(res.Files[0])
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil || len(data) < 1000 {
			t.Fatalf("%s is not a plausible PNG (%d bytes): %v", res.Files[0], len(data), err)
		}
		return img.Bounds().Dx(), img.Bounds().Dy()
	}
	if w, h := decode("screenshot"); w != 1280 || h != 720 {
		t.Errorf("viewport screenshot is %dx%d", w, h)
	}
	if w, h := decode("screenshot", "--full", "--filename", "full.png"); w != 1280 || h < 3000 {
		t.Errorf("full-page screenshot is %dx%d", w, h)
	}
	if _, err := os.Stat(filepath.Join(e.cwd, "full.png")); err != nil {
		t.Errorf("--filename is not relative to the working directory: %v", err)
	}
	if w, h := decode("screenshot", ref(t, out, `button "Deep button"`)); w < 40 || w > 200 || h < 10 || h > 60 {
		t.Errorf("element screenshot is %dx%d", w, h)
	}
}

// Paths inside the caller's working directory are printed relative to it;
// Result.Files and paths elsewhere stay absolute.
func TestPathsAreRelativeToWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".artus", "browser")
	e := newEnv(t, envOptions{dir: dir, cwd: cwd})
	run := func(args ...string) Result {
		t.Helper()
		res, err := e.m.Run(t.Context(), cwd, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, f := range res.Files {
			if _, err := os.Stat(f); err != nil || !filepath.IsAbs(f) {
				t.Errorf("%v: Result.Files has %q: %v", args, f, err)
			}
		}
		return res
	}
	res := run("open", e.site.url("/long"))
	mustContain(t, res.Text, "profile: .artus/browser/profiles/default)", "- [Snapshot](.artus/browser/snapshots/default-1.yml)")
	mustNotContain(t, res.Text, cwd)

	res = run("screenshot")
	mustContain(t, res.Text, "saved to .artus/browser/output/default-screenshot-")
	res = run("screenshot", "--filename", "shots/a.png")
	mustContain(t, res.Text, "saved to shots/a.png")
	if len(res.Files) != 1 || res.Files[0] != filepath.Join(cwd, "shots", "a.png") {
		t.Errorf("Result.Files = %v", res.Files)
	}
	res = run("state-save")
	mustContain(t, res.Text, "to .artus/browser/output/default-state-")
	mustContain(t, run("config").Text, "Configuration file: .artus/browser/config.json")

	run("goto", e.site.url("/download-page"))
	res = run("click", "a")
	mustContain(t, res.Text, `Downloaded file report.txt to ".artus/browser/downloads/report.txt"`)
	if len(res.Files) != 1 || res.Files[0] != filepath.Join(dir, "downloads", "report.txt") {
		t.Errorf("Result.Files = %v", res.Files)
	}

	// A file outside the working directory keeps its full path.
	outside := filepath.Join(t.TempDir(), "snap.yml")
	res = run("snapshot", "--filename", outside)
	mustContain(t, res.Text, "- [Snapshot]("+outside+")")
	res = run("snapshot", "--filename", "inside.yml")
	mustContain(t, res.Text, "- [Snapshot](inside.yml)")
}
