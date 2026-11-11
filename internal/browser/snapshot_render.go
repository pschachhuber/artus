package browser

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// render writes the node in the YAML dialect of Playwright's snapshots:
//
//	```yaml
//	- link "Forgot password?" [ref=e9] [cursor=pointer]:
//	  - /url: /reset
//	```
//
// maxDepth, when positive, omits children below that depth. inPointer tells
// whether an ancestor already carries the pointer cursor marker.
func (n *snapNode) render(out *strings.Builder, depth, maxDepth int, inPointer bool) {
	indent := strings.Repeat("  ", depth)
	if n.role == "text" {
		out.WriteString(indent + "- text: " + yamlValue(n.text) + "\n")
		return
	}
	key := n.role
	if n.name != "" {
		key += " " + quoteJSON(n.name)
	}
	switch n.checked {
	case "mixed":
		key += " [checked=mixed]"
	case "true":
		key += " [checked]"
	}
	if n.disabled {
		key += " [disabled]"
	}
	if n.expanded {
		key += " [expanded]"
	}
	if n.active {
		key += " [active]"
	}
	if n.level > 0 {
		key += " [level=" + strconv.Itoa(n.level) + "]"
	}
	switch n.pressed {
	case "mixed":
		key += " [pressed=mixed]"
	case "true":
		key += " [pressed]"
	}
	if n.selected {
		key += " [selected]"
	}
	marksPointer := n.ref != "" && n.pointer && !inPointer
	if n.ref != "" {
		key += " [ref=" + n.ref + "]"
		if marksPointer {
			key += " [cursor=pointer]"
		}
	}
	out.WriteString(indent + "- " + yamlKey(key))
	children := n.children
	if maxDepth > 0 && depth >= maxDepth {
		children = nil
	}
	switch {
	case n.url == "" && len(children) == 0:
		out.WriteString("\n")
	case n.url == "" && n.onlyText() != "" && len(children) == 1:
		out.WriteString(": " + yamlValue(n.onlyText()) + "\n")
	default:
		out.WriteString(":\n")
		if n.url != "" {
			out.WriteString(indent + "  - /url: " + yamlValue(n.url) + "\n")
		}
		for _, c := range children {
			c.render(out, depth+1, maxDepth, inPointer || marksPointer)
		}
	}
}

// quoteJSON quotes s as a JSON string without escaping HTML characters.
func quoteJSON(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

var yamlAmbiguous = regexp.MustCompile(`(?i)^(y|n|yes|no|true|false|on|off|null|~|[-+]?(\.\d+|\d+(\.\d*)?)(e[-+]?\d+)?|0x[0-9a-f]+|[-+]?\.(inf|nan))$`)

// yamlNeedsQuotes reports whether s cannot stand as a plain YAML scalar.
func yamlNeedsQuotes(s string) bool {
	if s == "" || s != strings.TrimSpace(s) || yamlAmbiguous.MatchString(s) {
		return true
	}
	if strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(s[0])) {
		return true
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return true
	}
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// yamlKey quotes a mapping key when needed, using single quotes so that the
// double-quoted name inside stays readable.
func yamlKey(s string) string {
	if !yamlNeedsQuotes(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// yamlValue quotes a scalar value when needed.
func yamlValue(s string) string {
	if !yamlNeedsQuotes(s) {
		return s
	}
	return quoteJSON(s)
}
