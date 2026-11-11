package browser

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

// Modifier bits as the DevTools protocol defines them.
const (
	modAlt = 1 << iota
	modControl
	modMeta
	modShift
)

// keyDef describes a key of a US keyboard.
type keyDef struct {
	key      string // DOM KeyboardEvent.key
	code     string // DOM KeyboardEvent.code
	keyCode  int    // Windows virtual key code
	text     string // text the key produces, if any
	location int
}

// keyboard maps key names ("Enter", "a", "?") and codes ("KeyA") to their
// definitions; shifted maps a key to the one Shift turns it into.
var (
	keyboard = map[string]keyDef{}
	shifted  = map[string]string{}
)

var keyAliases = map[string]string{
	"Ctrl": "Control", "Cmd": "Meta", "Command": "Meta", "Option": "Alt",
	"Esc": "Escape", "Return": "Enter", "Space": " ", "Spacebar": " ",
	"Del": "Delete", "Up": "ArrowUp", "Down": "ArrowDown", "Left": "ArrowLeft",
	"Right": "ArrowRight",
}

func init() {
	add := func(code string, keyCode int, key, shiftKey string) {
		def := keyDef{key: key, code: code, keyCode: keyCode}
		if len([]rune(key)) == 1 {
			def.text = key
		}
		keyboard[key], keyboard[code] = def, def
		if shiftKey != "" {
			def.key, def.text = shiftKey, shiftKey
			keyboard[shiftKey], shifted[key] = def, shiftKey
		}
	}
	for i := 0; i < 26; i++ {
		upper := string(rune('A' + i))
		add("Key"+upper, 65+i, strings.ToLower(upper), upper)
	}
	for i, symbol := range ")!@#$%^&*(" {
		add(fmt.Sprintf("Digit%d", i), 48+i, fmt.Sprint(i), string(symbol))
	}
	for i := 1; i <= 12; i++ {
		add(fmt.Sprintf("F%d", i), 111+i, fmt.Sprintf("F%d", i), "")
	}
	for _, k := range []struct {
		code       string
		keyCode    int
		key, shift string
	}{
		{"Backquote", 192, "`", "~"}, {"Minus", 189, "-", "_"}, {"Equal", 187, "=", "+"},
		{"BracketLeft", 219, "[", "{"}, {"BracketRight", 221, "]", "}"}, {"Backslash", 220, `\`, "|"},
		{"Semicolon", 186, ";", ":"}, {"Quote", 222, "'", `"`}, {"Comma", 188, ",", "<"},
		{"Period", 190, ".", ">"}, {"Slash", 191, "/", "?"}, {"Space", 32, " ", ""},
		{"Enter", 13, "Enter", ""}, {"Tab", 9, "Tab", ""}, {"Backspace", 8, "Backspace", ""},
		{"Delete", 46, "Delete", ""}, {"Escape", 27, "Escape", ""}, {"Insert", 45, "Insert", ""},
		{"ArrowLeft", 37, "ArrowLeft", ""}, {"ArrowUp", 38, "ArrowUp", ""},
		{"ArrowRight", 39, "ArrowRight", ""}, {"ArrowDown", 40, "ArrowDown", ""},
		{"Home", 36, "Home", ""}, {"End", 35, "End", ""}, {"PageUp", 33, "PageUp", ""},
		{"PageDown", 34, "PageDown", ""}, {"CapsLock", 20, "CapsLock", ""},
		{"ContextMenu", 93, "ContextMenu", ""},
	} {
		add(k.code, k.keyCode, k.key, k.shift)
	}
	for _, m := range []struct {
		key     string
		keyCode int
	}{{"Shift", 16}, {"Control", 17}, {"Alt", 18}, {"Meta", 91}} {
		def := keyDef{key: m.key, code: m.key + "Left", keyCode: m.keyCode, location: 1}
		keyboard[m.key], keyboard[def.code] = def, def
		def.code, def.location = m.key+"Right", 2
		keyboard[def.code] = def
	}
	enter := keyboard["Enter"]
	enter.text = "\r"
	keyboard["Enter"] = enter
}

// lookupKey finds a key by name, code or alias.
func lookupKey(name string) (keyDef, error) {
	if alias, ok := keyAliases[name]; ok {
		name = alias
	}
	if name == "ControlOrMeta" {
		name = "Control"
		if runtime.GOOS == "darwin" {
			name = "Meta"
		}
	}
	if def, ok := keyboard[name]; ok {
		return def, nil
	}
	return keyDef{}, fmt.Errorf("unknown key %q; use names like Enter, Tab, Escape, ArrowDown, F5, a, A, or combinations like Control+A", name)
}

// parseKeyCombo splits "Control+Shift+A" into its keys. A plus sign that
// does not follow a key name is the "+" key itself, as in "Control++".
func parseKeyCombo(combo string) ([]keyDef, error) {
	var names []string
	current := ""
	for _, r := range combo {
		if r == '+' && current != "" {
			names = append(names, current)
			current = ""
		} else {
			current += string(r)
		}
	}
	names = append(names, current)
	defs := make([]keyDef, len(names))
	for i, name := range names {
		def, err := lookupKey(name)
		if err != nil {
			return nil, err
		}
		defs[i] = def
	}
	return defs, nil
}

func modifierBit(key string) int {
	switch key {
	case "Alt":
		return modAlt
	case "Control":
		return modControl
	case "Meta":
		return modMeta
	case "Shift":
		return modShift
	}
	return 0
}

// parseModifiers turns "Shift,Control" into a modifier mask.
func parseModifiers(list string) (int, error) {
	mask := 0
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		def, err := lookupKey(name)
		if err != nil || modifierBit(def.key) == 0 {
			return 0, fmt.Errorf("unknown modifier %q; use Alt, Control, Meta or Shift", name)
		}
		mask |= modifierBit(def.key)
	}
	return mask, nil
}

// macCommands maps Command shortcuts to editing commands: synthesized key
// events do not reach the macOS menu that normally implements them.
var macCommands = map[string]string{
	"a": "selectAll", "c": "copy", "x": "cut", "v": "paste", "z": "undo", "Z": "redo",
}

// dispatchKey sends one key event with an explicit modifier mask.
func dispatchKey(ctx context.Context, sess *cdpSession, down bool, def keyDef, modifiers int) error {
	if modifiers&modShift != 0 {
		if upper, ok := shifted[def.key]; ok {
			def = keyboard[upper]
		}
	}
	params := obj{
		"type": "keyUp", "modifiers": modifiers, "key": def.key, "code": def.code,
		"windowsVirtualKeyCode": def.keyCode, "location": def.location,
	}
	if down {
		text := def.text
		if modifiers&^modShift != 0 {
			text = "" // a shortcut, not typing
		}
		params["type"] = "rawKeyDown"
		if text != "" {
			params["type"], params["text"], params["unmodifiedText"] = "keyDown", text, text
		}
		if command, ok := macCommands[def.key]; ok && runtime.GOOS == "darwin" && modifiers&^modShift == modMeta {
			params["commands"] = []string{command}
		}
	}
	return sess.call(ctx, "Input.dispatchKeyEvent", params, nil)
}

func (t *tab) keyDown(ctx context.Context, def keyDef) error {
	t.modifiers |= modifierBit(def.key)
	return dispatchKey(ctx, t.sess, true, def, t.modifiers)
}

func (t *tab) keyUp(ctx context.Context, def keyDef) error {
	t.modifiers &^= modifierBit(def.key)
	return dispatchKey(ctx, t.sess, false, def, t.modifiers)
}

// pressKey presses and releases a key or a combination such as "Control+A".
func (t *tab) pressKey(ctx context.Context, combo string) error {
	defs, err := parseKeyCombo(combo)
	if err != nil {
		return err
	}
	for _, def := range defs {
		if err := t.keyDown(ctx, def); err != nil {
			return err
		}
	}
	for i := len(defs) - 1; i >= 0; i-- {
		if err := t.keyUp(ctx, defs[i]); err != nil {
			return err
		}
	}
	return nil
}

// insertText inserts text into the focused element as one input event, the
// way a paste or an input method would.
func (t *tab) insertText(ctx context.Context, text string) error {
	return t.sess.call(ctx, "Input.insertText", obj{"text": text}, nil)
}

// typeText types text key by key. Characters that are not on a US keyboard
// are inserted directly.
func (t *tab) typeText(ctx context.Context, text string) error {
	pending := ""
	flush := func() error {
		if pending == "" {
			return nil
		}
		text := pending
		pending = ""
		return t.insertText(ctx, text)
	}
	for _, r := range strings.ReplaceAll(text, "\r\n", "\n") {
		name := string(r)
		if r == '\n' {
			name = "Enter"
		}
		def, ok := keyboard[name]
		if !ok || (def.text == "" && r != '\n') {
			pending += name
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if err := dispatchKey(ctx, t.sess, true, def, t.modifiers); err != nil {
			return err
		}
		if err := dispatchKey(ctx, t.sess, false, def, t.modifiers); err != nil {
			return err
		}
	}
	return flush()
}

var mouseButtons = map[string]int{"left": 1, "right": 2, "middle": 4}

// dispatchMouse sends one mouse event to the page.
func dispatchMouse(ctx context.Context, sess *cdpSession, kind string, pt point, button string, buttons, count, modifiers int, extra obj) error {
	params := obj{
		"type": kind, "x": pt.x, "y": pt.y, "button": button, "buttons": buttons,
		"clickCount": count, "modifiers": modifiers,
	}
	for k, v := range extra {
		params[k] = v
	}
	return sess.call(ctx, "Input.dispatchMouseEvent", params, nil)
}

func (t *tab) mouseMove(ctx context.Context, pt point) error {
	button := "none"
	for name, bit := range mouseButtons {
		if t.buttons&bit != 0 {
			button = name
		}
	}
	t.mouseX, t.mouseY = pt.x, pt.y
	return dispatchMouse(ctx, t.sess, "mouseMoved", pt, button, t.buttons, 0, t.modifiers, nil)
}

func (t *tab) mouseDown(ctx context.Context, button string, count int) error {
	t.buttons |= mouseButtons[button]
	return dispatchMouse(ctx, t.sess, "mousePressed", point{t.mouseX, t.mouseY}, button, t.buttons, count, t.modifiers, nil)
}

func (t *tab) mouseUp(ctx context.Context, button string, count int) error {
	t.buttons &^= mouseButtons[button]
	return dispatchMouse(ctx, t.sess, "mouseReleased", point{t.mouseX, t.mouseY}, button, t.buttons, count, t.modifiers, nil)
}

// mouseClick moves to pt and clicks there.
func (t *tab) mouseClick(ctx context.Context, pt point, opts clickOptions) error {
	button := opts.button
	if button == "" {
		button = "left"
	}
	held := t.modifiers
	t.modifiers |= opts.modifiers
	defer func() { t.modifiers = held }()
	if err := t.mouseMove(ctx, pt); err != nil {
		return err
	}
	for i := 1; i <= max(opts.count, 1); i++ {
		if err := t.mouseDown(ctx, button, i); err != nil {
			return err
		}
		if err := t.mouseUp(ctx, button, i); err != nil {
			return err
		}
	}
	return nil
}
