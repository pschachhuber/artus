package browser

import (
	"net/url"
	"strconv"
	"strings"
)

// snapNode is a node of the rendered snapshot: an element with a role, or a
// run of text (role "text").
type snapNode struct {
	role     string
	name     string
	text     string
	children []*snapNode

	checked  string // "true" or "mixed"
	pressed  string
	disabled bool
	expanded bool
	active   bool
	selected bool
	level    int
	url      string

	frame        frameRef
	backend      int
	ref          string
	visible      bool // has a box and receives pointer events
	pointer      bool // cursor: pointer
	fromContents bool // the name repeats the node's own content
}

// internalRoles maps Chrome's internal accessibility roles onto the ARIA
// roles a Playwright snapshot would show. Internal roles not listed here
// become "generic".
var internalRoles = map[string]string{
	"Iframe": "iframe", "IframePresentational": "iframe",
	"SvgRoot": "img", "Canvas": "canvas", "Video": "video", "Audio": "audio",
	"Date": "textbox", "DateTime": "textbox", "InputTime": "textbox", "ColorWell": "textbox",
	"DisclosureTriangle": "button", "DisclosureTriangleGrouped": "button", "ToggleButton": "button",
	"PopUpButton": "combobox", "ComboBoxSelect": "combobox", "ComboBoxMenuButton": "combobox",
	"ComboBoxGrouping": "combobox", "TextFieldWithComboBox": "combobox",
	"MenuListOption": "option", "Details": "group",
	"DescriptionList": "list", "DescriptionListTerm": "term", "DescriptionListDetail": "definition",
	"EmbeddedObject": "embed", "PluginObject": "embed",
}

// valueRoles render their current value instead of their inner structure.
var valueRoles = map[string]bool{
	"textbox": true, "searchbox": true, "spinbutton": true, "slider": true,
	"progressbar": true, "meter": true, "scrollbar": true,
}

// identifyingNames lists roles whose name labels a whole group and is kept
// even when the group's content is rendered too.
var identifyingNames = map[string]bool{
	"alertdialog": true, "dialog": true, "banner": true, "complementary": true,
	"contentinfo": true, "form": true, "main": true, "navigation": true,
	"region": true, "search": true, "application": true, "document": true,
	"figure": true, "group": true, "grid": true, "listbox": true, "menu": true,
	"menubar": true, "radiogroup": true, "tablist": true, "tabpanel": true,
	"toolbar": true, "tree": true, "treegrid": true, "table": true,
	"columnheader": true, "rowheader": true, "iframe": true,
}

// snapBuilder turns raw accessibility trees into the snapshot tree.
type snapBuilder struct {
	data *snapshotData
}

// build converts a frame's tree, descending into child frames.
func (b *snapBuilder) build(fs *frameSnapshot) []*snapNode {
	var out []*snapNode
	b.convert(fs, fs.root, false, &out)
	return mergeText(out)
}

// convert appends the snapshot nodes for n to out. Ignored and purely
// presentational nodes contribute their children directly.
func (b *snapBuilder) convert(fs *frameSnapshot, n *axNode, inPointer bool, out *[]*snapNode) {
	role := n.Role.Value
	switch role {
	case "InlineTextBox", "ListMarker":
		return
	case "StaticText":
		*out = append(*out, &snapNode{role: "text", text: n.Name.Value})
		return
	case "LineBreak":
		*out = append(*out, &snapNode{role: "text", text: " "})
		return
	}
	if n.Role.Type == "internalRole" {
		if mapped, ok := internalRoles[role]; ok {
			role = mapped
		} else if role != "RootWebArea" && role != "MenuListPopup" {
			role = "generic"
		}
	} else if role == "image" {
		role = "img"
	}
	transparent := n.Ignored || role == "none" || role == "presentation" || role == "RootWebArea" || role == "MenuListPopup"

	sn := &snapNode{role: role, frame: fs.frame, backend: n.BackendDOMNodeID}
	if !transparent {
		layout := b.data.layout[fs.frame.sess][n.BackendDOMNodeID]
		sn.visible = layout.box && !layout.hidden && !layout.untouched
		sn.pointer = layout.pointer
	}
	var children []*snapNode
	childPointer := inPointer || (sn.visible && sn.pointer)
	for _, id := range n.ChildIDs {
		if child := fs.nodes[id]; child != nil {
			b.convert(fs, child, childPointer, &children)
		}
	}
	if transparent {
		*out = append(*out, children...)
		return
	}
	sn.name = normalizeSpace(n.Name.Value)
	sn.fromContents = n.nameFromContents()
	sn.disabled = n.prop("disabled") == "true"
	sn.expanded = n.prop("expanded") == "true"
	sn.active = n.prop("focused") == "true"
	sn.selected = n.prop("selected") == "true"
	if v := n.prop("checked"); v == "true" || v == "mixed" {
		sn.checked = v
	}
	if v := n.prop("pressed"); v == "true" || v == "mixed" {
		sn.pressed = v
	}
	if role == "heading" || role == "treeitem" {
		sn.level, _ = strconv.Atoi(n.prop("level"))
	}
	if role == "link" {
		sn.url = relativeURL(n.prop("url"), fs.frame.url)
	}
	value := rawText(n.Value.Value)
	switch {
	case role == "iframe":
		children = nil
		if child := b.data.owners[frameOwnerKey{fs.frame.sess, n.BackendDOMNodeID}]; child != nil {
			children = b.build(child)
		}
	case valueRoles[role] && n.prop("editable") != "richtext":
		// Native controls: show the value, not the browser's inner widgets.
		children = textChild(value)
	case role == "combobox" && len(children) == 0:
		children = textChild(value)
	case role == "generic" && n.prop("editable") == "richtext" && n.prop("focusable") == "true":
		// The root of a contenteditable region is a text box in all but role.
		sn.role = "textbox"
	}
	sn.children = children
	*out = append(*out, b.distill(sn, inPointer)...)
}

func textChild(text string) []*snapNode {
	if text == "" {
		return nil
	}
	return []*snapNode{{role: "text", text: text}}
}

// distill makes a node less verbose without losing information, once its
// children are final. It returns what should take the node's place: the
// node, its children (unwrapped), or nothing.
func (b *snapBuilder) distill(n *snapNode, inPointer bool) []*snapNode {
	n.children = mergeText(n.children)
	// A generic child that only repeats this node's name says nothing new.
	if n.name != "" {
		kept := n.children[:0]
		for _, c := range n.children {
			if c.role == "generic" && !c.active && (c.name == "" && c.onlyText() == n.name || c.name == n.name && len(c.children) == 0) {
				continue
			}
			kept = append(kept, c)
		}
		n.children = kept
	}
	allText := true
	for _, c := range n.children {
		allText = allText && c.role == "text"
	}
	switch {
	case len(n.children) == 0:
	case allText && !valueRoles[n.role] && (n.fromContents || n.onlyText() == n.name):
		// The name already is the content.
		n.children = nil
	case !allText && n.fromContents && !identifyingNames[n.role]:
		// The content is rendered below; a name derived from it is noise.
		n.name = ""
	}
	clickRoot := n.visible && n.pointer && !inPointer
	if !n.visible && n.name == "" && len(n.children) == 0 {
		return nil // nothing to read and nothing to act on
	}
	switch n.role {
	case "img":
		if n.name == "" && len(n.children) == 0 && !clickRoot {
			return nil
		}
	case "generic":
		if n.name != "" || n.active {
			break
		}
		// Text wrapped in a second nameless generic belongs to this one.
		if len(n.children) == 1 {
			if c := n.children[0]; c.role == "generic" && c.name == "" && !c.active && c.onlyText() != "" {
				n.children = c.children
			}
		}
		switch {
		case len(n.children) == 0 && !clickRoot:
			return nil
		case len(n.children) == 1 && n.children[0].role != "text" && n.children[0].visible:
			// A wrapper around a single element adds only indentation.
			return n.children
		}
	}
	return []*snapNode{n}
}

// onlyText returns the node's text when its content is a single text run.
func (n *snapNode) onlyText() string {
	if len(n.children) == 1 && n.children[0].role == "text" {
		return n.children[0].text
	}
	return ""
}

// actionable reports whether the node gets a ref.
func (n *snapNode) actionable() bool {
	return n.role != "text" && n.visible && n.backend != 0
}

// mergeText joins adjacent text runs, normalises their whitespace and drops
// empty ones.
func mergeText(nodes []*snapNode) []*snapNode {
	out := nodes[:0]
	var pending *snapNode
	flush := func() {
		if pending != nil {
			if pending.text = normalizeSpace(pending.text); pending.text != "" {
				out = append(out, pending)
			}
			pending = nil
		}
	}
	for _, n := range nodes {
		if n.role != "text" {
			flush()
			out = append(out, n)
			continue
		}
		if pending == nil {
			pending = &snapNode{role: "text"}
		}
		pending.text += n.text
	}
	flush()
	return out
}

// normalizeSpace collapses runs of whitespace and trims the ends.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}

// relativeURL shortens a link target on the document's own origin to its
// path, and trims the payload of data: URLs.
func relativeURL(target, base string) string {
	if strings.HasPrefix(target, "data:") {
		if i := strings.IndexByte(target, ','); i >= 0 && len(target) > i+1 {
			return target[:i+1] + "…"
		}
		return target
	}
	t, err := url.Parse(target)
	if err != nil {
		return target
	}
	if b, err := url.Parse(base); err == nil && t.Host != "" && t.Scheme == b.Scheme && t.Host == b.Host {
		t.Scheme, t.Host, t.User = "", "", nil
		if rel := t.String(); rel != "" {
			return rel
		}
		return "/"
	}
	return target
}
