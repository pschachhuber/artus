package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// axNode is a node of Chrome's accessibility tree as the protocol reports it.
type axNode struct {
	NodeID  string `json:"nodeId"`
	Ignored bool   `json:"ignored"`
	Role    struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"role"`
	Name struct {
		Value   string `json:"value"`
		Sources []struct {
			Type       string           `json:"type"`
			Superseded bool             `json:"superseded"`
			Value      *json.RawMessage `json:"value"`
		} `json:"sources"`
	} `json:"name"`
	Value struct {
		Value json.RawMessage `json:"value"`
	} `json:"value"`
	Properties []struct {
		Name  string `json:"name"`
		Value struct {
			Value json.RawMessage `json:"value"`
		} `json:"value"`
	} `json:"properties"`
	ChildIDs         []string `json:"childIds"`
	ParentID         string   `json:"parentId"`
	BackendDOMNodeID int      `json:"backendDOMNodeId"`
}

// prop returns a property's value as text: true, false, mixed, a number, a
// URL and so on.
func (n *axNode) prop(name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return rawText(p.Value.Value)
		}
	}
	return ""
}

// nameFromContents reports whether the accessible name was computed from the
// node's own content rather than a label or attribute.
func (n *axNode) nameFromContents() bool {
	for _, src := range n.Name.Sources {
		if !src.Superseded && src.Value != nil {
			return src.Type == "contents"
		}
	}
	return false
}

func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// layoutInfo is what the snapshot needs to know about an element's box.
type layoutInfo struct {
	box       bool // has a non-empty layout box
	hidden    bool // visibility other than visible
	untouched bool // pointer-events: none
	pointer   bool // cursor: pointer
}

// frameSnapshot is the raw material for one frame of a snapshot.
type frameSnapshot struct {
	frame frameRef
	nodes map[string]*axNode
	root  *axNode
	owner int // backend node id of the frame's <iframe> element in its parent
}

type frameOwnerKey struct {
	sess    *cdpSession
	backend int
}

// snapshotData is everything fetched from the browser for a snapshot.
type snapshotData struct {
	frames []*frameSnapshot
	layout map[*cdpSession]map[int]layoutInfo
	owners map[frameOwnerKey]*frameSnapshot
}

// captureSnapshot fetches the accessibility tree of every frame plus the
// layout facts needed to decide which nodes can be acted upon. Frames are
// fetched concurrently; a frame that fails (it may be navigating) is left
// out, except the main frame, whose failure fails the snapshot.
func (t *tab) captureSnapshot(ctx context.Context) (*snapshotData, error) {
	frames := t.frameRefs()
	if len(frames) == 0 {
		return nil, fmt.Errorf("the page has no document yet")
	}
	data := &snapshotData{
		layout: map[*cdpSession]map[int]layoutInfo{},
		owners: map[frameOwnerKey]*frameSnapshot{},
	}
	byID := map[string]frameRef{}
	for _, f := range frames {
		byID[f.id] = f
		data.layout[f.sess] = nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := make([]error, len(frames))
	snaps := make([]*frameSnapshot, len(frames))
	sessions := make([]*cdpSession, 0, len(data.layout))
	for sess := range data.layout {
		sessions = append(sessions, sess)
	}
	for _, sess := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			layout := captureLayout(ctx, sess)
			mu.Lock()
			data.layout[sess] = layout
			mu.Unlock()
		}()
	}
	for i, f := range frames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps[i], errs[i] = captureFrame(ctx, f, byID[f.parentID])
		}()
	}
	wg.Wait()
	if errs[0] != nil {
		return nil, errs[0]
	}
	for i, fs := range snaps {
		if errs[i] != nil {
			continue
		}
		data.frames = append(data.frames, fs)
		if parent, ok := byID[fs.frame.parentID]; ok && fs.owner != 0 {
			data.owners[frameOwnerKey{parent.sess, fs.owner}] = fs
		}
	}
	return data, nil
}

func captureFrame(ctx context.Context, f, parent frameRef) (*frameSnapshot, error) {
	var tree struct {
		Nodes []*axNode `json:"nodes"`
	}
	if err := f.sess.call(ctx, "Accessibility.getFullAXTree", obj{"frameId": f.id}, &tree); err != nil {
		return nil, err
	}
	fs := &frameSnapshot{frame: f, nodes: make(map[string]*axNode, len(tree.Nodes))}
	for _, n := range tree.Nodes {
		fs.nodes[n.NodeID] = n
		if n.ParentID == "" && fs.root == nil {
			fs.root = n
		}
	}
	if fs.root == nil {
		return nil, fmt.Errorf("frame %s has no accessibility tree", f.id)
	}
	if parent.sess != nil {
		var owner struct {
			BackendNodeID int `json:"backendNodeId"`
		}
		if err := parent.sess.call(ctx, "DOM.getFrameOwner", obj{"frameId": f.id}, &owner); err != nil {
			return nil, err
		}
		fs.owner = owner.BackendNodeID
	}
	return fs, nil
}

// captureLayout asks one renderer process for the layout box and a few
// computed styles of every node, in a single call covering all its frames.
func captureLayout(ctx context.Context, sess *cdpSession) map[int]layoutInfo {
	var snap struct {
		Documents []struct {
			Nodes struct {
				BackendNodeID []int `json:"backendNodeId"`
			} `json:"nodes"`
			Layout struct {
				NodeIndex []int       `json:"nodeIndex"`
				Styles    [][]int     `json:"styles"`
				Bounds    [][]float64 `json:"bounds"`
			} `json:"layout"`
		} `json:"documents"`
		Strings []string `json:"strings"`
	}
	styles := []string{"cursor", "pointer-events", "visibility"}
	if err := sess.call(ctx, "DOMSnapshot.captureSnapshot", obj{"computedStyles": styles}, &snap); err != nil {
		return nil
	}
	str := func(indexes []int, i int) string {
		if i < len(indexes) && indexes[i] >= 0 && indexes[i] < len(snap.Strings) {
			return snap.Strings[indexes[i]]
		}
		return ""
	}
	out := map[int]layoutInfo{}
	for _, doc := range snap.Documents {
		for i, nodeIndex := range doc.Layout.NodeIndex {
			if nodeIndex >= len(doc.Nodes.BackendNodeID) || i >= len(doc.Layout.Bounds) || i >= len(doc.Layout.Styles) {
				continue
			}
			id := doc.Nodes.BackendNodeID[nodeIndex]
			info := out[id]
			if b := doc.Layout.Bounds[i]; len(b) == 4 && b[2] > 0 && b[3] > 0 {
				info.box = true
			}
			st := doc.Layout.Styles[i]
			info.pointer = info.pointer || str(st, 0) == "pointer"
			info.untouched = str(st, 1) == "none"
			info.hidden = str(st, 2) != "visible"
			out[id] = info
		}
	}
	return out
}

// nodeRef is what a ref from a snapshot points at.
type nodeRef struct {
	frameID  string
	loaderID string
	backend  int
	role     string
	name     string
}

// assignedRef remembers the ref given to a DOM node, so that the same
// element keeps its ref from one snapshot to the next.
type assignedRef struct {
	ref  string
	role string
	name string
}

// snapshot captures the page and returns the rendered tree. When root is not
// empty only the subtree of that ref is rendered; depth, when positive,
// limits how deep the rendering goes. The tab's ref table is replaced.
func (t *tab) snapshot(ctx context.Context, root string, depth int) (string, error) {
	var data *snapshotData
	var err error
	// The page may be between documents; give it a moment to produce a tree.
	for attempt := 0; ; attempt++ {
		if data, err = t.captureSnapshot(ctx); err == nil || attempt == 4 || ctx.Err() != nil || t.isClosed() {
			break
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	if err != nil {
		return "", err
	}
	b := &snapBuilder{data: data}
	top := b.build(data.frames[0])
	t.assignRefs(top)
	if root != "" {
		node := findRef(top, root)
		if node == nil {
			return "", staleRefError(root)
		}
		top = []*snapNode{node}
	}
	var out strings.Builder
	for _, n := range top {
		n.render(&out, 0, depth, false)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// assignRefs numbers the actionable nodes of a snapshot and records where
// each ref points.
func (t *tab) assignRefs(top []*snapNode) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	refs := map[string]nodeRef{}
	live := map[string]map[int]assignedRef{}
	var walk func(nodes []*snapNode)
	walk = func(nodes []*snapNode) {
		for _, n := range nodes {
			f := t.frames[n.frame.id]
			if n.actionable() && f != nil {
				prev, ok := f.nodeRefs[n.backend]
				if !ok || prev.role != n.role || prev.name != n.name {
					// Numbers are never reused within a tab, so that a ref
					// from an old snapshot cannot come to mean another
					// element. The main frame counts per tab because its
					// frame id can change across navigations.
					seq, prefix := &t.refSeq, ""
					if f.seq > 0 {
						seq, prefix = &f.refSeq, "f"+strconv.Itoa(f.seq)
					}
					*seq++
					prev = assignedRef{ref: prefix + "e" + strconv.Itoa(*seq), role: n.role, name: n.name}
				}
				if live[f.id] == nil {
					live[f.id] = map[int]assignedRef{}
				}
				live[f.id][n.backend] = prev
				n.ref = prev.ref
				refs[n.ref] = nodeRef{frameID: f.id, loaderID: n.frame.loaderID, backend: n.backend, role: n.role, name: n.name}
			}
			walk(n.children)
		}
	}
	walk(top)
	for id, f := range t.frames {
		f.nodeRefs = live[id]
		if f.nodeRefs == nil {
			f.nodeRefs = map[int]assignedRef{}
		}
	}
	t.refs = refs
}

func findRef(nodes []*snapNode, ref string) *snapNode {
	for _, n := range nodes {
		if n.ref == ref {
			return n
		}
		if found := findRef(n.children, ref); found != nil {
			return found
		}
	}
	return nil
}

func staleRefError(ref string) error {
	return fmt.Errorf("ref %s is not in the current page snapshot (the page changed or the ref is from an older snapshot); run `snapshot` and use a fresh ref", ref)
}
