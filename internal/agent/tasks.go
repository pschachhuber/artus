package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/store"
)

// Fire handles a trigger coming due: it starts a task run or wakes a chat.
func (e *Engine) Fire(t sched.Trigger) {
	kind, id, _ := strings.Cut(t.Target, ":")
	reason := "Trigger " + t.ID + " (" + sched.Describe(t.When) + ") fired."
	if t.Note != "" {
		reason += " Note left when it was set: " + t.Note
	}
	switch kind {
	case "task":
		if _, err := e.RunTask(id, reason); err != nil {
			e.emit("", "alert", fmt.Sprintf("Task %s did not start: %v", id, err))
		}
	case "chat":
		meta, err := e.Home.Chat(id)
		if err != nil {
			e.Sched.RemoveTarget(t.Target)
			return
		}
		meta.Unread = true
		e.Home.SaveChat(meta)
		if err := e.Send(id, Input{Text: "<harness>" + reason + "</harness>", Synthetic: true}); err != nil {
			e.emit(id, "alert", "Wake-up failed: "+err.Error())
		}
	}
	e.emit("", "schedule", nil)
}

// RunTask starts one unattended run of a task in a fresh context and returns
// the id of the chat that holds its transcript.
func (e *Engine) RunTask(taskID, reason string) (string, error) {
	task, err := e.Home.Task(taskID)
	if err != nil {
		e.Sched.RemoveTarget("task:" + taskID)
		return "", fmt.Errorf("no such task")
	}
	if task.Status != "active" {
		return "", fmt.Errorf("task is %s", task.Status)
	}
	for _, id := range e.Running() {
		if meta, err := e.Home.Chat(id); err == nil && meta.Kind == store.KindTask && meta.Task == taskID {
			return "", fmt.Errorf("the previous run is still going")
		}
	}
	cfg := e.Home.Config()
	meta := store.ChatMeta{
		Kind: store.KindTask, Task: taskID, Unread: true,
		Title: task.Name + " — " + time.Now().Format("2 Jan 15:04"),
		Model: firstNonEmpty(task.Model, cfg.Model), Effort: firstNonEmpty(task.Effort, cfg.Effort),
	}
	meta, err = e.NewChat(meta)
	if err != nil {
		return "", err
	}
	notes := strings.TrimSpace(task.Notes)
	if notes == "" {
		notes = "(empty: this is the first run. If the task should repeat, set its triggers with the schedule tool now.)"
	}
	text := fmt.Sprintf("<harness>%s</harness>\n\n# tasks/%s/TASK.md\n%s\n\n# tasks/%s/NOTES.md\n%s",
		reason, taskID, task.Description, taskID, notes)
	if err := e.Send(meta.ID, Input{Text: text, Synthetic: true}); err != nil {
		return "", err
	}
	e.emit("", "tasks", nil)
	return meta.ID, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Wait blocks until a chat's run has ended. It is used by tests and by the
// one-shot command-line mode.
func (e *Engine) Wait(ctx context.Context, chatID string) {
	for {
		e.mu.Lock()
		r := e.runs[chatID]
		e.mu.Unlock()
		if r == nil {
			return
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			return
		}
	}
}
