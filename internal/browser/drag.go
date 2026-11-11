package browser

import (
	"context"
	"encoding/json"
	"time"
)

// drag presses the mouse on one element and releases it on another. Native
// HTML drag-and-drop is driven through the protocol's drag interception;
// pages that implement dragging with mouse events just see the mouse.
func (t *tab) drag(ctx context.Context, from, to string, timeout time.Duration) (fromLabel, toLabel string, err error) {
	var source frameRef
	fromLabel, err = t.pointer(ctx, from, timeout, false, func(ctx context.Context, el *element, pt point) error {
		source = el.frame
		return t.mouseMove(ctx, pt)
	})
	if err != nil {
		return fromLabel, to, err
	}
	if err := t.callFrame(ctx, source, "armDrag", nil, nil); err != nil {
		return fromLabel, to, err
	}
	if err := t.mouseDown(ctx, "left", 1); err != nil {
		return fromLabel, to, err
	}
	var end point
	toLabel, err = t.pointer(ctx, to, timeout, false, func(_ context.Context, _ *element, pt point) error {
		end = pt
		return nil
	})
	if err != nil {
		t.mouseUp(ctx, "left", 1)
		return fromLabel, toLabel, err
	}
	t.s.mu.Lock()
	t.dragData = nil
	t.s.mu.Unlock()
	t.sess.call(ctx, "Input.setInterceptDrags", obj{"enabled": true}, nil)
	err = t.mouseMove(ctx, end)
	var started struct {
		Drag bool `json:"drag"`
	}
	if err == nil {
		err = t.callFrame(ctx, source, "dragStarted", nil, &started)
	}
	var data json.RawMessage
	if err == nil && started.Drag {
		err = t.s.wait(ctx, func() bool { data = t.dragData; return data != nil })
	}
	t.sess.call(ctx, "Input.setInterceptDrags", obj{"enabled": false}, nil)
	if err != nil || data == nil {
		if err == nil {
			err = t.mouseMove(ctx, end)
		}
		t.mouseUp(ctx, "left", 1)
		return fromLabel, toLabel, err
	}
	t.buttons = 0
	for _, kind := range []string{"dragEnter", "dragOver", "drop"} {
		params := obj{"type": kind, "x": end.x, "y": end.y, "data": data, "modifiers": t.modifiers}
		if err := t.sess.call(ctx, "Input.dispatchDragEvent", params, nil); err != nil {
			return fromLabel, toLabel, err
		}
	}
	return fromLabel, toLabel, nil
}
