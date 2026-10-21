package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var quotaAccount = regexp.MustCompile(`^Account [0-9]{1,3}$`)
var quotaIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_.:/ -]{1,100}$`)

func knownHarness(h string) bool {
	return h == "codex" || h == "omp" || h == "claude" || h == "opencode"
}
func quotaSource(h string) string {
	if h == "codex" {
		return "Codex account quota"
	}
	if h == "omp" {
		return "OMP native usage reports"
	}
	return "Not connected"
}
func (r *Relay) cachedQuota(h string) model.QuotaSnapshot {
	v, ok := r.quotaCache[h]
	if !ok {
		v = model.QuotaSnapshot{Harness: h, Source: quotaSource(h), State: "not_checked", Windows: []model.QuotaWindow{}}
	}
	if h != "codex" && h != "omp" {
		v.State = "not_supported"
		v.Message = "Provider quota is not connected for this harness."
	}
	if v.CheckedAt != nil && time.Since(*v.CheckedAt) > 60*time.Second {
		v.Stale = true
	}
	return v
}
func (r *Relay) quota(q model.ConsoleRequest) (any, error) {
	if !knownHarness(q.Harness) {
		return nil, model.Err(400, "unknown harness")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, model.Err(503, "relay is stopping")
	}
	old := r.cachedQuota(q.Harness)
	ad := r.adapters[q.Harness]
	if old.State == "not_supported" || (old.AttemptedAt != nil && time.Since(*old.AttemptedAt) < 15*time.Second) || (!q.Refresh && old.AttemptedAt != nil && time.Since(*old.AttemptedAt) < 60*time.Second) {
		r.mu.Unlock()
		return old, nil
	}
	r.mu.Unlock()
	// Share the discovery gate: quota refresh and model catalogs cannot multiply
	// native helper processes. Other callers retry instead of queuing goroutines.
	if !r.catalogMu.TryLock() {
		return nil, model.Err(409, "native status discovery is busy; try again shortly")
	}
	defer r.catalogMu.Unlock()
	r.mu.Lock()
	old = r.cachedQuota(q.Harness)
	if old.AttemptedAt != nil && time.Since(*old.AttemptedAt) < 15*time.Second {
		r.mu.Unlock()
		return old, nil
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, 25*time.Second)
	defer cancel()
	var windows []model.QuotaWindow
	var err error
	if ad.Quotas != nil {
		windows, err = ad.Quotas(ctx)
	} else if ad.Factory != nil {
		dir := filepath.Join(filepath.Dir(r.lock.Name()), "chat-workspaces", q.Harness)
		if err = os.MkdirAll(dir, 0700); err == nil {
			var rt model.Runtime
			rt, err = ad.Factory(ctx, model.Agent{ID: q.Harness + ":quota", Harness: q.Harness, Cwd: dir, Ephemeral: true}, nil)
			if err == nil {
				defer rt.Close()
				if reader, ok := rt.(model.QuotaReader); ok {
					windows, err = reader.Quotas(ctx)
				} else {
					err = model.Err(501, "quota reader unavailable")
				}
			}
		}
	} else {
		err = model.Err(503, "harness unavailable")
	}
	checked := now()
	next := old
	next.AttemptedAt = &checked
	next.Refreshing = false
	if err != nil {
		next.State = "unavailable"
		next.Stale = len(old.Windows) > 0
		next.Message = "Quota could not be refreshed. Check native sign-in or try again later."
	} else {
		next.Windows = sanitizeQuota(windows)
		next.CheckedAt = &checked
		next.Stale = false
		for _, w := range next.Windows {
			if w.ObservedAt != nil && time.Since(*w.ObservedAt) > 2*time.Minute {
				next.Stale = true
			}
		}
		next.Message = ""
		next.State = "reported"
		if len(next.Windows) == 0 {
			next.State = "unavailable"
			next.Message = "This account did not report quota windows."
		}
	}
	r.mu.Lock()
	r.quotaCache[q.Harness] = next
	r.mu.Unlock()
	return next, nil
}
func sanitizeQuota(in []model.QuotaWindow) []model.QuotaWindow {
	out := []model.QuotaWindow{}
	for _, w := range in {
		if len(out) >= 64 {
			break
		}
		if !quotaIdentifier.MatchString(w.Provider) || redact(w.Provider) != w.Provider {
			continue
		}
		// Return only normalized display metadata, never account IDs, emails, plan
		// metadata, credit identifiers or the native raw quota payload.
		w.ID = strconv.Itoa(len(out))
		if !quotaIdentifier.MatchString(w.Label) || redact(w.Label) != w.Label {
			w.Label = "Provider window"
		}
		if !quotaAccount.MatchString(w.Account) {
			w.Account = ""
		}
		if len(w.Account) > 20 {
			w.Account = ""
		}
		if w.UsedPercent != nil && (math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100000) {
			w.UsedPercent = nil
		}
		if w.Remaining != nil && (math.IsNaN(*w.Remaining) || math.IsInf(*w.Remaining, 0) || *w.Remaining < 0 || *w.Remaining > 1e15) {
			w.Remaining = nil
		}
		switch w.Unit {
		case "percent", "tokens", "requests", "credits", "usd", "minutes", "bytes":
		default:
			w.Unit = "unknown"
		}
		if w.ResetsAt != nil && (w.ResetsAt.Year() < 2000 || w.ResetsAt.Year() > 2200) {
			w.ResetsAt = nil
		}
		if w.ObservedAt != nil && (w.ObservedAt.Year() < 2000 || w.ObservedAt.After(now().Add(5*time.Minute))) {
			w.ObservedAt = nil
		}
		out = append(out, w)
	}
	return out
}
func (r *Relay) aiStatus(q model.ConsoleRequest) (any, error) {
	if q.ConversationID != "" {
		if err := r.member(model.Principal{Admin: true}, q.ConversationID, false); err != nil {
			return nil, err
		}
	}
	var latest struct {
		TaskID        string  `json:"taskId"`
		Attempt       int     `json:"attempt"`
		State         string  `json:"state"`
		Model         string  `json:"model"`
		Harness       string  `json:"harness"`
		Input         *int64  `json:"inputTokens"`
		Output        *int64  `json:"outputTokens"`
		Cached        *int64  `json:"cachedInputTokens"`
		ContextUsed   *int64  `json:"contextUsedTokens"`
		ContextWindow *int64  `json:"contextWindowTokens"`
		Estimated     *bool   `json:"estimated"`
		Source        *string `json:"contextSource"`
		ObservedAt    *string `json:"observedAt"`
	}
	if q.ConversationID != "" {
		err := r.db.QueryRow(`SELECT u.task_id,u.attempt,u.state,u.model,u.harness,u.input_tokens,u.output_tokens,u.cached_input_tokens,u.context_used_tokens,u.context_window_tokens,u.context_estimated,u.context_source,u.observed_at FROM run_usage u WHERE u.conversation_id=? ORDER BY u.started_at DESC,u.attempt DESC LIMIT 1`, q.ConversationID).Scan(&latest.TaskID, &latest.Attempt, &latest.State, &latest.Model, &latest.Harness, &latest.Input, &latest.Output, &latest.Cached, &latest.ContextUsed, &latest.ContextWindow, &latest.Estimated, &latest.Source, &latest.ObservedAt)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	var turns int
	if err := r.db.QueryRow("SELECT count(*) FROM run_usage WHERE started_at>=?", now().Format("2006-01-02")).Scan(&turns); err != nil {
		return nil, err
	}
	quotas := []model.QuotaSnapshot{}
	for _, h := range []string{"codex", "omp", "claude", "opencode"} {
		quotas = append(quotas, r.cachedQuota(h))
	}
	return map[string]any{"quotas": quotas, "latest": latest, "localBudget": map[string]any{"turns": turns, "limit": r.settings.DailyTurnLimit, "resetsAt": now().Truncate(24 * time.Hour).Add(24 * time.Hour)}, "contextNote": "Last native context snapshot, not a prediction for your next message. AI chats send bounded recent history on each turn."}, nil
}
func (r *Relay) usageHistory(q model.ConsoleRequest) (any, error) {
	if q.ConversationID != "" {
		if err := r.member(model.Principal{Admin: true}, q.ConversationID, false); err != nil {
			return nil, err
		}
	}
	if q.Harness != "" && !knownHarness(q.Harness) {
		return nil, model.Err(400, "unknown harness")
	}
	rows, err := r.db.Query(`SELECT substr(u.started_at,1,10),u.harness,u.model,count(*),coalesce(sum(u.input_tokens),0),coalesce(sum(u.output_tokens),0),coalesce(sum(u.cached_input_tokens),0),sum(u.input_tokens IS NOT NULL AND u.output_tokens IS NOT NULL),sum(u.cached_input_tokens IS NOT NULL) FROM run_usage u WHERE u.started_at>=? AND (?='' OR u.conversation_id=?) AND (?='' OR u.harness=?) GROUP BY substr(u.started_at,1,10),u.harness,u.model ORDER BY substr(u.started_at,1,10) DESC,u.harness,u.model LIMIT 101`, now().Add(-30*24*time.Hour).Format("2006-01-02"), q.ConversationID, q.ConversationID, q.Harness, q.Harness)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		Day            string `json:"day"`
		Harness        string `json:"harness"`
		Model          string `json:"model"`
		Turns          int    `json:"turns"`
		Input          int64  `json:"inputTokens"`
		Output         int64  `json:"outputTokens"`
		Cached         int64  `json:"cachedInputTokens"`
		Reported       int    `json:"reportedTurns"`
		CachedReported int    `json:"cachedReportedTurns"`
	}
	items := []item{}
	more := false
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.Day, &v.Harness, &v.Model, &v.Turns, &v.Input, &v.Output, &v.Cached, &v.Reported, &v.CachedReported); err != nil {
			return nil, err
		}
		if len(items) == 100 {
			more = true
			break
		}
		items = append(items, v)
	}
	return map[string]any{"items": items, "hasMore": more, "retentionDays": 30, "timezone": "UTC"}, rows.Err()
}
func (r *Relay) recordContext(t model.Task, data string) {
	var v struct {
		UsedTokens   *int64 `json:"usedTokens"`
		WindowTokens int64  `json:"windowTokens"`
		Estimated    bool   `json:"estimated"`
		Source       string `json:"source"`
	}
	if len(data) > 1024 || json.Unmarshal([]byte(data), &v) != nil || v.UsedTokens == nil || *v.UsedTokens < 0 || *v.UsedTokens > 1e12 || v.WindowTokens <= 0 || v.WindowTokens > 1e12 || len(v.Source) > 120 || !safeRegistryText(v.Source) || redact(v.Source) != v.Source {
		return
	}
	_, _ = r.db.Exec(`UPDATE run_usage SET context_used_tokens=?,context_window_tokens=?,context_estimated=?,context_source=?,observed_at=? WHERE task_id=? AND attempt=?`, *v.UsedTokens, v.WindowTokens, v.Estimated, v.Source, now().Format(time.RFC3339Nano), t.ID, t.Attempts)
}
