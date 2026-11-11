package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

//go:embed injected.js
var injectedSource string

// injectedFunction is the in-page helper, called with an operation name and
// its argument.
var injectedFunction = "async function(op, arg) {\n" + injectedSource + "\n}"

// isolatedWorld names the JavaScript world our helpers run in, out of reach
// of the page's own scripts.
const isolatedWorld = "artus"

var refPattern = regexp.MustCompile(`^(f\d+)?e\d+$`)

var (
	// errStale means the node or its document no longer exists.
	errStale = errors.New("element is no longer attached to the page")
	// errNoMatch means a CSS selector matched nothing (yet).
	errNoMatch = errors.New("no element matches the selector")
)

type point struct{ x, y float64 }

// element is a resolved action target.
type element struct {
	t        *tab
	frame    frameRef
	backend  int
	label    string // how messages refer to the element
	objectID string // handle in the isolated world, resolved on first use
	dropdown bool   // a <select> rendered as a drop-down, known after waitStates
}

// isStale reports whether err means the element, its frame or its document
// went away underneath us.
func isStale(err error) bool {
	if errors.Is(err, errStale) || errors.Is(err, errTargetClosed) {
		return true
	}
	for _, msg := range []string{
		"No node with given id", "Could not find node with given id", "does not belong to the document",
		"Cannot find context with specified id", "Execution context was destroyed", "Could not find object with given id",
		"Frame with the given id", "Node is detached from document", "Inspected target navigated or closed",
		"Invalid remote object id", "No frame for given id", "Frame not found",
	} {
		if cdpErrorContains(err, msg) {
			return true
		}
	}
	return false
}

// resolve finds the element a target names: a ref from the latest snapshot,
// or a CSS selector evaluated in the main frame.
func (t *tab) resolve(ctx context.Context, target string) (*element, error) {
	if !refPattern.MatchString(target) {
		return t.querySelector(ctx, target)
	}
	t.s.mu.Lock()
	ref, ok := t.refs[target]
	f := t.frames[ref.frameID]
	if !ok || f == nil || f.sess == nil || f.loaderID != ref.loaderID {
		t.s.mu.Unlock()
		return nil, staleRefError(target)
	}
	el := &element{t: t, frame: f.ref(), backend: ref.backend, label: target}
	t.s.mu.Unlock()
	if ref.name != "" {
		el.label = fmt.Sprintf("%s %q (%s)", ref.role, ref.name, target)
	} else {
		el.label = fmt.Sprintf("%s (%s)", ref.role, target)
	}
	return el, nil
}

func (t *tab) querySelector(ctx context.Context, selector string) (*element, error) {
	f, ok := t.frameRef(t.mainFrameID())
	if !ok {
		return nil, errStale
	}
	var res evalResult
	err := t.sess.call(ctx, "Runtime.evaluate", obj{
		"expression": "document.querySelector(" + quoteJSON(selector) + ")",
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("%q is neither a ref from the snapshot (like e5) nor a valid CSS selector", selector)
	}
	if res.Result.ObjectID == "" {
		return nil, errNoMatch
	}
	var desc struct {
		Node struct {
			BackendNodeID int `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := t.sess.call(ctx, "DOM.describeNode", obj{"objectId": res.Result.ObjectID}, &desc); err != nil {
		return nil, err
	}
	return &element{t: t, frame: f, backend: desc.Node.BackendNodeID, label: quoteJSON(selector)}, nil
}

func (t *tab) mainFrameID() string {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.mainFrame
}

type evalResult struct {
	Result struct {
		Type     string          `json:"type"`
		Subtype  string          `json:"subtype"`
		Value    json.RawMessage `json:"value"`
		ObjectID string          `json:"objectId"`
	} `json:"result"`
	ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
}

// world returns the execution context of our isolated world in frame f,
// creating it when the frame has a new document.
func (t *tab) world(ctx context.Context, f frameRef) (int, error) {
	t.s.mu.Lock()
	if rec := t.frames[f.id]; rec != nil && rec.world != 0 && rec.worldLoader == f.loaderID {
		id := rec.world
		t.s.mu.Unlock()
		return id, nil
	}
	t.s.mu.Unlock()
	var res struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	err := f.sess.call(ctx, "Page.createIsolatedWorld", obj{"frameId": f.id, "worldName": isolatedWorld}, &res)
	if err != nil {
		return 0, err
	}
	t.s.mu.Lock()
	if rec := t.frames[f.id]; rec != nil {
		rec.world, rec.worldLoader = res.ExecutionContextID, f.loaderID
	}
	t.s.mu.Unlock()
	return res.ExecutionContextID, nil
}

// forgetWorld drops the cached isolated world of a frame after the browser
// reported it gone.
func (t *tab) forgetWorld(f frameRef) {
	t.s.mu.Lock()
	if rec := t.frames[f.id]; rec != nil {
		rec.world = 0
	}
	t.s.mu.Unlock()
}

// object returns a handle to the element in the isolated world.
func (el *element) object(ctx context.Context) (string, error) {
	if el.objectID != "" {
		return el.objectID, nil
	}
	for attempt := 0; ; attempt++ {
		world, err := el.t.world(ctx, el.frame)
		if err != nil {
			return "", err
		}
		var res struct {
			Object struct {
				ObjectID string `json:"objectId"`
			} `json:"object"`
		}
		err = el.frame.sess.call(ctx, "DOM.resolveNode", obj{"backendNodeId": el.backend, "executionContextId": world}, &res)
		if cdpErrorContains(err, "context") && attempt == 0 {
			el.t.forgetWorld(el.frame)
			continue
		}
		if err != nil {
			return "", err
		}
		el.objectID = res.Object.ObjectID
		return el.objectID, nil
	}
}

// release frees the element's handle in the page.
func (el *element) release() {
	if el.objectID == "" {
		return
	}
	id := el.objectID
	el.objectID = ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), actionRelease)
		defer cancel()
		el.frame.sess.call(ctx, "Runtime.releaseObject", obj{"objectId": id}, nil)
	}()
}

// call runs an injected helper operation with the element as its target and
// decodes the JSON result into out.
func (el *element) call(ctx context.Context, op string, arg, out any) error {
	objectID, err := el.object(ctx)
	if err != nil {
		return err
	}
	return callInjected(ctx, el.frame.sess, obj{"objectId": objectID}, op, arg, out)
}

// callFrame runs a document-level helper operation in frame f.
func (t *tab) callFrame(ctx context.Context, f frameRef, op string, arg, out any) error {
	world, err := t.world(ctx, f)
	if err != nil {
		return err
	}
	err = callInjected(ctx, f.sess, obj{"executionContextId": world}, op, arg, out)
	if cdpErrorContains(err, "context") {
		t.forgetWorld(f)
	}
	return err
}

func callInjected(ctx context.Context, sess *cdpSession, where obj, op string, arg, out any) error {
	where["functionDeclaration"] = injectedFunction
	where["arguments"] = []obj{{"value": op}, {"value": arg}}
	where["awaitPromise"] = true
	where["returnByValue"] = true
	var res evalResult
	if err := sess.call(ctx, "Runtime.callFunctionOn", where, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("page helper %q failed: %s", op, res.ExceptionDetails.message())
	}
	var failure struct {
		Error string `json:"error"`
	}
	json.Unmarshal(res.Result.Value, &failure)
	switch failure.Error {
	case "":
	case "notconnected":
		return errStale
	default:
		return errors.New(failure.Error)
	}
	if out != nil {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

// contentQuads returns the element's content boxes in the coordinate space
// of its process's outermost frame.
func contentQuads(ctx context.Context, sess *cdpSession, node obj) ([][4]point, error) {
	var res struct {
		Quads [][]float64 `json:"quads"`
	}
	err := sess.call(ctx, "DOM.getContentQuads", node, &res)
	if cdpErrorContains(err, "layout object") || cdpErrorContains(err, "Could not compute content quads") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var quads [][4]point
	for _, q := range res.Quads {
		if len(q) == 8 {
			quads = append(quads, [4]point{{q[0], q[1]}, {q[2], q[3]}, {q[4], q[5]}, {q[6], q[7]}})
		}
	}
	return quads, nil
}

// frameOrigin returns where frame f's viewport sits inside the page's
// top-level viewport.
func (t *tab) frameOrigin(ctx context.Context, f frameRef) (point, int, error) {
	if f.parentID == "" {
		return point{}, 0, nil
	}
	parent, ok := t.frameRef(f.parentID)
	if !ok {
		return point{}, 0, errStale
	}
	var owner struct {
		BackendNodeID int `json:"backendNodeId"`
	}
	if err := parent.sess.call(ctx, "DOM.getFrameOwner", obj{"frameId": f.id}, &owner); err != nil {
		return point{}, 0, err
	}
	quads, err := contentQuads(ctx, parent.sess, obj{"backendNodeId": owner.BackendNodeID})
	if err != nil {
		return point{}, 0, err
	}
	if len(quads) == 0 {
		return point{}, 0, errors.New("the frame containing the element is not visible")
	}
	base, err := t.rootOrigin(ctx, parent)
	if err != nil {
		return point{}, 0, err
	}
	return point{quads[0][0].x + base.x, quads[0][0].y + base.y}, owner.BackendNodeID, nil
}

// rootOrigin returns the top-level position of the coordinate space that
// protocol geometry for frame f is reported in: the viewport of the
// outermost frame rendered by the same process.
func (t *tab) rootOrigin(ctx context.Context, f frameRef) (point, error) {
	for f.parentID != "" {
		parent, ok := t.frameRef(f.parentID)
		if !ok {
			return point{}, errStale
		}
		if parent.sess != f.sess {
			origin, _, err := t.frameOrigin(ctx, f)
			return origin, err
		}
		f = parent
	}
	return point{}, nil
}

// viewportSize returns the size of the page's top-level viewport in CSS
// pixels.
func (t *tab) viewportSize(ctx context.Context) (w, h float64, err error) {
	var m struct {
		CSSLayoutViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := t.sess.call(ctx, "Page.getLayoutMetrics", nil, &m); err != nil {
		return 0, 0, err
	}
	return m.CSSLayoutViewport.ClientWidth, m.CSSLayoutViewport.ClientHeight, nil
}
