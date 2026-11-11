package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pschachhuber/artus/internal/agent"
)

// hub fans live events out to connected interfaces over server-sent events.
// Every event gets a sequence number and the most recent ones are kept, so a
// client that reconnects resumes where it left off.
type hub struct {
	mu     sync.Mutex
	seq    int64
	recent []agent.Event
	subs   map[chan agent.Event]bool
}

const hubBacklog = 4000

func newHub() *hub { return &hub{subs: map[chan agent.Event]bool{}} }

func (h *hub) publish(ev agent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	h.recent = append(h.recent, ev)
	if len(h.recent) > hubBacklog {
		h.recent = append(h.recent[:0:0], h.recent[len(h.recent)-hubBacklog/2:]...)
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // too slow: drop the subscriber, it will reconnect and resync
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *hub) current() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// subscribe returns the events after since that are still buffered, and a
// channel of later ones. ok is false when since is too old to resume from.
func (h *hub) subscribe(since int64) (backlog []agent.Event, ch chan agent.Event, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ok = true
	if since > 0 && since < h.seq {
		if len(h.recent) == 0 || h.recent[0].Seq > since+1 {
			ok = false
		} else {
			for _, ev := range h.recent {
				if ev.Seq > since {
					backlog = append(backlog, ev)
				}
			}
		}
	}
	ch = make(chan agent.Event, 1024)
	h.subs[ch] = true
	return backlog, ch, ok
}

func (h *hub) unsubscribe(ch chan agent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[ch] {
		delete(h.subs, ch)
		close(ch)
	}
}

// serve streams events to one client until it disconnects.
func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	since, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q := r.URL.Query().Get("since"); q != "" {
		since, _ = strconv.ParseInt(q, 10, 64)
	}
	backlog, ch, ok := h.subscribe(since)
	defer h.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(ev agent.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
	}
	if !ok {
		fmt.Fprint(w, "event: resync\ndata: {}\n\n") // too far behind: reload state
	}
	for _, ev := range backlog {
		send(ev)
	}
	fmt.Fprintf(w, "event: ready\ndata: {\"seq\":%d}\n\n", h.current())
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case ev, open := <-ch:
			if !open {
				return
			}
			send(ev)
			for more := true; more; { // drain what is already queued before flushing
				select {
				case ev, open := <-ch:
					if !open {
						flusher.Flush()
						return
					}
					send(ev)
				default:
					more = false
				}
			}
		}
		flusher.Flush()
	}
}
