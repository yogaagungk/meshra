package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (r *Relay) loadPersonalSettings() error {
	r.settings = model.PersonalSettings{DisplayName: "Local operator", MaxParallelTurns: 4, MaxStartedAgents: 8, IdleMinutes: 0, DailyTurnLimit: 0}
	var body string
	err := r.db.QueryRow("SELECT body FROM personal_settings WHERE id=1").Scan(&body)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(body), &r.settings)
}
func validConfig(c model.Config, harness string) error {
	for _, s := range []string{c.Model, c.Provider} {
		if len(s) > 200 || !safeRegistryText(s) || strings.Contains(s, "sk-") || redact(s) != s {
			return model.Err(400, "model/provider must be identifiers, not credentials")
		}
	}
	if harness == "opencode" && (c.Model == "") != (c.Provider == "") {
		return model.Err(400, "OpenCode requires both model and provider")
	}
	if harness == "claude" && c.Provider != "" && c.Provider != "anthropic" {
		return model.Err(400, "Claude uses its native Anthropic provider")
	}
	return nil
}
func (r *Relay) personalLocked(op string, q model.ConsoleRequest) (any, error) {
	switch op {
	case "preferences":
		return r.settings, nil
	case "preferences/save":
		if q.Settings == nil {
			return nil, model.Err(400, "settings required")
		}
		v := *q.Settings
		v.DisplayName = strings.TrimSpace(v.DisplayName)
		if len(v.DisplayName) < 1 || len(v.DisplayName) > 80 || !safeRegistryText(v.DisplayName) || redact(v.DisplayName) != v.DisplayName {
			return nil, model.Err(400, "display name must be 1..80 bytes without credentials or control characters")
		}
		if v.MaxParallelTurns < 1 || v.MaxParallelTurns > 32 || v.MaxStartedAgents < 1 || v.MaxStartedAgents > 64 || v.IdleMinutes < 0 || v.IdleMinutes > 1440 || v.DailyTurnLimit < 0 || v.DailyTurnLimit > 100000 {
			return nil, model.Err(400, "invalid resource limits")
		}
		b, _ := json.Marshal(v)
		if _, err := r.db.Exec("INSERT INTO personal_settings VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", string(b)); err != nil {
			return nil, err
		}
		r.settings = v
		r.signal()
		return v, nil
	case "resources":
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		var queued int
		if err := r.db.QueryRow("SELECT count(*) FROM tasks WHERE state='queued'").Scan(&queued); err != nil {
			return nil, err
		}
		rss := int64(0)
		if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
			f := strings.Fields(string(raw))
			if len(f) > 1 {
				pages, _ := strconv.ParseInt(f[1], 10, 64)
				rss = pages * int64(os.Getpagesize())
			}
		}
		return map[string]any{"settings": r.settings, "startedAgents": len(r.slots), "activeTurns": len(r.repos), "queued": queued, "goHeapBytes": m.HeapAlloc, "daemonRssBytes": rss, "nativeMemoryIncluded": false}, nil
	case "usage":
		today := now().Format("2006-01-02")
		type row struct {
			Harness      string `json:"harness"`
			Model        string `json:"model"`
			Provider     string `json:"provider"`
			Turns        int    `json:"turns"`
			Completed    int    `json:"completed"`
			InputBytes   int64  `json:"inputBytes"`
			OutputBytes  int64  `json:"outputBytes"`
			Duration     int64  `json:"durationMs"`
			InputTokens  int64  `json:"inputTokens"`
			OutputTokens int64  `json:"outputTokens"`
			Reported     int    `json:"reportedTurns"`
		}
		var total int
		if err := r.db.QueryRow("SELECT count(*) FROM run_usage WHERE started_at>=?", today).Scan(&total); err != nil {
			return nil, err
		}
		rows, err := r.db.Query(`SELECT harness,model,provider,count(*),sum(state='completed'),sum(input_bytes),sum(output_bytes),sum(duration_ms),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),sum(input_tokens IS NOT NULL AND output_tokens IS NOT NULL) FROM run_usage WHERE started_at>=? GROUP BY harness,model,provider ORDER BY count(*) DESC LIMIT 100`, today)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []row{}
		for rows.Next() {
			var v row
			if err = rows.Scan(&v.Harness, &v.Model, &v.Provider, &v.Turns, &v.Completed, &v.InputBytes, &v.OutputBytes, &v.Duration, &v.InputTokens, &v.OutputTokens, &v.Reported); err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return map[string]any{"day": today, "timezone": "UTC", "turns": total, "dailyTurnLimit": r.settings.DailyTurnLimit, "items": items, "retentionDays": 30}, rows.Err()
	}
	return nil, model.Err(404, "unknown personal operation")
}

// Limits are enforced at dispatch, including regeneration and agent-to-agent work.
// Durable reservations count interrupted attempts and survive daemon restarts.
func (r *Relay) reserveRun(t model.Task, a model.Agent) (bool, error) {
	var count int
	if r.settings.DailyTurnLimit > 0 {
		if err := r.db.QueryRow("SELECT count(*) FROM run_usage WHERE started_at>=?", now().Format("2006-01-02")).Scan(&count); err != nil {
			return false, err
		}
		if count >= r.settings.DailyTurnLimit {
			return false, nil
		}
	}
	workspace := ""
	if t.Chat {
		config, err := r.chatSettings(t.ConversationID)
		if err != nil {
			return false, err
		}
		workspace = config.WorkspaceID
	}
	_, err := r.db.Exec(`INSERT INTO run_usage(task_id,attempt,started_at,harness,model,provider,workspace_id,state,input_bytes,conversation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, t.ID, t.Attempts, now().Format(time.RFC3339Nano), a.Harness, a.Config.Model, a.Config.Provider, workspace, "running", len(t.Text), t.ConversationID)
	return err == nil, err
}
func (r *Relay) reapIdle() {
	defer r.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.releaseIdle()
		}
	}
}
func (r *Relay) releaseIdle() {
	if !r.lifecycle.TryLock() {
		return
	}
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	ids := []string{}
	if !r.closed && r.settings.IdleMinutes > 0 {
		for id, s := range r.slots {
			if s.taskID != "" || time.Since(s.lastUsed) < time.Duration(r.settings.IdleMinutes)*time.Minute {
				continue
			}
			a, err := r.agent(id)
			if err != nil || a.State != "idle" {
				continue
			}
			var n int
			if r.db.QueryRow("SELECT count(*) FROM tasks WHERE recipient=? AND state IN ('queued','dispatching','running')", id).Scan(&n) == nil && n == 0 {
				a.State = "stopping"
				if r.saveAgent(a) == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		_ = r.stop(id)
	}
}
func (r *Relay) discoverModels(harness string) (any, error) {
	// Single discovery process at a time, bounded lifetime and no model generation.
	if !r.catalogMu.TryLock() {
		return nil, model.Err(409, "model discovery is already running")
	}
	defer r.catalogMu.Unlock()
	r.mu.Lock()
	ad, ok := r.adapters[harness]
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, model.Err(503, "relay is stopping")
	}
	if !ok {
		return nil, model.Err(400, "unknown harness")
	}
	if ad.Factory == nil && ad.Models == nil {
		return nil, model.Err(503, "harness is unavailable")
	}
	ctx, cancel := context.WithTimeout(r.ctx, 25*time.Second)
	defer cancel()
	var items []model.ModelOption
	var err error
	if ad.Models != nil {
		items, err = ad.Models(ctx)
	} else {
		dir := filepath.Join(filepath.Dir(r.lock.Name()), "chat-workspaces", harness)
		if err = os.MkdirAll(dir, 0700); err == nil && ad.Factory != nil {
			var rt model.Runtime
			rt, err = ad.Factory(ctx, model.Agent{ID: harness + ":catalog", Harness: harness, Cwd: dir, Ephemeral: true}, nil)
			if err == nil {
				defer rt.Close()
				if catalog, yes := rt.(model.ModelCatalog); yes {
					items, err = catalog.Models(ctx)
				} else {
					err = fmt.Errorf("harness has no model catalog")
				}
			}
		}
	}
	if err != nil {
		return nil, model.Err(503, "model discovery unavailable; check harness installation and native authentication")
	}
	out := []model.ModelOption{}
	seen := map[string]bool{}
	for _, v := range items {
		if len(out) >= 200 {
			break
		}
		if v.ID == "" || len(v.Name) > 200 || !safeRegistryText(v.Name) || redact(v.Name) != v.Name || validConfig(model.Config{Model: v.ID, Provider: v.Provider}, harness) != nil {
			continue
		}
		key := v.Provider + "/" + v.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		if v.Name == "" {
			v.Name = v.ID
		}
		out = append(out, v)
	}
	return map[string]any{"items": out, "limit": 200}, nil
}

func (r *Relay) recordUsage(t model.Task, data string) {
	var v struct {
		Input  *int64 `json:"inputTokens"`
		Output *int64 `json:"outputTokens"`
		Cached *int64 `json:"cachedInputTokens"`
	}
	if len(data) > 1024 || json.Unmarshal([]byte(data), &v) != nil || v.Input == nil || v.Output == nil || *v.Input < 0 || *v.Output < 0 || *v.Input > 1000000000000 || *v.Output > 1000000000000 {
		return
	}
	if v.Cached != nil && (*v.Cached < 0 || *v.Cached > *v.Input) {
		v.Cached = nil
	}
	_, _ = r.db.Exec("UPDATE run_usage SET input_tokens=?,output_tokens=?,cached_input_tokens=?,observed_at=? WHERE task_id=? AND attempt=?", *v.Input, *v.Output, v.Cached, now().Format(time.RFC3339Nano), t.ID, t.Attempts)
}
