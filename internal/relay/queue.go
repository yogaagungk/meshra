package relay

import (
	"database/sql"
	"encoding/json"
	"errors"

	"meshra/internal/model"
)

type queueView struct {
	AgentID   string       `json:"agentId"`
	ProfileID string       `json:"profileId,omitempty"`
	Paused    bool         `json:"paused"`
	Revision  int          `json:"revision"`
	Tasks     []model.Task `json:"tasks"`
}

type dispatchPriority struct{ rank, seq int64 }
type recipientTaskOrder struct {
	tasks      []model.Task
	priorities []dispatchPriority
	positions  []int
}

func (q recipientTaskOrder) Len() int { return len(q.positions) }
func (q recipientTaskOrder) Less(i, j int) bool {
	a, b := q.priorities[q.positions[i]], q.priorities[q.positions[j]]
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return a.seq < b.seq
}
func (q recipientTaskOrder) Swap(i, j int) {
	a, b := q.positions[i], q.positions[j]
	q.tasks[a], q.tasks[b] = q.tasks[b], q.tasks[a]
	q.priorities[a], q.priorities[b] = q.priorities[b], q.priorities[a]
}

func (r *Relay) pausedQueues() (map[string]bool, error) {
	rows, err := r.db.Query("SELECT agent_id FROM agent_queue_state WHERE paused=1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paused := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		paused[id] = true
	}
	return paused, rows.Err()
}

func touchQueue(tx *sql.Tx, agentID string) error {
	_, err := tx.Exec("INSERT INTO agent_queue_state(agent_id,paused,revision) VALUES(?,0,1) ON CONFLICT(agent_id) DO UPDATE SET revision=revision+1", agentID)
	if err != nil {
		return err
	}
	return insertAutomationEvent(tx, "queue.changed", agentID, "", nil)
}

func (r *Relay) queueState(agentID string) (queueView, error) {
	v := queueView{AgentID: agentID, Tasks: []model.Task{}}
	if _, err := r.agent(agentID); err != nil {
		return v, err
	}
	err := r.db.QueryRow("SELECT paused,revision FROM agent_queue_state WHERE agent_id=?", agentID).Scan(&v.Paused, &v.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	err = r.db.QueryRow("SELECT profile_id FROM profile_bindings WHERE agent_id=?", agentID).Scan(&v.ProfileID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	rows, err := r.db.Query(`SELECT t.body FROM tasks t LEFT JOIN task_dispatch_order d ON d.task_id=t.id WHERE t.recipient=? AND t.state='queued' ORDER BY coalesce(d.rank,t.seq),t.seq`, agentID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		var t model.Task
		if err = rows.Scan(&body); err != nil {
			return v, err
		}
		if err = json.Unmarshal([]byte(body), &t); err != nil {
			return v, err
		}
		v.Tasks = append(v.Tasks, t)
	}
	return v, rows.Err()
}

func (r *Relay) queuePaused(agentID string) (bool, error) {
	var paused bool
	err := r.db.QueryRow("SELECT paused FROM agent_queue_state WHERE agent_id=?", agentID).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return paused, err
}

func (r *Relay) queueOperation(p model.Principal, op string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is shutting down")
	}
	if op == "queue.list" {
		var q struct {
			AgentID   string `json:"agentId"`
			ProfileID string `json:"profileId"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if q.ProfileID != "" {
			if _, err := r.profile(q.ProfileID); err != nil {
				return nil, err
			}
		}
		if q.AgentID != "" {
			if _, err := r.agent(q.AgentID); err != nil {
				return nil, err
			}
		}
		rows, err := r.db.Query(`SELECT a.id FROM agents a WHERE (?='' OR a.id=?) AND (?='' OR EXISTS(SELECT 1 FROM profile_bindings b WHERE b.agent_id=a.id AND b.profile_id=?)) ORDER BY a.id`, q.AgentID, q.AgentID, q.ProfileID, q.ProfileID)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		items := make([]queueView, 0, len(ids))
		for _, id := range ids {
			v, err := r.queueState(id)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return map[string]any{"items": items}, nil
	}
	var q struct {
		AgentID          string   `json:"agentId"`
		ExpectedRevision *int     `json:"expectedRevision"`
		TaskIDs          []string `json:"taskIds,omitempty"`
	}
	if op == "queue.reorder" {
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
	} else {
		var request struct {
			AgentID          string `json:"agentId"`
			ExpectedRevision *int   `json:"expectedRevision"`
		}
		if err := decodeAutomation(raw, &request); err != nil {
			return nil, err
		}
		q.AgentID = request.AgentID
		q.ExpectedRevision = request.ExpectedRevision
	}
	if q.ExpectedRevision == nil {
		return nil, model.Err(400, "expectedRevision is required")
	}
	state, err := r.queueState(q.AgentID)
	if err != nil {
		return nil, err
	}
	if state.Revision != *q.ExpectedRevision {
		return nil, model.Err(409, "queue changed; reload before applying controls")
	}
	if op == "queue.reorder" {
		if len(q.TaskIDs) != len(state.Tasks) {
			return nil, model.Err(409, "reorder must name the exact current queued set")
		}
		current := make(map[string]bool, len(state.Tasks))
		for _, t := range state.Tasks {
			current[t.ID] = true
		}
		for _, id := range q.TaskIDs {
			if !current[id] {
				return nil, model.Err(409, "reorder must name each queued task exactly once")
			}
			delete(current, id)
		}
	} else if op != "queue.pause" && op != "queue.resume" {
		return nil, model.Err(404, "unknown queue operation")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if op == "queue.reorder" {
		for i, id := range q.TaskIDs {
			if _, err = tx.Exec("INSERT INTO task_dispatch_order(task_id,agent_id,rank) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET rank=excluded.rank", id, q.AgentID, i+1); err != nil {
				return nil, err
			}
		}
	} else {
		state.Paused = op == "queue.pause"
	}
	state.Revision++
	if _, err = tx.Exec("INSERT INTO agent_queue_state(agent_id,paused,revision) VALUES(?,?,?) ON CONFLICT(agent_id) DO UPDATE SET paused=excluded.paused,revision=excluded.revision", q.AgentID, state.Paused, state.Revision); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, op, q.AgentID, state.ProfileID, map[string]any{"revision": state.Revision, "paused": state.Paused}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	r.signal()
	return r.queueState(q.AgentID)
}
