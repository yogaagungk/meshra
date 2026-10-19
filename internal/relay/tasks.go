package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"meshra/internal/model"
)

const queueLimit = 128
const globalQueueLimit = 4096

func (r *Relay) task(id string) (model.Task, error) {
	var t model.Task
	var b string
	err := r.db.QueryRow("SELECT body FROM tasks WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return t, model.Err(404, "task not found: "+id)
	}
	if err != nil {
		return t, err
	}
	err = json.Unmarshal([]byte(b), &t)
	return t, err
}
func (r *Relay) tasks() ([]model.Task, error) {
	rows, err := r.db.Query("SELECT body FROM tasks WHERE state IN ('queued','dispatching','running') ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Task{}
	for rows.Next() {
		var b string
		var t model.Task
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (r *Relay) transition(t model.Task, kind, data string) error {
	t.UpdatedAt = now()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE tasks SET body=?,state=? WHERE id=?", string(b), t.State, t.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO events(task_id,kind,data,time) VALUES(?,?,?,?)", t.ID, kind, data, t.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil && (t.State == "queued" || model.Terminal(t.State)) {
		r.signal()
	}
	return err
}
func (r *Relay) access(p model.Principal, t model.Task, op string) error {
	if err := r.permit(p, op); err != nil {
		return err
	}
	if p.Admin {
		return nil
	}
	if (t.From == p.AgentID && t.FromSessionID == p.SessionID) || (t.OriginAgentID == p.AgentID && t.OriginSessionID == p.SessionID) || (t.To == p.AgentID && t.DestinationSessionID == p.SessionID && p.SessionID != "") {
		return nil
	}
	return model.Err(403, "task belongs to a different agent or session")
}
func (r *Relay) Send(p model.Principal, q model.SendRequest) (model.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var t model.Task
	if r.closed {
		return t, model.Err(503, "relay is shutting down")
	}
	if q.Type == "" {
		q.Type = "send"
	}
	if q.Type != "send" && q.Type != "request" {
		return t, model.Err(400, "type must be send or request")
	}
	if err := r.permit(p, q.Type); err != nil {
		return t, err
	}
	from := p.AgentID
	if p.Admin {
		from = q.From
	} else if q.From != "" && q.From != p.AgentID {
		return t, model.Err(403, "cannot impersonate another agent")
	}
	a, err := r.agent(from)
	if err != nil {
		return t, err
	}
	if _, err = r.agent(q.To); err != nil {
		return t, err
	}
	session := p.SessionID
	if p.Admin {
		session = a.SessionID
	}
	if p.TaskID != "" {
		parent, e := r.task(p.TaskID)
		if e != nil || model.Terminal(parent.State) || parent.To != from {
			return t, model.Err(401, "authenticated turn is no longer active")
		}
		if parent.DestinationSessionID == "" {
			return t, model.Err(409, "native session allocation has not completed; retry after the session event")
		}
		session = parent.DestinationSessionID
	}
	if !p.Admin && !contains(a.Policy.Destinations, q.To) {
		return t, model.Err(403, "destination not permitted: "+q.To)
	}
	if strings.TrimSpace(q.Text) == "" || !utf8.ValidString(q.Text) {
		return t, model.Err(400, "message must contain valid nonempty UTF-8 text")
	}
	if len(q.Text) > model.MaxTextBytes {
		return t, model.Err(413, "message exceeds 65536 bytes")
	}
	if len(q.IdempotencyKey) > 200 || strings.ContainsAny(q.IdempotencyKey, "\x00\r\n") {
		return t, model.Err(400, "invalid idempotency key")
	}
	if q.TimeoutSeconds == 0 {
		q.TimeoutSeconds = 300
	}
	if q.TimeoutSeconds < 1 || q.TimeoutSeconds > 86400 {
		return t, model.Err(400, "timeoutSeconds must be 1..86400")
	}
	maxHopsSpecified := q.MaxHops != 0
	if q.MaxHops == 0 {
		q.MaxHops = 4
	}
	if q.MaxHops < 1 || q.MaxHops > 16 {
		return t, model.Err(400, "maxHops must be 1..16")
	}
	if p.TaskID != "" {
		if q.ParentTaskID != "" && q.ParentTaskID != p.TaskID {
			return t, model.Err(403, "parent task does not match authenticated turn")
		}
		q.ParentTaskID = p.TaskID
	}
	t = model.Task{SchemaVersion: 1, ID: randomID(), MessageID: randomID(), CorrelationID: randomID(), OriginAgentID: from, OriginSessionID: session, From: from, FromSessionID: session, To: q.To, HopCount: 1, MaxHops: q.MaxHops, Type: q.Type, Text: q.Text, State: "queued", IdempotencyKey: q.IdempotencyKey, CreatedAt: now(), UpdatedAt: now()}
	t.Deadline = t.CreatedAt.Add(time.Duration(q.TimeoutSeconds) * time.Second)
	if q.ParentTaskID != "" {
		if !p.Admin && !a.Policy.Delegate {
			return model.Task{}, model.Err(403, "delegation not permitted")
		}
		parent, e := r.task(q.ParentTaskID)
		if e != nil {
			return model.Task{}, e
		}
		if parent.To != from || parent.DestinationSessionID != session {
			return model.Task{}, model.Err(403, "parent task is not owned by this destination session")
		}
		t.ParentTaskID = parent.ID
		t.CorrelationID = parent.CorrelationID
		t.OriginAgentID = parent.OriginAgentID
		t.OriginSessionID = parent.OriginSessionID
		t.HopCount = parent.HopCount + 1
		t.MaxHops = parent.MaxHops
		if maxHopsSpecified && q.MaxHops < t.MaxHops {
			t.MaxHops = q.MaxHops
		}
		if t.HopCount > t.MaxHops {
			return model.Task{}, model.Err(409, "delegation hop limit exceeded")
		}
		visited := map[string]bool{from: true}
		for cur := parent; ; {
			visited[cur.From] = true
			visited[cur.To] = true
			if cur.ParentTaskID == "" {
				break
			}
			cur, e = r.task(cur.ParentTaskID)
			if e != nil {
				return model.Task{}, e
			}
			if len(visited) > 32 {
				return model.Task{}, model.Err(409, "invalid task ancestry")
			}
		}
		if visited[q.To] {
			return model.Task{}, model.Err(409, "delegation cycle rejected; replies use task results, not a new request to an ancestor")
		}
	}
	if from == q.To {
		return model.Task{}, model.Err(409, "self-routing cycle rejected")
	}
	if q.IdempotencyKey != "" {
		var b string
		e := r.db.QueryRow("SELECT body FROM tasks WHERE sender=? AND sender_session=? AND idempotency_key=?", from, session, q.IdempotencyKey).Scan(&b)
		if e == nil {
			var old model.Task
			if e = json.Unmarshal([]byte(b), &old); e != nil {
				return model.Task{}, e
			}
			if old.To != t.To || old.Text != t.Text || old.Type != t.Type || old.ParentTaskID != t.ParentTaskID {
				return model.Task{}, model.Err(409, "idempotency key already used for a different request")
			}
			return old, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return model.Task{}, e
		}
	}
	all, err := r.tasks()
	if err != nil {
		return model.Task{}, err
	}
	n, total := 0, 0
	for _, old := range all {
		if !model.Terminal(old.State) {
			total++
			if old.To == q.To {
				n++
			}
		}
	}
	if n >= queueLimit || total >= globalQueueLimit {
		return model.Task{}, model.Err(429, "persistent inbox capacity reached")
	}
	b, _ := json.Marshal(t)
	var key any
	if q.IdempotencyKey != "" {
		key = q.IdempotencyKey
	}
	tx, err := r.db.Begin()
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO tasks(id,sender,sender_session,idempotency_key,state,body) VALUES(?,?,?,?,?,?)", t.ID, from, session, key, t.State, string(b)); err != nil {
		return model.Task{}, err
	}
	if _, err = tx.Exec("INSERT INTO events(task_id,kind,data,time) VALUES(?,?,?,?)", t.ID, "queued", "durably accepted; destination may be offline", now().Format(time.RFC3339Nano)); err != nil {
		return model.Task{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Task{}, err
	}
	r.signal()
	return t, nil
}
func (r *Relay) Task(p model.Principal, id string) (model.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.task(id)
	if err != nil {
		return t, err
	}
	if err = r.access(p, t, "status"); err != nil {
		return model.Task{}, err
	}
	return t, nil
}
func (r *Relay) Events(p model.Principal, id string, after int64) ([]model.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.task(id)
	if err != nil {
		return nil, err
	}
	if err = r.access(p, t, "events"); err != nil {
		return nil, err
	}
	rows, err := r.db.Query("SELECT id,kind,data,time FROM events WHERE task_id=? AND id>? ORDER BY id LIMIT 256", id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		e := model.Event{TaskID: id}
		var tm string
		if err = rows.Scan(&e.ID, &e.Type, &e.Data, &tm); err != nil {
			return nil, err
		}
		e.Time, err = time.Parse(time.RFC3339Nano, tm)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (r *Relay) Cancel(p model.Principal, id string) (model.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.task(id)
	if err != nil {
		return t, err
	}
	if err = r.access(p, t, "cancel"); err != nil {
		return model.Task{}, err
	}
	if model.Terminal(t.State) {
		return t, nil
	}
	t.State = "cancelled"
	t.Error = "cancelled by caller; started execution may have external effects"
	err = r.transition(t, "cancelled", t.Error)
	if s := r.slots[t.To]; s != nil && s.taskID == id && s.turnCancel != nil {
		s.turnCancel()
	}
	return t, err
}
func (r *Relay) Retry(p model.Principal, id string) (model.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := requireAdmin(p); err != nil {
		return model.Task{}, err
	}
	t, err := r.task(id)
	if err != nil {
		return t, err
	}
	if t.State != "failed" && t.State != "timed_out" && t.State != "cancelled" {
		return model.Task{}, model.Err(409, "only failed, timed_out or cancelled tasks may be manually retried")
	}
	if t.Attempts >= 3 {
		return model.Task{}, model.Err(409, "manual retry limit reached; inspect external effects before creating new work")
	}
	if s := r.slots[t.To]; s != nil && s.taskID == id {
		return model.Task{}, model.Err(409, "previous execution is still stopping")
	}
	all, err := r.tasks()
	if err != nil {
		return t, err
	}
	n, total := 0, 0
	for _, v := range all {
		if !model.Terminal(v.State) {
			total++
			if v.To == t.To {
				n++
			}
		}
	}
	if n >= queueLimit || total >= globalQueueLimit {
		return model.Task{}, model.Err(429, "persistent inbox capacity reached")
	}
	if _, err = r.agent(t.To); err != nil {
		return model.Task{}, err
	}
	t.State = "queued"
	t.Error = ""
	t.Result = ""
	t.Deadline = now().Add(300 * time.Second)
	err = r.transition(t, "queued", "explicit manual retry; execution may repeat external effects")
	return t, err
}
func (r *Relay) recover() error {
	agents, err := r.agents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.State == "starting" || a.State == "busy" || a.State == "idle" {
			a.State = "stopped"
			a.Error = "relay restarted; start explicitly to resume the owned session"
			if err = r.saveAgent(a); err != nil {
				return err
			}
		}
	}
	ts, err := r.tasks()
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.State == "dispatching" || t.State == "running" {
			t.State = "failed"
			t.Error = "relay interrupted execution; outcome uncertain; inspect native session then retry manually"
			if err = r.transition(t, "failed", t.Error); err != nil {
				return err
			}
		}
	}
	_, err = r.db.Exec("DELETE FROM credentials WHERE task_id<>''")
	return err
}

// signal coalesces scheduling hints; SQLite remains the source of truth.
func (r *Relay) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Relay) schedule() {
	defer r.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
		case <-r.wake:
		}
		timer.Reset(r.tick())
	}
}

// tick returns the next durable deadline; admission, lifecycle and completion
// wake the scheduler directly, so offline/idle queues need no periodic scans.
func (r *Relay) tick() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	delay := time.Hour
	if r.closed {
		return delay
	}
	ts, err := r.tasks()
	if err != nil {
		return time.Second
	}
	for _, t := range ts {
		remaining := time.Until(t.Deadline)
		if remaining <= 0 {
			t.State = "timed_out"
			t.Error = "task deadline exceeded (includes time queued/offline)"
			if r.transition(t, "timed_out", t.Error) != nil {
				return time.Second
			}
			if s := r.slots[t.To]; s != nil && s.taskID == t.ID && s.turnCancel != nil {
				s.turnCancel()
			}
			continue
		}
		delay = min(delay, remaining)
		if t.State != "queued" {
			continue
		}
		s := r.slots[t.To]
		if s == nil || s.runtime == nil || s.taskID != "" || r.repos[s.repo] {
			continue
		}
		a, e := r.agent(t.To)
		if e != nil || a.State != "idle" {
			continue
		}
		t.State = "dispatching"
		t.Attempts++
		t.DestinationSessionID = s.runtime.SessionID()
		if e = r.transition(t, "dispatching", "turn slot and repository lock acquired"); e != nil {
			return time.Second
		}
		a.State = "busy"
		if e = r.saveAgent(a); e != nil {
			return time.Second
		}
		s.taskID = t.ID
		r.repos[s.repo] = true
		ctx, cancel := context.WithDeadline(r.ctx, t.Deadline)
		s.turnCancel = cancel
		token, e := r.issue(a.ID, t.DestinationSessionID, t.ID)
		if e != nil {
			cancel()
			t.State = "failed"
			t.Error = "failed to issue turn credential"
			_ = r.transition(t, "failed", t.Error)
			s.taskID = ""
			s.turnCancel = nil
			delete(r.repos, s.repo)
			a.State = "error"
			_ = r.saveAgent(a)
			continue
		}
		r.wg.Add(1)
		go r.execute(ctx, a, s, t, token, r.mcpURL)
	}
	return delay
}
func (r *Relay) execute(ctx context.Context, a model.Agent, s *slot, t model.Task, token, url string) {
	defer r.wg.Done()
	defer r.signal()
	r.mu.Lock()
	current, e := r.task(t.ID)
	if e != nil {
		r.mu.Unlock()
		return
	}
	if !model.Terminal(current.State) {
		current.State = "running"
		e = r.transition(current, "running", "harness turn started")
	}
	r.mu.Unlock()
	var result string
	var runErr error
	eventCount := 0
	if e == nil && !model.Terminal(current.State) {
		result, runErr = s.runtime.Run(ctx, model.Turn{Task: current, Token: token, MCPURL: url}, func(kind, data string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			task, err := r.task(t.ID)
			if err != nil || model.Terminal(task.State) {
				return
			}
			if kind == "session" {
				a, err := r.agent(t.To)
				if err == nil {
					a.SessionID = data
					_ = r.saveAgent(a)
				}
				task.DestinationSessionID = data
				_ = r.transition(task, "session", data)
				_, _ = r.db.Exec("UPDATE credentials SET session_id=? WHERE hash=?", data, tokenHash(token))
				return
			}
			if eventCount >= 10000 {
				return
			}
			eventCount++
			if len(data) > 16384 {
				data = truncate(data, 16384) + " [event truncated]"
			}
			_ = r.transition(task, kind, data)
		})
	} else {
		runErr = e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ctxErr := ctx.Err()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	delete(r.repos, s.repo)
	current, e = r.task(t.ID)
	if e == nil && !model.Terminal(current.State) {
		switch {
		case r.closed:
			current.State = "failed"
			current.Error = "relay shutdown interrupted execution; outcome uncertain; retry manually"
		case ctxErr == context.DeadlineExceeded:
			current.State = "timed_out"
			current.Error = "task deadline exceeded"
		case ctxErr != nil:
			current.State = "cancelled"
			current.Error = "turn cancelled; execution may have external effects"
		case runErr != nil:
			current.State = "failed"
			current.Error = "harness turn failed: " + truncate(runErr.Error(), 2048)
		case len(result) > 256*1024:
			current.State = "failed"
			current.Error = "harness response exceeds 262144 bytes"
		default:
			current.State = "completed"
			current.Result = result
		}
		if sid := s.runtime.SessionID(); sid != "" {
			current.DestinationSessionID = sid
		}
		_ = r.transition(current, current.State, current.Error)
	}
	_, _ = r.db.Exec("DELETE FROM credentials WHERE hash=?", tokenHash(token))
	s.taskID = ""
	s.turnCancel = nil
	if r.slots[a.ID] == s {
		latest, err := r.agent(a.ID)
		if err == nil {
			latest.SessionID = s.runtime.SessionID()
			if latest.State == "busy" {
				if runErr != nil || ctxErr != nil {
					latest.State = "error"
					latest.Error = "harness turn failed or was interrupted; inspect task status and restart agent before dispatching more work"
				} else {
					latest.State = "idle"
				}
			}
			_ = r.saveAgent(latest)
		}
	}
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

var _ model.Service = (*Relay)(nil)
