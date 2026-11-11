package browser

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		argv    []string
		cmd     string
		session string
		args    []string
		flags   map[string]string
	}{
		{[]string{"open"}, "open", "default", nil, map[string]string{}},
		{[]string{}, "help", "default", nil, map[string]string{}},
		{[]string{"-s=work", "goto", "example.com"}, "goto", "work", []string{"example.com"}, map[string]string{"session": "work"}},
		{[]string{"goto", "--session", "work", "example.com"}, "goto", "work", []string{"example.com"}, map[string]string{"session": "work"}},
		{[]string{"goto", "example.com", "-s", "work"}, "goto", "work", []string{"example.com"}, map[string]string{"session": "work"}},
		{[]string{"--session=a", "open", "--headed", "--isolated", "u"}, "open", "a", []string{"u"}, map[string]string{"session": "a", "headed": "true", "isolated": "true"}},
		{[]string{"go-back"}, "back", "default", nil, map[string]string{}},
		{[]string{"fill", "e5", "hello world", "--submit"}, "fill", "default", []string{"e5", "hello world"}, map[string]string{"submit": "true"}},
		{[]string{"fill", "e5", "--secret", "NAME", "--timeout=250"}, "fill", "default", []string{"e5"}, map[string]string{"secret": "NAME", "timeout": "250"}},
		{[]string{"click", "e1", "--button=right", "--modifiers", "Shift,Control"}, "click", "default", []string{"e1"}, map[string]string{"button": "right", "modifiers": "Shift,Control"}},
		// Negative numbers are arguments, not flags.
		{[]string{"select", "e3", "-120"}, "select", "default", []string{"e3", "-120"}, map[string]string{}},
		// After "--" nothing is a flag.
		{[]string{"type", "--", "--submit"}, "type", "default", []string{"--submit"}, map[string]string{}},
		{[]string{"-s=x", "type", "--submit", "--", "-s=y"}, "type", "x", []string{"-s=y"}, map[string]string{"session": "x", "submit": "true"}},
		{[]string{"select", "e3", "a", "b", "c"}, "select", "default", []string{"e3", "a", "b", "c"}, map[string]string{}},
		{[]string{"snapshot", "--depth", "2", "--filename=out.yml"}, "snapshot", "default", nil, map[string]string{"depth": "2", "filename": "out.yml"}},
		{[]string{"wait", "--text", "--weird text"}, "wait", "default", nil, map[string]string{"text": "--weird text"}},
		{[]string{"open", "--headed=false"}, "open", "default", nil, map[string]string{"headed": "false"}},
	}
	for _, c := range cases {
		inv, err := parseArgs(c.argv)
		if err != nil {
			t.Errorf("%q: %v", c.argv, err)
			continue
		}
		if inv.cmd != c.cmd || inv.session != c.session || !slices.Equal(inv.args, c.args) || !reflect.DeepEqual(inv.flags, c.flags) {
			t.Errorf("%q parsed as cmd=%s session=%s args=%q flags=%v", c.argv, inv.cmd, inv.session, inv.args, inv.flags)
		}
	}
	if inv, _ := parseArgs([]string{"open", "--headed=false"}); inv.has("headed") {
		t.Error("--headed=false counts as set")
	}
	if inv, _ := parseArgs([]string{"click", "e1", "--timeout", "1500"}); must(inv.timeout(time.Second)) != 1500*time.Millisecond {
		t.Error("--timeout was not applied")
	}

	failures := map[string][]string{
		"unknown command":       {"explode"},
		"usage: artus browser":  {"click"},
		"usage: artus browser ": {"goto", "a", "b"},
		"unknown flag --depth":  {"click", "e1", "--depth", "2"},
		"needs a value":         {"click", "e1", "--button"},
		"mutually exclusive":    {"open", "--headed", "--headless"},
		"invalid session name":  {"-s=a/b", "open"},
	}
	for want, argv := range failures {
		if _, err := parseArgs(argv); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%q: error %v, want one containing %q", argv, err, want)
		}
	}
	if inv, _ := parseArgs([]string{"click", "e1", "--timeout", "soon"}); inv != nil {
		if _, err := inv.timeout(time.Second); err == nil {
			t.Error("a non-numeric --timeout was accepted")
		}
	}
}

func TestKeyCombos(t *testing.T) {
	keys := func(combo string) string {
		defs, err := parseKeyCombo(combo)
		if err != nil {
			return "error: " + err.Error()
		}
		var parts []string
		for _, d := range defs {
			parts = append(parts, d.key+"/"+d.code)
		}
		return strings.Join(parts, " ")
	}
	for combo, want := range map[string]string{
		"Enter":           "Enter/Enter",
		"a":               "a/KeyA",
		"A":               "A/KeyA",
		"Control+A":       "Control/ControlLeft A/KeyA",
		"Control+Shift+z": "Control/ControlLeft Shift/ShiftLeft z/KeyZ",
		"Shift+Tab":       "Shift/ShiftLeft Tab/Tab",
		"Meta+V":          "Meta/MetaLeft V/KeyV",
		"Ctrl+Alt+Delete": "Control/ControlLeft Alt/AltLeft Delete/Delete",
		"Control++":       "Control/ControlLeft +/Equal",
		"+":               "+/Equal",
		"Space":           " /Space",
		"Esc":             "Escape/Escape",
		"ArrowDown":       "ArrowDown/ArrowDown",
		"Down":            "ArrowDown/ArrowDown",
		"F12":             "F12/F12",
		"KeyQ":            "q/KeyQ",
		"?":               "?/Slash",
		"5":               "5/Digit5",
		"%":               "%/Digit5",
	} {
		if got := keys(combo); got != want {
			t.Errorf("parseKeyCombo(%q) = %s, want %s", combo, got, want)
		}
	}
	for _, bad := range []string{"", "Hyper+A", "Control+", "Enter+NoSuchKey", "é"} {
		if got := keys(bad); !strings.HasPrefix(got, "error: unknown key") {
			t.Errorf("parseKeyCombo(%q) = %s, want an unknown key error", bad, got)
		}
	}
	for def, want := range map[string]keyDef{
		"Enter": {key: "Enter", code: "Enter", keyCode: 13, text: "\r"},
		"a":     {key: "a", code: "KeyA", keyCode: 65, text: "a"},
		"Z":     {key: "Z", code: "KeyZ", keyCode: 90, text: "Z"},
		"!":     {key: "!", code: "Digit1", keyCode: 49, text: "!"},
		"F5":    {key: "F5", code: "F5", keyCode: 116},
		"Shift": {key: "Shift", code: "ShiftLeft", keyCode: 16, location: 1},
		" ":     {key: " ", code: "Space", keyCode: 32, text: " "},
	} {
		if got := keyboard[def]; got != want {
			t.Errorf("keyboard[%q] = %+v, want %+v", def, got, want)
		}
	}
	for want, got := range map[string]string{
		"selectAll": macCommand("KeyA", modMeta), "redo": macCommand("KeyZ", modShift|modMeta),
		"deleteBackward": macCommand("Backspace", 0), "moveLeftAndModifySelection": macCommand("ArrowLeft", modShift),
		"": macCommand("KeyA", modControl),
	} {
		if got != want {
			t.Errorf("macCommand = %q, want %q", got, want)
		}
	}
	if mask, err := parseModifiers("Shift, Ctrl,Alt"); err != nil || mask != modShift|modControl|modAlt {
		t.Errorf("parseModifiers = %d, %v", mask, err)
	}
	if mask, err := parseModifiers(""); err != nil || mask != 0 {
		t.Errorf("parseModifiers of nothing = %d, %v", mask, err)
	}
	if _, err := parseModifiers("Shift,Enter"); err == nil {
		t.Error("Enter was accepted as a modifier")
	}
}

func TestSnapshotRendering(t *testing.T) {
	tree := &snapNode{role: "navigation", name: "Main: menu", ref: "e1", children: []*snapNode{
		{role: "link", name: `Say "hi"`, ref: "e2", pointer: true, url: "/hi?x=1#top", children: []*snapNode{
			{role: "img", name: "Wave", ref: "e3", pointer: true},
		}},
		{role: "checkbox", name: "On", checked: "mixed", disabled: true, active: true, ref: "f1e4"},
		{role: "button", name: "Toggle", pressed: "true", expanded: true, selected: true},
		{role: "heading", level: 2, children: []*snapNode{{role: "text", text: "Plain"}}},
		{role: "textbox", name: "Amount", ref: "e5", children: []*snapNode{{role: "text", text: "42"}}},
		{role: "textbox", name: "Note", children: []*snapNode{{role: "text", text: "a: b #c\nnext"}}},
		{role: "text", text: "- loose text"},
		{role: "list", children: []*snapNode{{role: "listitem", children: []*snapNode{{role: "text", text: "deep"}}}}},
	}}
	var out strings.Builder
	tree.render(&out, 0, 0, false)
	want := `- 'navigation "Main: menu" [ref=e1]':
  - link "Say \"hi\"" [ref=e2] [cursor=pointer]:
    - /url: /hi?x=1#top
    - img "Wave" [ref=e3]
  - checkbox "On" [checked=mixed] [disabled] [active] [ref=f1e4]
  - button "Toggle" [expanded] [pressed] [selected]
  - heading [level=2]: Plain
  - textbox "Amount" [ref=e5]: "42"
  - textbox "Note": "a: b #c\nnext"
  - text: "- loose text"
  - list:
    - listitem: deep
`
	if out.String() != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	tree.render(&out, 0, 1, false)
	if strings.Contains(out.String(), "listitem") || strings.Contains(out.String(), "Wave") || !strings.Contains(out.String(), "  - list\n") {
		t.Errorf("depth limit not applied:\n%s", out.String())
	}

	for in, want := range map[string]string{
		"plain text": "plain text", "": `""`, "true": `"true"`, "12.5": `"12.5"`, " padded": `" padded"`,
		"#tag": `"#tag"`, "key: value": `"key: value"`, "tab\there": `"tab\there"`, "it's fine": "it's fine",
		"[x]": `"[x]"`, "ends:": `"ends:"`, "a#b": "a#b", "<b>&": `<b>&`,
	} {
		if got := yamlValue(in); got != want {
			t.Errorf("yamlValue(%q) = %s, want %s", in, got, want)
		}
	}
	for target, want := range map[string]string{
		"http://a.test/reset?x=1#y": "/reset?x=1#y", "http://a.test/": "/", "http://a.test": "/",
		"https://a.test/x": "https://a.test/x", "http://b.test/x": "http://b.test/x", "mailto:x@y.z": "mailto:x@y.z",
		"data:image/png;base64,AAAA": "data:image/png;base64,…", "http://a.test:8080/p": "http://a.test:8080/p",
	} {
		if got := relativeURL(target, "http://a.test/page"); got != want {
			t.Errorf("relativeURL(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestOriginFilter(t *testing.T) {
	allows := func(f originFilter, raw string) bool { return f.allows(raw) }
	open := newOriginFilter(networkConfig{})
	if open.active() || !allows(open, "https://anything.example/") {
		t.Error("an empty filter must allow everything")
	}
	f := newOriginFilter(networkConfig{
		AllowedOrigins: []string{"https://example.com", "http://localhost:*", "*.corp.test", "https://api.test:8443/"},
		BlockedOrigins: []string{"https://ads.corp.test", "tracker.test"},
	})
	for raw, want := range map[string]bool{
		"https://example.com/a":        true,
		"https://example.com:443/a":    true,
		"http://example.com/a":         false, // other scheme
		"https://example.com:8443/a":   false, // other port
		"https://www.example.com/":     false, // subdomains only with *.
		"http://localhost:3000/x":      true,
		"http://localhost/x":           true,
		"https://localhost:3000/x":     false,
		"https://corp.test/":           true,
		"http://deep.sub.corp.test:81": true,
		"https://ads.corp.test/pixel":  false, // blocked wins
		"https://api.test:8443/v1":     true,
		"https://api.test/v1":          false,
		"https://tracker.test/t.js":    false,
		"wss://example.com/socket":     false,
		"https://elsewhere.test/":      false,
		"data:text/html,hi":            true, // not a network request
		"about:blank":                  true,
		"blob:https://x.test/uuid":     true,
	} {
		if got := allows(f, raw); got != want {
			t.Errorf("allows(%q) = %v, want %v", raw, got, want)
		}
	}
	blockOnly := newOriginFilter(networkConfig{BlockedOrigins: []string{"https://ads.test"}})
	if !allows(blockOnly, "https://example.com/") || allows(blockOnly, "https://ads.test/x") {
		t.Error("with only a block list everything else must be allowed")
	}
	if u, _ := url.Parse("https://Example.COM/"); !parseOriginPattern("https://example.com").matches(u) {
		t.Error("host matching must ignore case")
	}
}

func TestNormalizeURLAndGlob(t *testing.T) {
	for in, want := range map[string]string{
		"example.com": "https://example.com", "example.com/a?b": "https://example.com/a?b",
		"localhost:8080/x": "http://localhost:8080/x", "127.0.0.1:9": "http://127.0.0.1:9",
		"http://a.test": "http://a.test", "about:blank": "about:blank", "file:///tmp/x.html": "file:///tmp/x.html",
		"data:text/html,<p>": "data:text/html,<p>",
	} {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	for pattern, urls := range map[string]map[string]bool{
		"**/login":            {"https://a.test/login": true, "https://a.test/x/login": true, "https://a.test/login/next": false},
		"https://a.test/*":    {"https://a.test/x": true, "https://a.test/x/y": false},
		"https://a.test/**":   {"https://a.test/x/y?z=1": true, "https://b.test/x": false},
		"/dashboard":          {"https://a.test/dashboard?tab=1": true, "https://a.test/other": false},
		"https://a.test/?.js": {"https://a.test/a.js": true, "https://a.test/ab.js": false},
	} {
		re := must(globToRegexp(pattern))
		for raw, want := range urls {
			if re.MatchString(raw) != want {
				t.Errorf("glob %q on %q = %v, want %v", pattern, raw, !want, want)
			}
		}
	}
}
