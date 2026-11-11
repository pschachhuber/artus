package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenAndSnapshotFormat(t *testing.T) {
	e := shared(t)
	out := e.run("open", e.site.url("/"))
	mustContain(t, out,
		"### Result\n", `Browser session "default" is open (headless`,
		"### Page\n- Page URL: "+e.site.url("/")+"\n- Page Title: Home\n",
		"### Snapshot\n```yaml\n",
		`- heading "Welcome" [level=1] [ref=`,
		`- paragraph [ref=`, `]: Hello bold world`,
		"- link \"Sign in\" [ref=", "] [cursor=pointer]:\n  - /url: /form\n",
		"  - listitem [ref=", "]: One\n",
		`- button "Unavailable" [disabled] [ref=`,
		`- img "Logo" [ref=`,
	)
	mustNotContain(t, out, "Hidden text", "generic")

	// The same elements keep their refs from one snapshot to the next.
	again := e.run("snapshot")
	if a, b := ref(t, out, `link "Sign in"`), ref(t, again, `link "Sign in"`); a != b {
		t.Errorf("ref of an unchanged element changed from %s to %s", a, b)
	}
	// Each snapshot is also written to the state directory.
	files, _ := filepath.Glob(filepath.Join(e.dir, "snapshots", "default-*.yml"))
	if len(files) == 0 {
		t.Fatal("no snapshot file written")
	}
	data, _ := os.ReadFile(files[len(files)-1])
	mustContain(t, string(data), `- heading "Welcome" [level=1]`)
}

func TestClickNavigates(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/"))
	out = e.run("click", ref(t, out, `link "Sign in"`))
	mustContain(t, out, `Clicked link "Sign in" (`, "- Page URL: "+e.site.url("/form"), "- Page Title: Sign in", `- button "Submit"`)

	out = e.run("back")
	mustContain(t, out, "Went back", "- Page Title: Home")
	out = e.run("forward")
	mustContain(t, out, "- Page Title: Sign in")
	out = e.run("reload")
	mustContain(t, out, "Reloaded the page", "- Page Title: Sign in")
	mustContain(t, e.fail("forward"), "no next page")
}

func TestNavigationEdgeCases(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/links"))
	quick := func(args ...string) string {
		t.Helper()
		start := time.Now()
		text := e.run(args...)
		if d := time.Since(start); d > 2500*time.Millisecond {
			t.Errorf("%v took %s", args, d)
		}
		return text
	}
	// None of these produce a new document; none may stall the command.
	mustContain(t, quick("click", ref(t, out, `link "No content"`)), "- Page URL: "+e.site.url("/links")+"\n")
	mustContain(t, quick("click", ref(t, out, `link "Jump"`)), "- Page URL: "+e.site.url("/links#section")+"\n")
	mustContain(t, quick("click", ref(t, out, `link "Mail"`)), "- Page Title: Links")
	mustContain(t, quick("click", ref(t, out, `link "Script"`)), "- Page Title: script ran")
	mustContain(t, quick("back"), "- Page URL: "+e.site.url("/links")+"\n")

	// A slow navigation is waited for: the reply shows the new page.
	out = e.run("click", ref(t, out, `link "Slow page"`))
	mustContain(t, out, "- Page Title: Slow", `heading "Finally"`)
	out = e.run("back")
	// Enter in a text field submits its form.
	e.run("click", ref(t, out, `textbox "Query"`))
	e.run("type", "gophers")
	mustContain(t, e.run("press", "Enter"), "- Page Title: Submitted", "gophers")
}

func TestFormFillTypePressSubmit(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/form"))
	out = e.run("fill", ref(t, out, `textbox "Email"`), "ada@example.com")
	mustContain(t, out, `Filled textbox "Email"`, `with "ada@example.com"`, `textbox "Email" [active]`, "]: ada@example.com")

	// Typing goes to the focused element; Control+A then typing replaces.
	e.run("click", ref(t, out, `textbox "Code"`))
	e.run("type", "first draft")
	e.run("press", "ControlOrMeta+A") // select all: Control or, on macOS, Command
	out = e.run("type", "Zürich 42!")
	mustContain(t, out, `Typed "Zürich 42!"`, "]: Zürich 42!")

	notes := ref(t, out, `textbox "Notes"`)
	e.run("fill", notes, "to be removed")
	out = e.run("fill", notes, "") // filling with nothing clears the field
	mustContain(t, out, `textbox "Notes" [active] [ref=`+notes+"]\n")
	mustContain(t, e.run("eval", "document.querySelector('[name=notes]').value"), `""`)
	out = e.run("fill", notes, "line one")
	mustContain(t, out, "]: line one")
	e.run("fill", ref(t, out, `textbox "When"`), "2031-04-05")
	e.run("fill", ref(t, out, `slider "Volume"`), "8")
	e.run("fill", ref(t, out, `textbox "Tint"`), "#ff8800")
	out = e.run("fill", ref(t, out, `textbox "Bio"`), "Likes Go")
	mustContain(t, out, `textbox "Bio" [active]`, "]: Likes Go")
	mustContain(t, e.run("eval", "document.getElementById('bio').textContent"), `"Likes Go"`)

	// A read-only field is reported as such instead of being typed into.
	mustContain(t, e.fail("fill", ref(t, out, `textbox "Locked"`), "x", "--timeout", "600"), "timed out after 600ms", "disabled or read-only")

	out = e.run("fill", ref(t, out, `textbox "Password"`), "pw", "--submit")
	mustContain(t, out, "and pressed Enter", "- Page Title: Submitted", `heading "Thanks"`)
	form := e.site.lastSubmit(t)
	for field, want := range map[string]string{
		"email": "ada@example.com", "code": "Zürich 42!", "notes": "line one", "when": "2031-04-05",
		"volume": "8", "tint": "#ff8800", "password": "pw", "news": "on", "locked": "fixed",
	} {
		if got := strings.Join(form[field], ","); got != want {
			t.Errorf("submitted %s = %q, want %q", field, got, want)
		}
	}
}

func TestSecretsNeverReachOutput(t *testing.T) {
	// The caller's Redact is deliberately absent: values resolved for
	// --secret and --totp must be scrubbed by the package itself.
	e := shared(t)
	password, code := testSecrets["SITE_PASSWORD"], testTOTP["SITE_TOTP"]
	out := e.run("goto", e.site.url("/form"))
	out = e.run("fill", ref(t, out, `textbox "Password"`), "--secret", "SITE_PASSWORD")
	mustContain(t, out, `Filled textbox "Password"`, "with secret SITE_PASSWORD")
	out = e.run("fill", ref(t, out, `textbox "Code"`), "--totp", "SITE_TOTP")
	mustContain(t, out, "the current one-time code of SITE_TOTP", "[totp:SITE_TOTP]")
	// Unlike a password field, a plain one shows its value in the snapshot.
	e.run("click", ref(t, out, `textbox "Notes"`))
	out = e.run("type", "--secret", "SITE_PASSWORD")
	mustContain(t, out, "Typed secret SITE_PASSWORD", "[secret:SITE_PASSWORD]")
	e.run("eval", "document.querySelector('[name=notes]').value")
	e.run("eval", "el => el.value", ref(t, out, `textbox "Notes"`))
	e.run("find", "hunter2")
	e.run("snapshot", "--filename", "leak-check.yml")
	out = e.run("click", ref(t, out, `button "Submit"`))
	mustContain(t, out, "- Page Title: Submitted")
	mustContain(t, e.fail("fill", "e1", "--secret", "MISSING"), `secret "MISSING"`)
	mustContain(t, e.fail("fill", "e1", "text", "--secret", "SITE_PASSWORD"), "exactly one of")

	form := e.site.lastSubmit(t)
	if form.Get("password") != password || form.Get("code") != code || form.Get("notes") != password {
		t.Errorf("the page did not receive the secrets: %v", form)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, text := range e.texts {
		if strings.Contains(text, password) || strings.Contains(text, code) {
			t.Fatalf("a secret leaked into command output:\n%s", text)
		}
	}
	files, _ := filepath.Glob(filepath.Join(e.dir, "snapshots", "*.yml"))
	files = append(files, filepath.Join(e.cwd, "leak-check.yml"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), password) || strings.Contains(string(data), code) {
			t.Fatalf("a secret leaked into %s", file)
		}
	}
}

func TestCallerRedaction(t *testing.T) {
	e := newEnv(t, envOptions{redact: func(s string) string { return strings.ReplaceAll(s, "Welcome", "[redacted]") }})
	out := e.run("open", e.site.url("/"))
	mustContain(t, out, `heading "[redacted]"`)
	mustNotContain(t, out, "Welcome")
	mustNotContain(t, e.run("eval", "document.querySelector('h1').textContent"), "Welcome")
	mustNotContain(t, e.fail("click", "h1.Welcome", "--timeout", "300"), "Welcome")
}

func TestSelectCheckUncheck(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/form"))
	color := ref(t, out, `combobox "Color"`)
	out = e.run("select", color, "Green")
	mustContain(t, out, `Selected "Green" in combobox "Color"`, `option "Green" [selected]`)
	out = e.run("select", color, "b") // by value
	mustContain(t, out, `option "Blue" [selected]`)
	mustContain(t, e.fail("select", color, "Purple", "--timeout", "500"), `no option matches "Purple"`, "Red", "Blue")
	// The option list of a drop-down is browser UI out of our reach.
	mustContain(t, e.fail("click", color), "this is a <select>", "`select <ref> <value or label>`")

	remember := ref(t, out, `checkbox "Remember me"`)
	out = e.run("check", remember)
	mustContain(t, out, `checkbox "Remember me"`, "is now checked", `checkbox "Remember me" [checked]`)
	mustContain(t, e.run("check", remember), "was already checked")
	out = e.run("uncheck", ref(t, out, `checkbox "Newsletter"`))
	mustContain(t, out, "is now unchecked")
	mustNotContain(t, out, `checkbox "Newsletter" [checked]`)
	out = e.run("check", ref(t, out, `radio "Pro"`))
	mustContain(t, out, `radio "Pro" [checked]`)
	mustContain(t, e.fail("uncheck", ref(t, out, `radio "Pro"`)), "radio button cannot be unchecked")
	mustContain(t, e.fail("check", ref(t, out, `button "Submit"`)), "Not a checkbox or radio button")

	e.run("click", ref(t, out, `button "Submit"`))
	form := e.site.lastSubmit(t)
	if got := []string{form.Get("color"), form.Get("remember"), form.Get("news"), form.Get("plan")}; strings.Join(got, ",") != "b,on,,pro" {
		t.Errorf("submitted color, remember, news, plan = %v", got)
	}
}

func TestAutoWaitForDelayedElement(t *testing.T) {
	e := shared(t)
	e.run("goto", e.site.url("/delayed"))
	// The button does not exist yet, then exists disabled, then enables.
	out := e.run("click", "button")
	mustContain(t, out, `Clicked "button"`, "late clicked")
}

func TestCoveredElementNamesTheInterceptor(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/covered"))
	msg := e.fail("click", ref(t, out, `button "Buy now"`), "--timeout", "1200")
	mustContain(t, msg, "timed out after 1.2s", `button "Buy now"`, "element is covered by", `<div class="cookie-banner">We use cookies</div>`)
	mustContain(t, e.run("eval", "document.title"), `"Covered"`) // the click never happened
}

func TestBelowFoldAndStickyHeader(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/sticky"))
	// Far below the fold: scrolled into view first.
	out = e.run("click", ref(t, out, `button "Deep button"`))
	mustContain(t, out, "deep clicked")
	// Now the upper button sits above the viewport. Put it under the sticky
	// header: the default scroll would leave it covered, the retries with
	// other alignments free it.
	e.run("eval", "document.getElementById('top').scrollIntoView({block: 'start'})")
	out = e.run("click", ref(t, out, `button "Upper button"`))
	mustContain(t, out, "top clicked")
}

func TestStaleRef(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/"))
	link := ref(t, out, `link "Sign in"`)
	e.run("goto", e.site.url("/reset"))
	mustContain(t, e.fail("click", link), "ref "+link+" is not in the current page snapshot", "run `snapshot`")
	mustContain(t, e.fail("click", "e99999"), "is not in the current page snapshot")
	mustContain(t, e.fail("fill", "f9e1", "x"), "is not in the current page snapshot")
	mustContain(t, e.fail("click", "button[", "--timeout", "300"), "neither a ref from the snapshot")
}

func TestHoverDblclickAndModifiers(t *testing.T) {
	e := shared(t)
	e.run("goto", e.site.url("/"))
	e.run("eval", `(() => {
		const h = document.querySelector('h1');
		window.seen = [];
		for (const type of ['mouseover', 'click', 'dblclick', 'contextmenu'])
			h.addEventListener(type, ev => window.seen.push(type + (ev.shiftKey ? '+shift' : '') + ':' + ev.button));
	})()`)
	out := e.run("snapshot")
	heading := ref(t, out, `heading "Welcome"`)
	mustContain(t, e.run("hover", heading), `Hovering over heading "Welcome"`)
	e.run("dblclick", heading)
	e.run("click", heading, "--modifiers", "Shift")
	e.run("click", heading, "--button", "right")
	got := e.run("eval", "window.seen.join(' ')")
	mustContain(t, got, `mouseover:0 click:0 click:0 dblclick:0 click+shift:0 contextmenu:2`)
	mustContain(t, e.fail("click", heading, "--button", "side"), "--button must be")
	mustContain(t, e.fail("click", heading, "--modifiers", "Hyper"), "unknown modifier")
}

func TestDragAndDrop(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/drag"))
	out = e.run("drag", "#src", "#dst")
	mustContain(t, out, `Dragged "#src" onto "#dst"`, "dropped card-1")
}

func TestKeyboard(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/form"))
	e.run("eval", `(() => { window.keys = []; addEventListener('keydown', ev => window.keys.push(ev.key + (ev.shiftKey ? '+S' : '') + (ev.ctrlKey ? '+C' : ''))); })()`)
	e.run("click", ref(t, out, `textbox "Code"`))
	e.run("press", "a")
	e.run("press", "Shift+B")
	e.run("press", "?")
	e.run("press", "ArrowLeft")
	e.run("press", "Control+A")
	out = e.run("press", "Shift+Tab")
	mustContain(t, out, "Pressed Shift+Tab", `textbox "Password" [active]`, "]: aB?")
	mustContain(t, e.run("eval", "window.keys.join(' ')"), `"a Shift+S B+S ? ArrowLeft Control+C A+C Shift+S Tab+S"`)
	// Keys scroll the page like they do for a user.
	e.run("goto", e.site.url("/sticky"))
	e.run("press", "End")
	if strings.Contains(e.run("eval", "window.scrollY > 1000"), "false") {
		t.Error("End did not scroll the page")
	}
	mustContain(t, e.fail("press", "NoSuchKey"), `unknown key "NoSuchKey"`)
}
