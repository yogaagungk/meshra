package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
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
	rows, err := r.db.Query("SELECT t.body,coalesce(d.rank,t.seq),t.seq FROM tasks t LEFT JOIN task_dispatch_order d ON d.task_id=t.id WHERE t.state IN ('queued','dispatching','running') ORDER BY t.seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Task{}
	priorities := []dispatchPriority{}
	recipients := map[string][]int{}
	for rows.Next() {
		var b string
		var t model.Task
		var priority dispatchPriority
		if err = rows.Scan(&b, &priority.rank, &priority.seq); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &t); err != nil {
			return nil, err
		}
		if t.State == "queued" {
			recipients[t.To] = append(recipients[t.To], len(out))
		}
		priorities = append(priorities, priority)
		out = append(out, t)
	}
	for _, positions := range recipients {
		if len(positions) > 1 {
			sort.Sort(recipientTaskOrder{tasks: out, priorities: priorities, positions: positions})
		}
	}
	return out, rows.Err()
}
func (r *Relay) transition(t model.Task, kind, data string) error {
	t.UpdatedAt = now()
	t.Text = redact(t.Text)
	t.Result = redact(t.Result)
	t.Error = redact(t.Error)
	if model.Terminal(t.State) {
		t.LeaseOwner = ""
		t.LeaseUntil = time.Time{}
		t.NextAttemptAt = time.Time{}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err = tx.QueryRow("SELECT state FROM tasks WHERE id=?", t.ID).Scan(&old); err != nil {
		return err
	}
	if !allowedTransition(old, t.State) && !(t.Chat && kind == "regenerated" && old == "completed" && t.State == "queued") {
		return model.Err(409, "invalid task transition: "+old+" -> "+t.State)
	}
	if _, err = tx.Exec("UPDATE tasks SET body=?,state=? WHERE id=? AND state=?", string(b), t.State, t.ID, old); err != nil {
		return err
	}
	if (old == "queued") != (t.State == "queued") {
		if err = touchQueue(tx, t.To); err != nil {
			return err
		}
	}
	if t.ConversationID != "" && model.Terminal(t.State) {
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,actor,operation,resource,time) VALUES(?,?,?,?,?)", t.ConversationID, t.To, "task."+t.State, t.ID, now().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if model.Terminal(t.State) {
		if err = invalidateToolTaskTx(tx, t.ID, "task became terminal"); err != nil {
			return err
		}
	}
	if err = insertEvent(tx, t, kind, data); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil {
		if kind == t.State || kind == "retry_scheduled" || kind == "delivered" {
			r.logTransition(t, kind)
		}
		if t.State == "queued" || model.Terminal(t.State) {
			r.signal()
		}
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
	return r.admitLocked(p, q, nil)
}
func (r *Relay) admitLocked(p model.Principal, q model.SendRequest, meta *admissionMeta) (model.Task, error) {
	var t model.Task
	if r.closed {
		return t, model.Err(503, "relay is shutting down")
	}
	ingress := meta != nil && meta.ingress != nil
	if ingress {
		if err := meta.ingress.validate(r, q); err != nil {
			return t, err
		}
	}
	if meta != nil && meta.validate != nil {
		if err := meta.validate(p, q); err != nil {
			return t, err
		}
	}
	if q.Type == "" {
		q.Type = "send"
	}
	if q.Type != "send" && q.Type != "request" {
		return t, model.Err(400, "type must be send or request")
	}
	if err := r.permit(p, q.Type); err != nil && !ingress {
		r.auditDenied(p, q.To, q.Type)
		return t, err
	}
	from := p.AgentID
	if p.Admin {
		from = q.From
	} else if q.From != "" && q.From != p.AgentID {
		r.auditDenied(p, q.To, q.Type)
		return t, model.Err(403, "cannot impersonate another agent")
	}
	var a model.Agent
	var err error
	if ingress {
		from = meta.ingress.source
		a = model.Agent{ID: from, SessionID: meta.ingress.source}
	} else {
		a, err = r.agent(from)
	}
	if p.Admin && from == "human:operator" {
		a = model.Agent{ID: from, SessionID: "console"}
		err = nil
	}
	if err != nil {
		return t, err
	}
	destination, err := r.agent(q.To)
	if err != nil {
		return t, err
	}
	if destination.ProfileManaged {
		if meta == nil || meta.profile == nil {
			return t, model.Err(403, "profile-managed recipients require trusted profile admission")
		}
		if err = r.validateProfileAdmission(q, *meta.profile); err != nil {
			return t, err
		}
	}
	if destination.GenerationDraft && (meta == nil || meta.generationAgentID != destination.ID) {
		return t, model.Err(403, "draft identities require trusted generation admission")
	}
	if !ingress {
		if err = r.authorizeRecipient(p, from, destination, q.Type); err != nil {
			return t, err
		}
	}
	session := p.SessionID
	if p.Admin || ingress {
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
	if !p.Admin && !ingress && !contains(a.Policy.Destinations, q.To) {
		r.auditDenied(p, q.To, q.Type)
		return t, model.Err(403, "destination not permitted: "+q.To)
	}
	if strings.TrimSpace(q.Text) == "" || !utf8.ValidString(q.Text) {
		return t, model.Err(400, "message must contain valid nonempty UTF-8 text")
	}
	if len(q.Text) > model.MaxTextBytes {
		return t, model.Err(413, "message exceeds 65536 bytes")
	}
	if len(q.IdempotencyKey) > 200 || strings.ContainsAny(q.IdempotencyKey, "\x00\r\n") || redact(q.IdempotencyKey) != q.IdempotencyKey {
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
			r.auditDenied(p, q.To, q.Type)
			return t, model.Err(403, "parent task does not match authenticated turn")
		}
		q.ParentTaskID = p.TaskID
	}
	attachmentPrincipal := p
	if ingress {
		attachmentPrincipal = model.Principal{AgentID: destination.ID}
	}
	attachments, err := r.validateTaskAttachments(attachmentPrincipal, q)
	if err != nil {
		return t, err
	}
	t = model.Task{ConversationID: q.ConversationID, ContextRefs: q.ContextRefs, DependsOn: q.DependsOn, SchemaVersion: 1, ID: randomID(), MessageID: randomID(), CorrelationID: randomID(), OriginAgentID: from, OriginSessionID: session, From: from, FromSessionID: session, To: q.To, HopCount: 1, MaxHops: q.MaxHops, Type: q.Type, Text: redact(q.Text), State: "queued", IdempotencyKey: q.IdempotencyKey, CreatedAt: now(), UpdatedAt: now(), MaxAttempts: 3, RequestHash: fingerprint(q)}
	t.AttachmentIDs = q.AttachmentIDs
	if len(attachments) > 0 {
		body, marshalErr := json.Marshal(struct {
			Base        string
			Attachments []model.Attachment
		}{t.RequestHash, attachments})
		if marshalErr != nil {
			return model.Task{}, marshalErr
		}
		t.RequestHash = tokenHash(string(body))
	}
	if meta != nil && meta.profile != nil {
		body, marshalErr := json.Marshal(struct {
			Base     string
			Snapshot model.ProfileTaskSnapshot
		}{t.RequestHash, *meta.profile})
		if marshalErr != nil {
			return model.Task{}, marshalErr
		}
		t.RequestHash = tokenHash(string(body))
	}
	t.Deadline = t.CreatedAt.Add(time.Duration(q.TimeoutSeconds) * time.Second)
	if q.ParentTaskID != "" {
		if !p.Admin && !a.Policy.Delegate {
			r.auditDenied(p, q.To, q.Type)
			return model.Task{}, model.Err(403, "delegation not permitted")
		}
		parent, e := r.task(q.ParentTaskID)
		if e != nil {
			return model.Task{}, e
		}
		if parent.Chat {
			return model.Task{}, model.Err(403, "chat turns cannot delegate")
		}
		if parent.To != from || parent.DestinationSessionID != session {
			r.auditDenied(p, q.To, q.Type)
			return model.Task{}, model.Err(403, "parent task is not owned by this destination session")
		}
		t.ParentTaskID = parent.ID
		t.CorrelationID = parent.CorrelationID
		t.OriginAgentID = parent.OriginAgentID
		t.OriginSessionID = parent.OriginSessionID
		t.HopCount = parent.HopCount + 1
		t.MaxHops = parent.MaxHops
		if parent.Deadline.Before(t.Deadline) {
			t.Deadline = parent.Deadline
		}
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
		e := r.db.QueryRow("SELECT body FROM tasks WHERE sender=? AND sender_session=? AND operation=? AND idempotency_key=?", from, session, q.Type, q.IdempotencyKey).Scan(&b)
		if e == nil {
			var old model.Task
			if e = json.Unmarshal([]byte(b), &old); e != nil {
				return model.Task{}, e
			}
			oldHash := old.RequestHash
			if oldHash == "" {
				oldHash = fingerprint(model.SendRequest{To: old.To, Text: old.Text, Type: old.Type, ParentTaskID: old.ParentTaskID})
			}
			if oldHash != t.RequestHash {
				return model.Task{}, model.Err(409, "idempotency key already used for a different request")
			}
			return old, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return model.Task{}, e
		}
	}
	if !ingress {
		if err := r.validateCollaboration(p, q); err != nil {
			return model.Task{}, err
		}
	}
	if q.ConversationID != "" {
		t.Chat, err = r.isChat(q.ConversationID)
		if err != nil {
			return model.Task{}, err
		}
		if t.Chat {
			if !p.Admin || from != "human:operator" || q.ParentTaskID != "" {
				return model.Task{}, model.Err(403, "chat accepts only direct human messages")
			}
			var active int
			if err = r.db.QueryRow(`SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=? AND state IN ('queued','dispatching','running')`, q.ConversationID).Scan(&active); err != nil {
				return model.Task{}, err
			}
			if active > 0 {
				return model.Task{}, model.Err(409, "wait for the reply or stop it before sending another message")
			}
		}
	}
	n, total, err := r.inboxDepth(q.To)
	if err != nil {
		return model.Task{}, err
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
	if _, err = tx.Exec("INSERT INTO tasks(id,sender,sender_session,recipient,operation,idempotency_key,state,body) VALUES(?,?,?,?,?,?,?,?)", t.ID, from, session, q.To, q.Type, key, t.State, string(b)); err != nil {
		return model.Task{}, err
	}
	if _, err = tx.Exec("INSERT INTO task_dispatch_order(task_id,agent_id,rank) SELECT ?,?,coalesce(max(rank),0)+1 FROM task_dispatch_order WHERE agent_id=?", t.ID, t.To, t.To); err != nil {
		return model.Task{}, err
	}
	if err = touchQueue(tx, t.To); err != nil {
		return model.Task{}, err
	}
	for _, attachment := range attachments {
		if _, err = tx.Exec("INSERT INTO attachment_tasks(attachment_id,task_id,role) VALUES(?,?,'input')", attachment.ID, t.ID); err != nil {
			return model.Task{}, err
		}
		if t.ConversationID != "" {
			if _, err = tx.Exec("INSERT OR IGNORE INTO attachment_conversations(attachment_id,conversation_id) VALUES(?,?)", attachment.ID, t.ConversationID); err != nil {
				return model.Task{}, err
			}
		}
	}
	if t.ConversationID != "" {
		if _, err = tx.Exec("INSERT INTO conversation_events(conversation_id,actor,operation,resource,time) VALUES(?,?,?,?,?)", t.ConversationID, t.From, "task.queued", t.ID, now().Format(time.RFC3339Nano)); err != nil {
			return model.Task{}, err
		}
	}
	if err = insertEvent(tx, t, "queued", "durably accepted; destination may be offline"); err != nil {
		return model.Task{}, err
	}
	if meta != nil && meta.profile != nil {
		snapshot := *meta.profile
		body, marshalErr := json.Marshal(struct {
			model.ProfileTaskSnapshot
			SourceRequestHash string `json:"sourceRequestHash"`
		}{snapshot, meta.sourceRequestHash})
		if marshalErr != nil {
			return model.Task{}, marshalErr
		}
		if _, err = tx.Exec("INSERT INTO profile_task_snapshots(task_id,profile_id,profile_revision,binding_id,body) VALUES(?,?,?,?,?)", t.ID, snapshot.Profile.ID, snapshot.Profile.Revision, snapshot.Binding.ID, string(body)); err != nil {
			return model.Task{}, err
		}
		bindingBody, bindingErr := json.Marshal(snapshot.Binding)
		if bindingErr != nil {
			return model.Task{}, bindingErr
		}
		if _, err = tx.Exec("UPDATE profile_bindings SET body=? WHERE id=?", string(bindingBody), snapshot.Binding.ID); err != nil {
			return model.Task{}, err
		}
		if err = insertAutomationEvent(tx, "profiles.task", t.ID, snapshot.Profile.ID, map[string]string{"conversationId": t.ConversationID, "bindingId": snapshot.Binding.ID}); err != nil {
			return model.Task{}, err
		}
	}
	if meta != nil && meta.persist != nil {
		if err = meta.persist(tx, t); err != nil {
			return model.Task{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.Task{}, err
	}
	r.logTransition(t, "queued")
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
	if t.ConversationID != "" && t.State == "queued" {
		ready, err := r.dependenciesReady(t)
		t.SchedulingState = "blocked"
		if ready && err == nil {
			t.SchedulingState = "ready"
		}
	}
	if t.State == "queued" {
		paused, pauseErr := r.queuePaused(t.To)
		if pauseErr != nil {
			return t, pauseErr
		}
		if paused {
			t.SchedulingState = "paused"
		}
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
	return r.events(t, after)
}

func (r *Relay) events(t model.Task, after int64) ([]model.Event, error) {
	rows, err := r.db.Query("SELECT id,kind,data,time,metadata FROM events WHERE task_id=? AND id>? ORDER BY id LIMIT 256", t.ID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		e := taskEvent(t)
		var tm, metadata string
		if err = rows.Scan(&e.ID, &e.Type, &e.Data, &tm, &metadata); err != nil {
			return nil, err
		}
		var meta model.Event
		if err = json.Unmarshal([]byte(metadata), &meta); err != nil {
			return nil, err
		}
		if meta.TaskID != "" {
			meta.ID, meta.Type, meta.Data = e.ID, e.Type, e.Data
			e = meta
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
	t.ErrorCategory = "cancelled"
	t.Ambiguous = t.Attempts > 0
	t.Error = "cancelled by caller; started execution may have external effects"
	t.LeaseOwner = ""
	t.LeaseUntil = time.Time{}
	t.NextAttemptAt = time.Time{}
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
	destination, err := r.agent(t.To)
	if err != nil {
		return t, err
	}
	if destination.GenerationDraft {
		return t, model.Err(403, "draft generation is disposable; create a new profiles.generate task")
	}
	if destination.ProfileManaged {
		snapshot, snapshotErr := r.profileSnapshot(t)
		if snapshotErr != nil {
			return t, snapshotErr
		}
		if err = r.validateProfileAdmission(model.SendRequest{To: t.To, ConversationID: t.ConversationID}, snapshot); err != nil {
			return t, err
		}
	}
	if t.State != "failed" && t.State != "timed_out" && t.State != "cancelled" {
		return model.Task{}, model.Err(409, "only failed, timed_out or cancelled tasks may be manually retried")
	}
	limit := t.MaxAttempts
	if limit == 0 {
		limit = 3
	}
	if t.Attempts >= limit {
		return model.Task{}, model.Err(409, "manual retry limit reached; inspect external effects before creating new work")
	}
	if s := r.slots[t.To]; s != nil && s.taskID == id {
		return model.Task{}, model.Err(409, "previous execution is still stopping")
	}
	if t.Chat {
		var active int
		if err = r.db.QueryRow(`SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=? AND state IN ('queued','dispatching','running')`, t.ConversationID).Scan(&active); err != nil {
			return t, err
		}
		if active > 0 {
			return t, model.Err(409, "another chat reply is active")
		}
	}
	n, total, err := r.inboxDepth(t.To)
	if err != nil {
		return t, err
	}
	if n >= queueLimit || total >= globalQueueLimit {
		return model.Task{}, model.Err(429, "persistent inbox capacity reached")
	}
	if _, err = r.agent(t.To); err != nil {
		return model.Task{}, err
	}
	t.State = "queued"
	t.Error = ""
	t.ErrorCategory = ""
	t.Ambiguous = false
	t.DeliveryAttempts = 0
	t.NextAttemptAt = time.Time{}
	t.LeaseOwner = ""
	t.LeaseUntil = time.Time{}
	t.Result = ""
	t.ResultRef = nil
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
			t.ErrorCategory = "uncertain_execution"
			t.Ambiguous = true
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
	next := now().Add(delay)
	if !now().Before(r.nextPrune) {
		if err := r.prune(); err != nil {
			r.logger.Error("history retention failed", "errorClass", "storage")
			return time.Second
		}
	}
	if r.nextPrune.Before(next) {
		next = r.nextPrune
	}
	ts, err := r.tasks()
	if err != nil {
		return time.Second
	}
	paused, err := r.pausedQueues()
	if err != nil {
		return time.Second
	}
	rotations, err := r.pendingProfileRotations()
	if err != nil {
		return time.Second
	}
	for _, t := range ts {
		remaining := time.Until(t.Deadline)
		if remaining <= 0 {
			t.Ambiguous = t.State != "queued"
			t.ErrorCategory = "deadline_exceeded"
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
		if t.Deadline.Before(next) {
			next = t.Deadline
		}
		if t.State != "queued" {
			continue
		}
		if paused[t.To] || r.rotatingProfiles[t.To] {
			continue
		}
		ready, depErr := r.dependenciesReady(t)
		if depErr != nil {
			t.State = "failed"
			t.ErrorCategory = "dependency_failed"
			t.Error = "dependency missing or failed; explicit retry required"
			if r.transition(t, "failed", t.Error) != nil {
				return time.Second
			}
			continue
		}
		if !ready {
			continue
		}
		s := r.slots[t.To]
		if len(r.repos) >= r.settings.MaxParallelTurns || (s != nil && s.taskID != "") {
			continue
		}
		a, e := r.agent(t.To)
		if e == nil && a.ProfileManaged && rotations[t.To] && s != nil && s.runtime != nil && a.State == "idle" {
			r.beginProfileRotation(t.To)
			continue
		}
		if e != nil || s == nil || s.runtime == nil || a.State != "idle" {
			if e == nil && a.ProfileManaged {
				continue
			}
			if wait := time.Until(t.NextAttemptAt); wait > 0 {
				if t.NextAttemptAt.Before(next) {
					next = t.NextAttemptAt
				}
				continue
			}
			if e = r.offline(t); e != nil {
				return time.Second
			}
			// transition signals the scheduler, which observes the persisted retry time.
			continue
		}
		a, e = r.chatAgent(t, a)
		if e != nil {
			t.State = "failed"
			t.ErrorCategory = "workspace_unavailable"
			t.Error = "workspace is unavailable; check the project directory"
			_ = r.transition(t, "failed", t.Error)
			continue
		}
		repo := repository(a.Cwd)
		if r.repos[repo] {
			continue
		}
		if e = r.validateAttachmentDispatch(t); e != nil {
			t.State = "failed"
			t.ErrorCategory = "attachment_unavailable"
			t.Error = "attachment unavailable, corrupt or authorization revoked"
			if r.transition(t, "failed", t.Error) != nil {
				return time.Second
			}
			continue
		}
		if _, e = r.resolveTaskContext(t); e != nil {
			t.State = "failed"
			t.ErrorCategory = "context_denied"
			t.Error = "context unavailable or authorization revoked"
			if r.transition(t, "failed", t.Error) != nil {
				return time.Second
			}
			continue
		}
		t.State = "dispatching"
		t.Attempts++
		reserved, reserveErr := r.reserveRun(t, a)
		if reserveErr != nil {
			return time.Second
		}
		if !reserved {
			reset := now().Truncate(24 * time.Hour).Add(24 * time.Hour)
			if reset.Before(next) {
				next = reset
			}
			continue
		}
		s.repo = repo
		t.Error = ""
		t.ErrorCategory = ""
		t.NextAttemptAt = time.Time{}
		t.LeaseOwner = r.owner
		t.LeaseUntil = t.Deadline
		if !t.Chat {
			t.DestinationSessionID = s.runtime.SessionID()
		} else {
			t.DestinationSessionID = ""
		}
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
			t.ErrorCategory = "credential_unavailable"
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
	// Persistence/logging may have consumed the remaining time. Never restart a
	// relative interval measured before that work and thereby extend a deadline.
	return time.Until(next)
}
func (r *Relay) execute(ctx context.Context, a model.Agent, s *slot, t model.Task, token, url string) {
	defer r.wg.Done()
	defer r.signal()
	startedAt := time.Now()
	r.mu.Lock()
	current, e := r.task(t.ID)
	if e != nil {
		r.mu.Unlock()
		return
	}
	if !model.Terminal(current.State) {
		e = r.transition(current, "delivered", "delivered to the owned adapter; execution completion not acknowledged")
		if e == nil {
			current.State = "running"
			e = r.transition(current, "running", "harness turn started; external effects now possible")
		}
	}
	var tools *toolBroker
	if e == nil && a.ProfileManaged && !model.Terminal(current.State) {
		tools, e = r.newToolBrokerLocked(ctx, current, s)
	}
	var injected string
	if e == nil && !model.Terminal(current.State) {
		injected, e = r.resolveTaskContext(current)
	}
	if e == nil && len(current.AttachmentIDs) > 0 && !model.Terminal(current.State) {
		var excerpt string
		excerpt, e = r.attachmentExcerpt(current)
		injected += excerpt
	}
	if e == nil && current.Chat && !model.Terminal(current.State) {
		injected, e = r.chatPrompt(current, injected)
	}
	if e == nil && a.ProfileManaged && !model.Terminal(current.State) {
		var profileText string
		profileText, e = r.profilePrompt(current)
		injected = profileText + injected
	}
	r.mu.Unlock()
	runtime := s.runtime
	if current.Chat {
		runtime = nil
	}
	if e == nil && current.Chat && !model.Terminal(current.State) {
		fresh := a
		fresh.SessionID = ""
		fresh.Ephemeral = true
		runtime, e = r.adapters[a.Harness].Factory(ctx, fresh, nil)
		if e == nil {
			r.mu.Lock()
			current, e = r.task(t.ID)
			if e == nil && !model.Terminal(current.State) {
				current.DestinationSessionID = runtime.SessionID()
				_, e = r.db.Exec("UPDATE credentials SET session_id=? WHERE hash=?", current.DestinationSessionID, tokenHash(token))
				if e == nil {
					e = r.transition(current, "session", current.DestinationSessionID)
				}
			}
			r.mu.Unlock()
		}
	}
	var result string
	var runErr error
	eventCount := 0
	var deltas strings.Builder
	deltaOverflow := false
	sawDelta := false
	if e == nil && !model.Terminal(current.State) {
		inputBytes := len(injected)
		if !current.Chat {
			inputBytes += len(current.Text)
		}
		_, _ = r.db.Exec("UPDATE run_usage SET input_bytes=? WHERE task_id=? AND attempt=?", inputBytes, t.ID, t.Attempts)
		delivery := current
		if current.Chat {
			delivery.Text = injected
		} else {
			delivery.Text += injected
		}
		var executor model.ToolExecutor
		if tools != nil {
			executor = tools
			ctx = context.WithValue(ctx, admittedConnectionKey{}, &tools.snapshot)
		}
		result, runErr = runtime.Run(ctx, model.Turn{Task: delivery, Token: token, MCPURL: url, Tools: executor}, func(kind, data string) {
			data = strings.ReplaceAll(data, token, "[REDACTED]")
			r.mu.Lock()
			defer r.mu.Unlock()
			task, err := r.task(t.ID)
			if err != nil || model.Terminal(task.State) {
				return
			}
			if eventCount >= 10000 {
				if kind == "delta" {
					deltaOverflow = true
				}
				return
			}
			eventCount++
			if kind == "delta" {
				// Do not persist raw fragments: a credential can span any native
				// delta boundary. Progress stays immediate; text waits for success.
				if !sawDelta {
					sawDelta = true
					_ = r.transition(task, "progress", "text buffered until completion for credential-safe persistence")
				}
				if !deltaOverflow && deltas.Len()+len(data) <= 256*1024 {
					deltas.WriteString(data)
				} else {
					deltaOverflow = true
				}
				return
			}
			if kind == "context_usage" {
				r.recordContext(task, data)
				return
			}
			if kind == "usage" {
				r.recordUsage(task, data)
				return
			}
			if kind == "session" {
				a, err := r.agent(t.To)
				if err == nil && !t.Chat {
					a.SessionID = data
					_ = r.saveAgent(a)
				}
				task.DestinationSessionID = data
				_ = r.transition(task, "session", data)
				_, _ = r.db.Exec("UPDATE credentials SET session_id=? WHERE hash=?", data, tokenHash(token))
				return
			}
			if len(data) > 16384 {
				data = truncate(data, 16384) + " [event truncated]"
			}
			_ = r.transition(task, kind, data)
		})
	} else {
		runErr = e
	}
	if tools != nil {
		tools.Close()
	}
	if current.Chat && runtime != nil {
		_ = runtime.Close()
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
			current.ErrorCategory = "uncertain_execution"
			current.Ambiguous = true
		case ctxErr == context.DeadlineExceeded:
			current.State = "timed_out"
			current.Error = "task deadline exceeded"
			current.ErrorCategory = "deadline_exceeded"
			current.Ambiguous = true
		case ctxErr != nil:
			current.State = "cancelled"
			current.Error = "turn cancelled; execution may have external effects"
			current.ErrorCategory = "cancelled"
			current.Ambiguous = true
		case runErr != nil:
			current.State = "failed"
			current.Error = "harness turn failed: " + truncate(strings.ReplaceAll(runErr.Error(), token, "[REDACTED]"), 2048)
			current.ErrorCategory = "adapter_failure"
			current.Ambiguous = true
		case len(result) > 256*1024:
			current.State = "failed"
			current.Error = "harness response exceeds 262144 bytes"
			current.ErrorCategory = "response_too_large"
			current.Ambiguous = true
		default:
			text := deltas.String()
			if deltaOverflow {
				text = result
			}
			text = redact(strings.ReplaceAll(text, token, "[REDACTED]"))
			for len(text) > 0 && eventCount < 10000 {
				chunk := truncate(text, 16384)
				if err := r.transition(current, "delta", chunk); err != nil {
					r.logger.Error("sanitized output persistence failed", "requestId", current.ID, "errorClass", "storage")
					break
				}
				text = text[len(chunk):]
				eventCount++
			}
			current.State = "completed"
			current.Result = redact(strings.ReplaceAll(result, token, "[REDACTED]"))
		}
		if current.State == "completed" && !current.Chat && current.ConversationID != "" && current.Result != "" {
			ref, err := r.publishTaskResult(current)
			if err != nil {
				current.State = "failed"
				current.ErrorCategory = "result_storage_failed"
				current.Error = "execution completed but result artifact persistence failed; inspect before retry"
				current.Ambiguous = true
			} else {
				current.ResultRef = &ref
			}
		}
		if sid := runtimeSessionID(runtime); sid != "" {
			current.DestinationSessionID = sid
		}
		_ = r.transition(current, current.State, current.Error)
	}
	_, _ = r.db.Exec("DELETE FROM credentials WHERE hash=?", tokenHash(token))
	s.lastUsed = now()
	_, _ = r.db.Exec("UPDATE run_usage SET state=?,output_bytes=?,duration_ms=? WHERE task_id=? AND attempt=?", current.State, len(result), time.Since(startedAt).Milliseconds(), t.ID, t.Attempts)
	s.taskID = ""
	s.turnCancel = nil
	if r.slots[a.ID] == s {
		latest, err := r.agent(a.ID)
		if err == nil {
			if !t.Chat {
				latest.SessionID = s.runtime.SessionID()
			}
			if latest.State == "busy" {
				if !t.Chat && (runErr != nil || ctxErr != nil) {
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
