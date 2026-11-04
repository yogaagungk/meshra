package relay

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	statsRetention   = 30 * 24 * time.Hour
	statsBatch       = 1000
	statsMaxLine     = 8 << 20
	statsImportEvery = 60 * time.Second
)

// statsRow is one model request in the common shape every source maps to.
type statsRow struct {
	source, harness, extID                                         string
	ts                                                             int64
	model, provider, project, agentType, status, stopReason, errMs string
	duration, ttft                                                 *int64
	input, output, cacheRead, cacheWrite                           int64
	premium                                                        float64
	cost                                                           *float64
}

var statsImporting atomic.Bool

// statsKick asks the stats loop to import now. It never blocks.
var statsKick = make(chan struct{}, 1)

func (r *Relay) statsLoop() {
	defer r.wg.Done()
	if r.options.StatsOMPDB == "" && r.options.StatsClaudeDir == "" {
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
		case <-statsKick:
		}
		r.runStatsImport(r.ctx)
		timer.Reset(statsImportEvery)
	}
}

// runStatsImport imports every configured source once. A source failure is
// recorded for the dashboard and never stops the others.
func (r *Relay) runStatsImport(ctx context.Context) {
	if !statsImporting.CompareAndSwap(false, true) {
		return
	}
	defer statsImporting.Store(false)
	if r.options.StatsOMPDB != "" {
		r.finishStatsSource("omp", r.importOMP(ctx))
	}
	if r.options.StatsClaudeDir != "" {
		r.finishStatsSource("claude", r.importClaude(ctx))
	}
	r.pruneStats()
}

func (r *Relay) finishStatsSource(source string, err error) {
	available := err == nil
	message := ""
	if err != nil {
		message = err.Error()
		var missing *fs.PathError
		if errors.As(err, &missing) && errors.Is(err, fs.ErrNotExist) {
			message = "source not found on this machine"
		}
		available = false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	_, _ = r.db.Exec(`INSERT INTO stats_import_state(source,last_run_at,last_error,available) VALUES(?,?,?,?)
 ON CONFLICT(source) DO UPDATE SET last_run_at=excluded.last_run_at,last_error=excluded.last_error,available=excluded.available`, source, now().UnixMilli(), truncate(message, 200), boolInt(available))
}

func (r *Relay) pruneStats() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	_, _ = r.db.Exec(`DELETE FROM model_requests WHERE ts<?`, now().Add(-statsRetention-24*time.Hour).UnixMilli())
}

// insertStatsRows upserts rows and, in the same transaction, saves the import
// position so a crash never skips or repeats a batch.
func (r *Relay) insertStatsRows(rows []statsRow, save func(tx *sql.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("relay is stopping")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO model_requests(source,harness,ext_id,ts,model,provider,project,agent_type,status,stop_reason,error,duration_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,premium,cost)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(source,ext_id) DO UPDATE SET ts=excluded.ts,model=excluded.model,provider=excluded.provider,project=excluded.project,agent_type=excluded.agent_type,status=excluded.status,stop_reason=excluded.stop_reason,error=excluded.error,duration_ms=excluded.duration_ms,ttft_ms=excluded.ttft_ms,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_write_tokens=excluded.cache_write_tokens,premium=excluded.premium,cost=excluded.cost`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err = stmt.Exec(row.source, row.harness, row.extID, row.ts, row.model, row.provider, row.project, row.agentType, row.status, row.stopReason, row.errMs, row.duration, row.ttft, row.input, row.output, row.cacheRead, row.cacheWrite, row.premium, row.cost); err != nil {
			return err
		}
	}
	if save != nil {
		if err = save(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Relay) statsCursor(source string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cursor string
	_ = r.db.QueryRow(`SELECT cursor FROM stats_import_state WHERE source=?`, source).Scan(&cursor)
	return cursor
}

func statsStatus(stopReason, errText string) string {
	switch {
	case stopReason == "aborted" || stopReason == "cancelled":
		// A user stop is not a failure even when the runtime attaches a message.
		return "aborted"
	case errText != "" || stopReason == "error":
		return "failed"
	}
	return "ok"
}

func statsText(value string, limit int) string {
	return truncate(redact(strings.TrimSpace(value)), limit)
}

// importOMP copies new rows from the OMP stats database, read-only. The OMP
// file is never written and its schema is not changed.
func (r *Relay) importOMP(ctx context.Context) error {
	path := r.options.StatsOMPDB
	if _, err := os.Stat(path); err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(3000)"}).String()
	source, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	var cursor int64
	if text := r.statsCursor("omp"); text != "" {
		_ = json.Unmarshal([]byte(text), &cursor)
	}
	cutoff := now().Add(-statsRetention).UnixMilli()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		rows, err := source.QueryContext(ctx, `SELECT id,session_file,entry_id,folder,model,provider,timestamp,duration,ttft,stop_reason,coalesce(error_message,''),input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,premium_requests,cost_total,cost_unpriced,agent_type FROM messages WHERE id>? AND timestamp>=? ORDER BY id LIMIT ?`, cursor, cutoff, statsBatch)
		if err != nil {
			return err
		}
		var batch []statsRow
		last := cursor
		for rows.Next() {
			var id, ts, unpriced int64
			var session, entry, folder, mdl, provider, stop, errText, agent string
			var duration, ttft sql.NullFloat64
			var cost float64
			var row statsRow
			if err = rows.Scan(&id, &session, &entry, &folder, &mdl, &provider, &ts, &duration, &ttft, &stop, &errText, &row.input, &row.output, &row.cacheRead, &row.cacheWrite, &row.premium, &cost, &unpriced, &agent); err != nil {
				rows.Close()
				return err
			}
			last = id
			row.source, row.harness, row.extID = "omp", "omp", session+":"+entry
			row.ts, row.model, row.provider = ts, statsText(mdl, 200), statsText(provider, 200)
			row.project, row.agentType = statsText(folder, 300), statsText(agent, 40)
			row.stopReason, row.errMs = statsText(stop, 40), statsText(errText, 300)
			row.status = statsStatus(row.stopReason, row.errMs)
			// OMP stores timings as REAL milliseconds.
			if duration.Valid && duration.Float64 >= 0 {
				v := int64(math.Round(duration.Float64))
				row.duration = &v
			}
			if ttft.Valid && ttft.Float64 >= 0 {
				v := int64(math.Round(ttft.Float64))
				row.ttft = &v
			}
			if unpriced == 0 {
				row.cost = &cost
			}
			batch = append(batch, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		value, _ := json.Marshal(last)
		if err = r.insertStatsRows(batch, func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT INTO stats_import_state(source,cursor) VALUES('omp',?) ON CONFLICT(source) DO UPDATE SET cursor=excluded.cursor`, string(value))
			return e
		}); err != nil {
			return err
		}
		cursor = last
		if len(batch) < statsBatch {
			return nil
		}
	}
}

type claudeLine struct {
	Type              string `json:"type"`
	Timestamp         string `json:"timestamp"`
	Cwd               string `json:"cwd"`
	IsSidechain       bool   `json:"isSidechain"`
	RequestID         string `json:"requestId"`
	UUID              string `json:"uuid"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Message           struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parseClaudeLine maps one Claude session log line to a request row. Only
// assistant messages that report usage count; the log has no latency or price.
func parseClaudeLine(line []byte, home string) (statsRow, bool) {
	var row statsRow
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return row, false
	}
	var d claudeLine
	if json.Unmarshal(line, &d) != nil || d.Type != "assistant" || d.Message.Usage == nil || d.Message.Model == "" || d.Message.Model == "<synthetic>" {
		return row, false
	}
	at, err := time.Parse(time.RFC3339Nano, d.Timestamp)
	if err != nil {
		return row, false
	}
	id := d.Message.ID
	if id == "" {
		id = d.RequestID
	}
	if id == "" {
		id = d.UUID
	}
	if id == "" {
		return row, false
	}
	u := d.Message.Usage
	for _, n := range []int64{u.Input, u.Output, u.CacheRead, u.CacheCreation} {
		if n < 0 || n > 1e12 {
			return row, false
		}
	}
	project := d.Cwd
	if home != "" && strings.HasPrefix(project, home) {
		project = "~" + strings.TrimPrefix(project, home)
	}
	row = statsRow{source: "claude", harness: "claude", extID: id, ts: at.UnixMilli(), model: statsText(d.Message.Model, 200), provider: "anthropic", project: statsText(project, 300), agentType: "main", stopReason: statsText(d.Message.StopReason, 40), input: u.Input, output: u.Output, cacheRead: u.CacheRead, cacheWrite: u.CacheCreation}
	if d.IsSidechain {
		row.agentType = "subagent"
	}
	row.status = statsStatus(row.stopReason, "")
	if d.IsAPIErrorMessage {
		row.status, row.stopReason = "failed", "error"
	}
	return row, true
}

// importClaude reads new complete lines from each recent session log. Only
// usage numbers, timestamps, model and project path are kept; message text is
// never read into the database.
func (r *Relay) importClaude(ctx context.Context) error {
	root := r.options.StatsClaudeDir
	if _, err := os.Stat(root); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	cutoff := now().Add(-statsRetention)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			if info, e := entry.Info(); e == nil && info.ModTime().After(cutoff) {
				files = append(files, path)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		if err = r.importClaudeFile(ctx, path, home); err != nil {
			return err
		}
	}
	return nil
}

func (r *Relay) importClaudeFile(ctx context.Context, path, home string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	var offset, mtime int64
	r.mu.Lock()
	_ = r.db.QueryRow(`SELECT offset,mtime FROM stats_import_files WHERE path=?`, path).Scan(&offset, &mtime)
	r.mu.Unlock()
	if info.Size() < offset {
		offset = 0
	}
	if info.Size() == offset && info.ModTime().UnixMilli() == mtime {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(file, 256<<10)
	var batch []statsRow
	consumed := offset
	flush := func() error {
		position := consumed
		return r.insertStatsRows(batch, func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT INTO stats_import_files(path,offset,mtime) VALUES(?,?,?) ON CONFLICT(path) DO UPDATE SET offset=excluded.offset,mtime=excluded.mtime`, path, position, info.ModTime().UnixMilli())
			return e
		})
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			// A trailing line without a newline may still be written; leave it.
			break
		}
		consumed += int64(len(line))
		if len(line) > statsMaxLine {
			continue
		}
		if row, ok := parseClaudeLine(line, home); ok {
			batch = append(batch, row)
			if len(batch) >= statsBatch {
				if err = flush(); err != nil {
					return err
				}
				batch = batch[:0]
			}
		}
	}
	return flush()
}
