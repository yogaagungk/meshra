package relay

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"meshra/internal/model"
)

// Options bound delivery checks and history without changing v0.1 Open callers.
type Options struct {
	Retention           time.Duration
	MaxDeliveryAttempts int
	RetryBase           time.Duration
	RetryCap            time.Duration
}

func (o Options) defaults() (Options, error) {
	if o.Retention == 0 {
		o.Retention = 30 * 24 * time.Hour
	}
	if o.MaxDeliveryAttempts == 0 {
		o.MaxDeliveryAttempts = 16
	}
	if o.RetryBase == 0 {
		o.RetryBase = time.Second
	}
	if o.RetryCap == 0 {
		o.RetryCap = 30 * time.Second
	}
	if o.MaxDeliveryAttempts < 1 || o.MaxDeliveryAttempts > 1000 || o.RetryBase < time.Millisecond || o.RetryCap < o.RetryBase || o.RetryCap > time.Hour {
		return o, errors.New("invalid delivery policy: attempts 1..1000, retry base >=1ms, base <= cap <=1h")
	}
	return o, nil
}

func migrate(db *sql.DB) error {
	var exists int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		// New databases use the unchanged v0.1 baseline followed by the same upgrade.
		if _, err := db.Exec(migration); err != nil {
			return err
		}
	}
	var version int
	if err := db.QueryRow("SELECT max(version) FROM schema_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > 2 {
		return fmt.Errorf("unsupported database schema %d; this executable supports 1..2", version)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=OFF;"); err != nil {
		return err
	}
	if version == 1 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(migrationV2); err != nil {
			return fmt.Errorf("schema 2 migration: %w", err)
		}
		rows, err := tx.Query("PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		invalid := rows.Next()
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return scanErr
		}
		if invalid {
			return errors.New("schema migration would break foreign keys")
		}
		if err = scrubLegacy(tx); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err := db.Exec("PRAGMA foreign_keys=ON")
	return err
}

func (r *Relay) SetLogger(logger *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if logger != nil {
		r.logger = logger
	}
}
func fingerprint(q model.SendRequest) string {
	// Compare the original request, not its redacted representation.
	b, _ := json.Marshal(struct{ To, Text, Type, Parent string }{q.To, q.Text, q.Type, q.ParentTaskID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func allowedTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case "queued":
		return to == "dispatching" || to == "failed" || to == "cancelled" || to == "timed_out"
	case "dispatching":
		return to == "running" || to == "failed" || to == "cancelled" || to == "timed_out"
	case "running":
		return model.Terminal(to)
	case "failed", "cancelled", "timed_out":
		return to == "queued"
	default:
		return false
	}
}

// Redact retained v0.1 content in the upgrade transaction, retaining admission
// fingerprints before redaction. Read bounded pages; do not load history at once.
func scrubLegacy(tx *sql.Tx) error {
	var seq int64
	for {
		rows, err := tx.Query("SELECT seq,body FROM tasks WHERE seq>? ORDER BY seq LIMIT 128", seq)
		if err != nil {
			return err
		}
		tasks := []model.Task{}
		for rows.Next() {
			var b string
			var t model.Task
			if err = rows.Scan(&seq, &b); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal([]byte(b), &t); err != nil {
				rows.Close()
				return err
			}
			if t.RequestHash == "" {
				t.RequestHash = fingerprint(model.SendRequest{To: t.To, Text: t.Text, Type: t.Type, ParentTaskID: t.ParentTaskID})
			}
			t.Text, t.Result, t.Error = redact(t.Text), redact(t.Result), redact(t.Error)
			if t.MaxAttempts == 0 {
				t.MaxAttempts = 3
			}
			tasks = append(tasks, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			break
		}
		for _, t := range tasks {
			b, err := json.Marshal(t)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE tasks SET body=? WHERE id=?", string(b), t.ID); err != nil {
				return err
			}
		}
	}
	var cursor int64
	for {
		rows, err := tx.Query("SELECT id,data FROM events WHERE id>? ORDER BY id LIMIT 256", cursor)
		if err != nil {
			return err
		}
		type item struct {
			id   int64
			data string
		}
		items := []item{}
		for rows.Next() {
			var e item
			if err = rows.Scan(&e.id, &e.data); err != nil {
				rows.Close()
				return err
			}
			cursor = e.id
			items = append(items, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		for _, e := range items {
			if clean := redact(e.data); clean != e.data {
				if _, err = tx.Exec("UPDATE events SET data=? WHERE id=?", clean, e.id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *Relay) inboxDepth(recipient string) (int, int, error) {
	var local, total int
	err := r.db.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN recipient=? THEN 1 ELSE 0 END),0) FROM tasks WHERE state IN ('queued','dispatching','running')", recipient).Scan(&total, &local)
	return local, total, err
}
func taskEvent(t model.Task) model.Event {
	harness, _, _ := strings.Cut(t.To, ":")
	return model.Event{TaskID: t.ID, MessageID: t.MessageID, CorrelationID: t.CorrelationID, From: t.From, To: t.To, Harness: harness, SessionID: t.DestinationSessionID, Attempt: t.Attempts}
}
func insertEvent(tx *sql.Tx, t model.Task, kind, data string) error {
	meta, _ := json.Marshal(taskEvent(t))
	_, err := tx.Exec("INSERT INTO events(task_id,kind,data,time,metadata) VALUES(?,?,?,?,?)", t.ID, kind, redact(data), t.UpdatedAt.Format(time.RFC3339Nano), string(meta))
	return err
}
func (r *Relay) logTransition(t model.Task, kind string) {
	harness, _, _ := strings.Cut(t.To, ":")
	r.logger.Info("message lifecycle", "requestId", t.ID, "messageId", t.MessageID, "correlationId", t.CorrelationID, "sender", t.From, "recipient", t.To, "harness", harness, "session", redact(t.DestinationSessionID), "transition", kind, "state", t.State, "attempt", t.Attempts, "deliveryAttempt", t.DeliveryAttempts, "latencySeconds", time.Since(t.CreatedAt).Seconds(), "errorClass", t.ErrorCategory, "ambiguous", t.Ambiguous)
}
func (r *Relay) retryDelay(attempt int) time.Duration {
	delay := r.options.RetryBase
	for i := 1; i < attempt && delay < r.options.RetryCap; i++ {
		delay = min(delay*2, r.options.RetryCap)
	}
	// Equal jitter, bounded in [delay/2, delay], avoids lockstep retries.
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}
func (r *Relay) offline(t model.Task) error {
	t.DeliveryAttempts++
	t.ErrorCategory = "destination_offline"
	t.Error = "destination offline; explicit agent start required"
	if t.DeliveryAttempts >= r.options.MaxDeliveryAttempts {
		t.State = "failed"
		t.ErrorCategory = "delivery_exhausted"
		t.Error = "offline delivery checks exhausted; start destination and explicitly retry"
		return r.transition(t, "failed", t.Error)
	}
	t.NextAttemptAt = now().Add(r.retryDelay(t.DeliveryAttempts))
	return r.transition(t, "retry_scheduled", t.Error)
}

func (r *Relay) Trace(p model.Principal, id string) (model.Trace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.task(id)
	if err != nil {
		return model.Trace{}, err
	}
	if err = r.access(p, t, "status"); err != nil {
		return model.Trace{}, err
	}
	if err = r.access(p, t, "events"); err != nil {
		return model.Trace{}, err
	}
	out := model.Trace{Task: t, Events: []model.Event{}}
	var after int64
	if err = r.db.QueryRow("SELECT coalesce((SELECT id FROM events WHERE task_id=? ORDER BY id DESC LIMIT 1 OFFSET 256),0)", id).Scan(&after); err != nil {
		return model.Trace{}, err
	}
	out.EventsTruncated = after > 0
	out.Events, err = r.events(t, after)
	if err != nil {
		return model.Trace{}, err
	}
	for i := range out.Events {
		if len(out.Events[i].Data) > 2048 {
			out.Events[i].Data = truncate(out.Events[i].Data, 2048) + " [trace truncated; use paginated events]"
			out.EventsTruncated = true
		}
	}
	return out, nil
}
func (r *Relay) Metrics(p model.Principal) (model.Metrics, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var m model.Metrics
	if err := requireAdmin(p); err != nil {
		return m, err
	}
	rows, err := r.db.Query(`SELECT state,count(*),coalesce(sum(CASE WHEN coalesce(json_extract(body,'$.attempts'),0)>1 OR coalesce(json_extract(body,'$.deliveryAttempts'),0)>0 THEN 1 ELSE 0 END),0),coalesce(min(json_extract(body,'$.createdAt')),''),coalesce(avg((julianday(json_extract(body,'$.updatedAt'))-julianday(json_extract(body,'$.createdAt')))*86400),0) FROM tasks GROUP BY state`)
	if err != nil {
		return m, err
	}
	var completed int64
	var totalLatency float64
	for rows.Next() {
		var state, oldest string
		var count, retried int64
		var latency float64
		if err = rows.Scan(&state, &count, &retried, &oldest, &latency); err != nil {
			rows.Close()
			return m, err
		}
		m.Retried += retried
		switch state {
		case "queued":
			m.Queued += count
			if tm, e := time.Parse(time.RFC3339Nano, oldest); e == nil {
				m.OldestQueueSeconds = max(0, time.Since(tm).Seconds())
			}
		case "dispatching", "running":
			m.InFlight += count
		case "completed":
			m.Succeeded += count
		case "failed":
			m.Failed += count
		case "timed_out":
			m.Expired += count
		}
		if model.Terminal(state) {
			completed += count
			totalLatency += latency * float64(count)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return m, err
	}
	if completed > 0 {
		m.AverageLatencySeconds = max(0, totalLatency/float64(completed))
	}
	agents, err := r.agents()
	if err != nil {
		return m, err
	}
	for _, a := range agents {
		if a.State != "idle" && a.State != "busy" {
			m.OfflineAgents++
		}
	}
	return m, nil
}

func (r *Relay) Doctor(p model.Principal) (model.Diagnostics, error) {
	if err := requireAdmin(p); err != nil {
		return model.Diagnostics{}, err
	}
	r.mu.Lock()
	agents, err := r.agents()
	dbErr := r.db.Ping()
	r.mu.Unlock()
	if err != nil {
		return model.Diagnostics{}, err
	}
	out := model.Diagnostics{Healthy: true, Checks: []model.Diagnostic{}}
	add := func(name, status, detail string) {
		out.Checks = append(out.Checks, model.Diagnostic{Name: name, Status: status, Detail: detail})
		if status == "error" {
			out.Healthy = false
		}
	}
	if dbErr != nil {
		add("sqlite", "error", "database unavailable")
	} else {
		add("sqlite", "ok", "WAL database open; exclusive daemon ownership")
	}
	for _, h := range []string{"omp", "codex", "claude", "opencode"} {
		executable := h
		if h == "claude" {
			executable = "node"
		}
		if h == "opencode" {
			executable = os.Getenv("MESHRA_OPENCODE_BIN")
			if executable == "" {
				executable = "opencode"
				if _, e := exec.LookPath(executable); e != nil {
					executable = localAsset("bridges/claude-agent-sdk/opencode/node_modules/.bin/opencode", "")
				}
			}
		}
		path, e := exec.LookPath(executable)
		if e != nil {
			add(h, "error", "executable unavailable; see adapter installation guide")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		arg := "--version"
		output, e := exec.CommandContext(ctx, path, arg).CombinedOutput()
		cancel()
		if e != nil {
			add(h, "error", "version probe failed; inspect executable installation")
			continue
		}
		version := strings.TrimSpace(string(output))
		if len(version) > 200 {
			version = "version response too large"
		}
		expected := ""
		switch h {
		case "omp":
			expected = "omp/18.12.1"
		case "codex":
			expected = "codex-cli 0.163.0"
		case "opencode":
			expected = "1.19.3"
		}
		if expected != "" && version != expected {
			add(h, "error", "unsupported/unverified installed version; expected "+expected)
		} else {
			add(h, "ok", redact(version))
		}
		if h == "claude" {
			bridge := localAsset("bridges/claude-agent-sdk/bridge.mjs", "MESHRA_CLAUDE_BRIDGE")
			if bridge == "" {
				add("claude-bridge", "error", "bridge missing; set MESHRA_CLAUDE_BRIDGE")
				continue
			}
			data, e := os.ReadFile(filepath.Join(filepath.Dir(bridge), "node_modules/@anthropic-ai/claude-agent-sdk/package.json"))
			var manifest struct {
				Version           string `json:"version"`
				ClaudeCodeVersion string `json:"claudeCodeVersion"`
			}
			if e != nil || json.Unmarshal(data, &manifest) != nil {
				add("claude-sdk", "error", "SDK dependency missing; run npm ci --prefix bridges/claude-agent-sdk")
			} else if manifest.Version != "0.3.303" || manifest.ClaudeCodeVersion != "2.1.303" {
				add("claude-sdk", "error", "unverified SDK version; expected SDK0.3.303/CLI2.1.303")
			} else {
				add("claude-sdk", "ok", "SDK0.3.303 / bundled CLI2.1.303")
			}
		}
	}
	for _, a := range agents {
		if e := validateWorkspace(a); e != nil {
			add(a.ID+" workspace", "error", "working directory missing or canonical path changed")
		} else {
			add(a.ID+" workspace", "ok", "canonical directory available")
		}
		if a.State == "error" {
			add(a.ID+" runtime", "error", "unhealthy; inspect task trace and restart explicitly")
		}
	}
	add("credentials", "unknown", "credentials remain in native stores; no provider call or secret inspection performed")
	return out, nil
}

// Match the existing optional-adapter discovery order without changing harness configuration.
func localAsset(relative, override string) string {
	if override != "" {
		if path := os.Getenv(override); path != "" {
			if abs, err := filepath.Abs(path); err == nil {
				if st, e := os.Stat(abs); e == nil && !st.IsDir() {
					return abs
				}
			}
			return ""
		}
	}
	exe, _ := os.Executable()
	_, source, _, _ := runtime.Caller(0)
	for _, root := range []string{filepath.Dir(exe), ".", filepath.Join(filepath.Dir(source), "../..")} {
		path, err := filepath.Abs(filepath.Join(root, relative))
		if err != nil {
			continue
		}
		if st, e := os.Stat(path); e == nil && !st.IsDir() {
			return path
		}
	}
	return ""
}

// prune removes expired leaf history transactionally. Parents referenced by retained
// work and terminal tasks whose Run has not returned are never removed.
func (r *Relay) prune() error {
	r.nextPrune = now().Add(time.Hour)
	if r.options.Retention < 0 {
		return nil
	}
	cutoff := now().Add(-r.options.Retention).Format(time.RFC3339Nano)
	for {
		rows, err := r.db.Query(`SELECT id FROM tasks t WHERE state IN ('completed','failed','cancelled','timed_out') AND json_extract(body,'$.updatedAt')<? AND NOT EXISTS(SELECT 1 FROM tasks child WHERE json_extract(child.body,'$.parentTaskId')=t.id) LIMIT 256`, cutoff)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			active := false
			for _, s := range r.slots {
				if s.taskID == id {
					active = true
					break
				}
			}
			if !active {
				ids = append(ids, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err = tx.Exec("DELETE FROM events WHERE task_id=?", id); err != nil {
				break
			}
			if _, err = tx.Exec("DELETE FROM tasks WHERE id=?", id); err != nil {
				break
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err := r.db.Exec("DELETE FROM audit WHERE time<?", cutoff)
	return err
}
