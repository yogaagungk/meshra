package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"meshra/internal/model"
)

const (
	hourMS = int64(time.Hour / time.Millisecond)
	dayMS  = 24 * hourMS
)

type statsFilter struct {
	RangeHours      int    `json:"rangeHours"`
	Harness         string `json:"harness"`
	Project         string `json:"project"`
	TZOffsetMinutes int    `json:"tzOffsetMinutes"`
}

func (f *statsFilter) validate() error {
	if f.RangeHours == 0 {
		f.RangeHours = 24
	}
	if f.RangeHours != 24 && f.RangeHours != 168 && f.RangeHours != 720 {
		return model.Err(400, "rangeHours must be 24, 168 or 720")
	}
	switch f.Harness {
	case "", "openai", "omp", "claude":
	default:
		return model.Err(400, "unknown harness")
	}
	if len(f.Project) > 300 || f.TZOffsetMinutes < -14*60 || f.TZOffsetMinutes > 14*60 {
		return model.Err(400, "invalid stats filter")
	}
	return nil
}

func (f statsFilter) window() (from, to, bucket int64) {
	to = now().UnixMilli()
	from = to - int64(f.RangeHours)*hourMS
	bucket = hourMS
	if f.RangeHours > 168 {
		bucket = dayMS
	}
	return
}

// where is the shared predicate for the range, harness and project filters.
func (f statsFilter) where(from, to int64) (string, []any) {
	return ` WHERE ts>=? AND ts<? AND (?='' OR harness=?) AND (?='' OR project=?)`, []any{from, to, f.Harness, f.Harness, f.Project, f.Project}
}

func (r *Relay) statsOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	switch operation {
	case "stats.summary":
		var q statsFilter
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if err := q.validate(); err != nil {
			return nil, err
		}
		return r.statsSummary(ctx, q)
	case "stats.requests":
		var q struct {
			statsFilter
			Status   string `json:"status"`
			Search   string `json:"search"`
			BeforeTS int64  `json:"beforeTs"`
			BeforeID int64  `json:"beforeId"`
			Limit    int    `json:"limit"`
		}
		if err := decodeAutomation(raw, &q); err != nil {
			return nil, err
		}
		if err := q.validate(); err != nil {
			return nil, err
		}
		return r.statsRequests(ctx, q.statsFilter, q.Status, q.Search, q.BeforeTS, q.BeforeID, q.Limit)
	case "stats.refresh":
		select {
		case statsKick <- struct{}{}:
		default:
		}
		return map[string]any{"importing": true}, nil
	}
	return nil, model.Err(404, "unknown stats operation")
}

type nf = sql.NullFloat64

func nullable(v nf) *float64 {
	if v.Valid && !math.IsNaN(v.Float64) {
		return &v.Float64
	}
	return nil
}

func (r *Relay) statsSummary(ctx context.Context, f statsFilter) (any, error) {
	from, to, bucket := f.window()
	where, args := f.where(from, to)
	tz := int64(f.TZOffsetMinutes) * int64(time.Minute/time.Millisecond)
	bucketExpr := `((ts+?)/?)*?-?`
	bucketArgs := []any{tz, bucket, bucket, tz}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	q := func(query string, extra []any, scan func(*sql.Rows) error) error {
		rows, err := r.db.QueryContext(ctx, query, append(extra, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err = scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	tokensExpr := `(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens)`
	aggregate := `count(*),coalesce(sum(status='failed'),0),coalesce(sum(status='aborted'),0),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(cache_read_tokens),0),coalesce(sum(cache_write_tokens),0),coalesce(sum(premium),0),sum(cost),count(cost),avg(duration_ms),avg(ttft_ms),avg(CASE WHEN duration_ms>0 AND output_tokens>0 THEN output_tokens*1000.0/duration_ms END)`
	type totals struct {
		Requests       int64    `json:"requests"`
		Failed         int64    `json:"failed"`
		Aborted        int64    `json:"aborted"`
		Input          int64    `json:"input"`
		Output         int64    `json:"output"`
		CacheRead      int64    `json:"cacheRead"`
		CacheWrite     int64    `json:"cacheWrite"`
		Premium        float64  `json:"premium"`
		Cost           *float64 `json:"cost"`
		PricedRequests int64    `json:"pricedRequests"`
		AvgDurationMs  *float64 `json:"avgDurationMs"`
		AvgTTFTMs      *float64 `json:"avgTtftMs"`
		TokensPerSec   *float64 `json:"tokensPerSec"`
	}
	scanTotals := func(rows *sql.Rows, t *totals, lead ...any) error {
		var cost, dur, ttft, tps nf
		dest := append(lead, &t.Requests, &t.Failed, &t.Aborted, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Premium, &cost, &t.PricedRequests, &dur, &ttft, &tps)
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		t.Cost, t.AvgDurationMs, t.AvgTTFTMs, t.TokensPerSec = nullable(cost), nullable(dur), nullable(ttft), nullable(tps)
		return nil
	}
	out := map[string]any{"from": from, "to": to, "bucketMs": bucket, "tzOffsetMinutes": f.TZOffsetMinutes, "importing": statsImporting.Load(), "retentionDays": 30}

	var total totals
	if err := q(`SELECT `+aggregate+` FROM model_requests`+where, nil, func(rows *sql.Rows) error { return scanTotals(rows, &total) }); err != nil {
		return nil, err
	}
	out["totals"] = total

	type point struct {
		T        int64    `json:"t"`
		Requests int64    `json:"requests"`
		Failed   int64    `json:"failed"`
		Tokens   int64    `json:"tokens"`
		Cost     *float64 `json:"cost"`
	}
	series := []point{}
	if err := q(`SELECT `+bucketExpr+` AS b,count(*),coalesce(sum(status='failed'),0),coalesce(sum(`+tokensExpr+`),0),sum(cost) FROM model_requests`+where+` GROUP BY b ORDER BY b`, bucketArgs, func(rows *sql.Rows) error {
		var v point
		var cost nf
		if err := rows.Scan(&v.T, &v.Requests, &v.Failed, &v.Tokens, &cost); err != nil {
			return err
		}
		v.Cost = nullable(cost)
		series = append(series, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["series"] = series

	type agent struct {
		Agent    string `json:"agent"`
		Requests int64  `json:"requests"`
		Tokens   int64  `json:"tokens"`
	}
	agents := []agent{}
	if err := q(`SELECT agent_type,count(*),coalesce(sum(`+tokensExpr+`),0) FROM model_requests`+where+` GROUP BY agent_type ORDER BY 3 DESC`, nil, func(rows *sql.Rows) error {
		var v agent
		if err := rows.Scan(&v.Agent, &v.Requests, &v.Tokens); err != nil {
			return err
		}
		agents = append(agents, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["agents"] = agents

	type modelRow struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Harness  string `json:"harness"`
		totals
	}
	models := []modelRow{}
	if err := q(`SELECT model,provider,harness,`+aggregate+` FROM model_requests`+where+` GROUP BY model,provider,harness ORDER BY 4 DESC LIMIT 200`, nil, func(rows *sql.Rows) error {
		var v modelRow
		if err := scanTotals(rows, &v.totals, &v.Model, &v.Provider, &v.Harness); err != nil {
			return err
		}
		models = append(models, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["models"] = models

	type modelPoint struct {
		T        int64  `json:"t"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Harness  string `json:"harness"`
		Requests int64  `json:"requests"`
	}
	modelSeries := []modelPoint{}
	if err := q(`SELECT `+bucketExpr+` AS b,model,provider,harness,count(*) FROM model_requests`+where+` GROUP BY b,model,provider,harness ORDER BY b`, bucketArgs, func(rows *sql.Rows) error {
		var v modelPoint
		if err := rows.Scan(&v.T, &v.Model, &v.Provider, &v.Harness, &v.Requests); err != nil {
			return err
		}
		modelSeries = append(modelSeries, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["modelSeries"] = modelSeries

	type providerRow struct {
		Provider string `json:"provider"`
		Models   int64  `json:"models"`
		totals
	}
	providers := []providerRow{}
	if err := q(`SELECT provider,count(DISTINCT model),`+aggregate+` FROM model_requests`+where+` GROUP BY provider ORDER BY `+`sum(`+tokensExpr+`) DESC LIMIT 50`, nil, func(rows *sql.Rows) error {
		var v providerRow
		if err := scanTotals(rows, &v.totals, &v.Provider, &v.Models); err != nil {
			return err
		}
		providers = append(providers, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["providers"] = providers

	type providerPoint struct {
		T        int64    `json:"t"`
		Provider string   `json:"provider"`
		Requests int64    `json:"requests"`
		Tokens   int64    `json:"tokens"`
		Cost     *float64 `json:"cost"`
	}
	providerSeries := []providerPoint{}
	if err := q(`SELECT `+bucketExpr+` AS b,provider,count(*),coalesce(sum(`+tokensExpr+`),0),sum(cost) FROM model_requests`+where+` GROUP BY b,provider ORDER BY b`, bucketArgs, func(rows *sql.Rows) error {
		var v providerPoint
		var cost nf
		if err := rows.Scan(&v.T, &v.Provider, &v.Requests, &v.Tokens, &cost); err != nil {
			return err
		}
		v.Cost = nullable(cost)
		providerSeries = append(providerSeries, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["providerSeries"] = providerSeries

	// Tokens by local hour of day, per provider, for the peak-hours chart.
	type hourPoint struct {
		Hour     int    `json:"hour"`
		Provider string `json:"provider"`
		Tokens   int64  `json:"tokens"`
	}
	hours := []hourPoint{}
	if err := q(`SELECT (((ts+?)/?)%24) AS h,provider,coalesce(sum(`+tokensExpr+`),0) FROM model_requests`+where+` GROUP BY h,provider`, []any{tz, hourMS}, func(rows *sql.Rows) error {
		var v hourPoint
		if err := rows.Scan(&v.Hour, &v.Provider, &v.Tokens); err != nil {
			return err
		}
		hours = append(hours, v)
		return nil
	}); err != nil {
		return nil, err
	}
	out["hourOfDay"] = hours

	// Filter choices ignore the harness/project filters so they stay selectable.
	type choice struct {
		Name     string `json:"name"`
		Requests int64  `json:"requests"`
	}
	choices := func(column string) ([]choice, error) {
		list := []choice{}
		rows, err := r.db.QueryContext(ctx, `SELECT `+column+`,count(*) FROM model_requests WHERE ts>=? AND ts<? GROUP BY 1 ORDER BY 2 DESC LIMIT 100`, from, to)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var v choice
			if err = rows.Scan(&v.Name, &v.Requests); err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, rows.Err()
	}
	var err error
	if out["harnesses"], err = choices("harness"); err != nil {
		return nil, err
	}
	if out["projects"], err = choices("project"); err != nil {
		return nil, err
	}

	type sourceState struct {
		Source    string `json:"source"`
		Available bool   `json:"available"`
		LastRunAt int64  `json:"lastRunAt"`
		Error     string `json:"error,omitempty"`
		Requests  int64  `json:"requests"`
	}
	sources := []sourceState{}
	rows, err := r.db.QueryContext(ctx, `SELECT s.source,s.available,s.last_run_at,s.last_error,(SELECT count(*) FROM model_requests m WHERE m.source=s.source) FROM stats_import_state s ORDER BY s.source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v sourceState
		var available int
		if err = rows.Scan(&v.Source, &available, &v.LastRunAt, &v.Error, &v.Requests); err != nil {
			return nil, err
		}
		v.Available = available == 1
		sources = append(sources, v)
	}
	out["sources"] = sources
	return out, rows.Err()
}

func percentile(sorted []int64, p float64) *int64 {
	if len(sorted) == 0 {
		return nil
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return &sorted[i]
}

func likePattern(search string) string {
	search = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
	return "%" + search + "%"
}

func (r *Relay) statsRequests(ctx context.Context, f statsFilter, status, search string, beforeTS, beforeID int64, limit int) (any, error) {
	switch status {
	case "", "all", "ok", "aborted", "failed":
	default:
		return nil, model.Err(400, "status must be all, ok, aborted or failed")
	}
	if len(search) > 100 {
		return nil, model.Err(400, "search is too long")
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 {
		return nil, model.Err(400, "limit must be 1..200")
	}
	from, to, _ := f.window()
	where, args := f.where(from, to)
	if search != "" {
		pattern := likePattern(search)
		where += ` AND (model LIKE ? ESCAPE '\' OR provider LIKE ? ESCAPE '\' OR project LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern, pattern)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, model.Err(503, "relay is stopping")
	}
	out := map[string]any{"from": from, "to": to, "importing": statsImporting.Load()}

	// Counts and latency ignore the status tab so the tabs keep their totals.
	var all, okCount, aborted, failed, tokens, firstTS int64
	var cost nf
	if err := r.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(status='ok'),0),coalesce(sum(status='aborted'),0),coalesce(sum(status='failed'),0),coalesce(sum(input_tokens+output_tokens+cache_read_tokens+cache_write_tokens),0),sum(cost),coalesce(min(ts),0) FROM model_requests`+where, args...).Scan(&all, &okCount, &aborted, &failed, &tokens, &cost, &firstTS); err != nil {
		return nil, err
	}
	durations, err := r.statsSorted(ctx, `duration_ms`, where, args)
	if err != nil {
		return nil, err
	}
	ttfts, err := r.statsSorted(ctx, `ttft_ms`, where, args)
	if err != nil {
		return nil, err
	}
	out["summary"] = map[string]any{
		"requests": all, "ok": okCount, "aborted": aborted, "failed": failed, "tokens": tokens, "cost": nullable(cost), "firstTs": firstTS,
		"medianDurationMs": percentile(durations, 0.5), "p95DurationMs": percentile(durations, 0.95),
		"medianTtftMs": percentile(ttfts, 0.5), "p95TtftMs": percentile(ttfts, 0.95),
	}

	query := `SELECT id,ts,harness,model,provider,project,agent_type,status,stop_reason,error,duration_ms,ttft_ms,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,cost,compacted FROM model_requests` + where
	if status != "" && status != "all" {
		query += ` AND status=?`
		args = append(args, status)
	}
	if beforeTS > 0 {
		query += ` AND (ts<? OR (ts=? AND id<?))`
		args = append(args, beforeTS, beforeTS, beforeID)
	}
	query += ` ORDER BY ts DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		ID         int64    `json:"id"`
		TS         int64    `json:"ts"`
		Harness    string   `json:"harness"`
		Model      string   `json:"model"`
		Provider   string   `json:"provider"`
		Project    string   `json:"project"`
		Agent      string   `json:"agent"`
		Status     string   `json:"status"`
		StopReason string   `json:"stopReason"`
		Error      string   `json:"error,omitempty"`
		DurationMs *int64   `json:"durationMs"`
		TTFTMs     *int64   `json:"ttftMs"`
		Input      int64    `json:"input"`
		CacheRead  int64    `json:"cacheRead"`
		CacheWrite int64    `json:"cacheWrite"`
		Output     int64    `json:"output"`
		Cost       *float64 `json:"cost"`
		Compacted  bool     `json:"compacted"`
	}
	items := []item{}
	more := false
	for rows.Next() {
		var v item
		var dur, ttft sql.NullInt64
		var c nf
		if err = rows.Scan(&v.ID, &v.TS, &v.Harness, &v.Model, &v.Provider, &v.Project, &v.Agent, &v.Status, &v.StopReason, &v.Error, &dur, &ttft, &v.Input, &v.CacheRead, &v.CacheWrite, &v.Output, &c, &v.Compacted); err != nil {
			return nil, err
		}
		if len(items) == limit {
			more = true
			break
		}
		if dur.Valid {
			v.DurationMs = &dur.Int64
		}
		if ttft.Valid {
			v.TTFTMs = &ttft.Int64
		}
		v.Cost = nullable(c)
		items = append(items, v)
	}
	out["items"], out["hasMore"] = items, more
	return out, rows.Err()
}

func (r *Relay) statsSorted(ctx context.Context, column, where string, args []any) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+column+` FROM model_requests`+where+` AND `+column+` IS NOT NULL ORDER BY `+column+` LIMIT 200000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []int64{}
	for rows.Next() {
		var v int64
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values, rows.Err()
}

// recordModelRequest stores one Direct API model call reported by the adapter.
// The caller holds relay ownership.
func (r *Relay) recordModelRequest(t model.Task, data string) {
	var v struct {
		Seq        int    `json:"seq"`
		Model      string `json:"model"`
		Provider   string `json:"provider"`
		DurationMs *int64 `json:"durationMs"`
		TTFTMs     *int64 `json:"ttftMs"`
		StopReason string `json:"stopReason"`
		Error      string `json:"error"`
		Input      int64  `json:"inputTokens"`
		Output     int64  `json:"outputTokens"`
		Cached     int64  `json:"cachedInputTokens"`
		CacheWrite int64  `json:"cacheWriteInputTokens"`
		Compacted  bool   `json:"compacted"`
	}
	if len(data) > 4096 || json.Unmarshal([]byte(data), &v) != nil {
		return
	}
	for _, n := range []int64{v.Input, v.Output, v.Cached, v.CacheWrite} {
		if n < 0 || n > 1e12 {
			return
		}
	}
	for _, d := range []*int64{v.DurationMs, v.TTFTMs} {
		if d != nil && (*d < 0 || *d > int64(24*time.Hour/time.Millisecond)) {
			return
		}
	}
	if v.Cached > v.Input {
		v.Cached = 0
	}
	if v.CacheWrite > v.Input-v.Cached {
		v.CacheWrite = 0
	}
	provider := v.Provider
	var body string
	if r.db.QueryRow(`SELECT body FROM provider_endpoints WHERE id=?`, provider).Scan(&body) == nil {
		var endpoint struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(body), &endpoint) == nil && endpoint.Name != "" {
			provider = endpoint.Name
		}
	}
	row := statsRow{source: "meshra", harness: "openai", extID: fmt.Sprintf("%s/%d/%d", t.ID, t.Attempts, v.Seq), ts: now().UnixMilli(),
		model: statsText(v.Model, 200), provider: statsText(provider, 200), project: statsText(t.To, 300), agentType: "main",
		stopReason: statsText(v.StopReason, 40), errMs: statsText(v.Error, 300), duration: v.DurationMs, ttft: v.TTFTMs,
		input: v.Input - v.Cached - v.CacheWrite, output: v.Output, cacheRead: v.Cached, cacheWrite: v.CacheWrite}
	if row.model == "" {
		row.model = "unknown"
	}
	if row.provider == "" {
		row.provider = "unknown"
	}
	row.status = statsStatus(row.stopReason, row.errMs)
	_, _ = r.db.Exec(`INSERT OR IGNORE INTO model_requests(source,harness,ext_id,ts,model,provider,project,agent_type,status,stop_reason,error,duration_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,premium,cost,compacted) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?)`,
		row.source, row.harness, row.extID, row.ts, row.model, row.provider, row.project, row.agentType, row.status, row.stopReason, row.errMs, row.duration, row.ttft, row.input, row.output, row.cacheRead, row.cacheWrite, v.Compacted)
}
