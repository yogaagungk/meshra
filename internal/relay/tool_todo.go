package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"meshra/internal/model"
)

type toolTodoArgs struct {
	Action string `json:"action"`
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Status string `json:"status,omitempty"`
}
type todoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

func validateTodo(q *toolTodoArgs) error {
	if q.Action != "add" && q.Action != "update" && q.Action != "list" {
		return model.Err(400, "todo action must be add, update, or list")
	}
	if len(q.ID) > 64 || len(q.Text) > 1024 || (q.Status != "" && q.Status != "pending" && q.Status != "in_progress" && q.Status != "done") {
		return model.Err(400, "invalid todo item")
	}
	if q.Action == "add" && strings.TrimSpace(q.Text) == "" {
		return model.Err(400, "todo text is required")
	}
	if q.Action == "update" && (q.ID == "" || (q.Text == "" && q.Status == "")) {
		return model.Err(400, "todo update requires id and a change")
	}
	return nil
}

func (b *toolBroker) todoForTool(ctx context.Context, q toolTodoArgs) (model.ToolResult, error) {
	r := b.relay
	r.mu.Lock()
	defer r.mu.Unlock()
	task, _, _, err := b.activeLocked()
	if err != nil {
		return model.ToolResult{}, err
	}
	var items []todoItem
	var data string
	err = r.db.QueryRowContext(ctx, "SELECT data FROM events WHERE task_id=? AND kind='todo' ORDER BY id DESC LIMIT 1", task.ID).Scan(&data)
	if err == nil {
		if err = json.Unmarshal([]byte(data), &items); err != nil {
			return model.ToolResult{}, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.ToolResult{}, err
	}
	switch q.Action {
	case "add":
		if len(items) >= 100 {
			return model.ToolResult{}, model.Err(400, "todo list is full")
		}
		status := q.Status
		if status == "" {
			status = "pending"
		}
		items = append(items, todoItem{ID: randomID(), Text: q.Text, Status: status})
	case "update":
		found := false
		for i := range items {
			if items[i].ID == q.ID {
				found = true
				if q.Text != "" {
					items[i].Text = q.Text
				}
				if q.Status != "" {
					items[i].Status = q.Status
				}
				break
			}
		}
		if !found {
			return model.ToolResult{}, model.Err(404, "todo item not found")
		}
	}
	if items == nil {
		items = []todoItem{}
	}
	if q.Action != "list" {
		body, err := json.Marshal(items)
		if err != nil {
			return model.ToolResult{}, err
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return model.ToolResult{}, err
		}
		defer tx.Rollback()
		task.UpdatedAt = now()
		if err = insertEvent(tx, task, "todo", string(body)); err != nil {
			return model.ToolResult{}, err
		}
		if err = tx.Commit(); err != nil {
			return model.ToolResult{}, err
		}
	}
	return toolJSON(items)
}
