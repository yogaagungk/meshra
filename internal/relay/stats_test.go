package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

func statsCall(t *testing.T, r *Relay, operation string, payload any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.statsOperation(context.Background(), admin, operation, raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func number(t *testing.T, value any) float64 {
	t.Helper()
	n, ok := value.(float64)
	if !ok {
		t.Fatalf("expected a number, got %v", value)
	}
	return n
}

func fakeOMPStats(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,session_file TEXT,entry_id TEXT,folder TEXT,model TEXT,provider TEXT,api TEXT,timestamp INTEGER,duration INTEGER,ttft INTEGER,stop_reason TEXT,error_message TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,total_tokens INTEGER,premium_requests REAL,cost_total REAL,cost_unpriced INTEGER,agent_type TEXT)`); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func addOMPMessage(t *testing.T, db *sql.DB, entry string, at time.Time, stop, errText string, input, output, cacheRead int64, unpriced int, agent string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages(session_file,entry_id,folder,model,provider,api,timestamp,duration,ttft,stop_reason,error_message,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,total_tokens,premium_requests,cost_total,cost_unpriced,agent_type) VALUES('s.jsonl',?,'-proj','gpt-x','openai-codex','x',?,?,?,?,?,?,?,?,0,0,0,0.5,?,?)`,
		entry, at.UnixMilli(), 2000.4, 500.2, stop, errText, input, output, cacheRead, unpriced, agent); err != nil {
		t.Fatal(err)
	}
}

func TestStatsImportOMPIsIncrementalAndSummarized(t *testing.T) {
	r, _ := openTest(t, nil)
	path, db := fakeOMPStats(t)
	r.options.StatsOMPDB = path
	at := now().Add(-30 * time.Minute)
	addOMPMessage(t, db, "a", at, "toolUse", "", 100, 40, 900, 0, "main")
	addOMPMessage(t, db, "b", at.Add(time.Minute), "stop", "", 100, 60, 900, 0, "subagent")
	addOMPMessage(t, db, "c", at.Add(2*time.Minute), "aborted", "Request was aborted", 0, 0, 0, 1, "main")
	addOMPMessage(t, db, "d", at.Add(3*time.Minute), "error", "boom", 0, 0, 0, 1, "subagent")
	r.runStatsImport(context.Background())
	r.runStatsImport(context.Background()) // idempotent
	summary := statsCall(t, r, "stats.summary", map[string]any{"rangeHours": 24})
	totals := summary["totals"].(map[string]any)
	if number(t, totals["requests"]) != 4 || number(t, totals["failed"]) != 1 || number(t, totals["aborted"]) != 1 {
		t.Fatalf("unexpected totals: %v", totals)
	}
	if number(t, totals["input"]) != 200 || number(t, totals["output"]) != 100 || number(t, totals["cacheRead"]) != 1800 {
		t.Fatalf("unexpected token totals: %v", totals)
	}
	// Two unpriced rows are excluded from the cost; the priced ones sum to 1.0.
	if number(t, totals["cost"]) != 1 || number(t, totals["pricedRequests"]) != 2 {
		t.Fatalf("unpriced rows must not count as zero cost: %v", totals)
	}
	if number(t, totals["avgTtftMs"]) != 500 || number(t, totals["avgDurationMs"]) != 2000 {
		t.Fatalf("unexpected latency: %v", totals)
	}
	agents := summary["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agent split missing: %v", agents)
	}
	if models := summary["models"].([]any); len(models) != 1 || models[0].(map[string]any)["model"] != "gpt-x" || number(t, models[0].(map[string]any)["requests"]) != 4 {
		t.Fatalf("unexpected models: %v", models)
	}
	sources := summary["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["available"] != true {
		t.Fatalf("source state missing: %v", sources)
	}
	// Only rows added after the cursor are imported on the next run.
	addOMPMessage(t, db, "e", at.Add(4*time.Minute), "stop", "", 10, 5, 0, 0, "main")
	r.runStatsImport(context.Background())
	if got := number(t, statsCall(t, r, "stats.summary", map[string]any{})["totals"].(map[string]any)["requests"]); got != 5 {
		t.Fatalf("incremental import failed: %v", got)
	}
}

func TestStatsMissingSourceIsReportedNotFatal(t *testing.T) {
	r, _ := openTest(t, nil)
	r.options.StatsOMPDB = filepath.Join(t.TempDir(), "absent.db")
	r.runStatsImport(context.Background())
	sources := statsCall(t, r, "stats.summary", map[string]any{})["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["available"] != false || sources[0].(map[string]any)["error"] == "" {
		t.Fatalf("a missing source must be reported: %v", sources)
	}
}

func claudeFixture(id, at, model string, sidechain bool, input, output, cacheRead, cacheWrite int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/proj","isSidechain":%v,"message":{"id":%q,"model":%q,"stop_reason":"end_turn","content":"secret text","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`, at, sidechain, id, model, input, output, cacheRead, cacheWrite) + "\n"
}

func TestStatsImportClaudeLogs(t *testing.T) {
	r, _ := openTest(t, nil)
	dir := t.TempDir()
	project := filepath.Join(dir, "-work-proj")
	if err := os.MkdirAll(filepath.Join(project, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	at := now().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	file := filepath.Join(project, "s.jsonl")
	unfinished := strings.TrimSuffix(claudeFixture("partial", at, "claude-x", false, 1, 1, 0, 0), "\n")
	content := claudeFixture("m1", at, "claude-x", false, 2, 10, 500, 100) +
		claudeFixture("m1", at, "claude-x", false, 2, 50, 500, 100) + // streamed update of m1
		claudeFixture("m2", at, "claude-x", true, 3, 20, 0, 0) +
		claudeFixture("m3", at, "<synthetic>", false, 1, 1, 1, 1) +
		`{"type":"user","message":{"content":"hello usage"}}` + "\n" +
		unfinished[:40] // a line still being written
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r.options.StatsClaudeDir = dir
	r.runStatsImport(context.Background())
	totals := statsCall(t, r, "stats.summary", map[string]any{})["totals"].(map[string]any)
	if number(t, totals["requests"]) != 2 || number(t, totals["output"]) != 70 || number(t, totals["cacheRead"]) != 500 || number(t, totals["cacheWrite"]) != 100 {
		t.Fatalf("a streamed message must count once with its final usage: %v", totals)
	}
	if totals["cost"] != nil || totals["avgDurationMs"] != nil {
		t.Fatalf("Claude logs carry no price or latency: %v", totals)
	}
	// Finishing the trailing line imports it on the next run without repeats.
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(unfinished[40:] + "\n")
	f.Close()
	r.runStatsImport(context.Background())
	if got := number(t, statsCall(t, r, "stats.summary", map[string]any{})["totals"].(map[string]any)["requests"]); got != 3 {
		t.Fatalf("the completed line was not imported exactly once: %v", got)
	}
	// Message text never reaches the stats table.
	var count int
	if err = r.db.QueryRow(`SELECT count(*) FROM model_requests WHERE project LIKE '%secret%' OR error LIKE '%secret%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("message text leaked into stats: %d %v", count, err)
	}
	rows := statsCall(t, r, "stats.requests", map[string]any{"harness": "claude"})["items"].([]any)
	if len(rows) != 3 || rows[0].(map[string]any)["project"] != "/work/proj" {
		t.Fatalf("unexpected request log: %v", rows)
	}
}

func TestRecordModelRequestAndRequestLog(t *testing.T) {
	r, _ := openTest(t, nil)
	task := model.Task{ID: "task-1", Attempts: 1, To: "openai:reviewer"}
	record := func(seq int, body string) {
		r.mu.Lock()
		r.recordModelRequest(task, fmt.Sprintf(`{"seq":%d,%s}`, seq, body))
		r.mu.Unlock()
	}
	record(1, `"model":"kimi-k3","provider":"conn-1","durationMs":1500,"ttftMs":400,"stopReason":"toolUse","inputTokens":1000,"outputTokens":50,"cachedInputTokens":800`)
	record(2, `"model":"kimi-k3","provider":"conn-1","durationMs":3000,"stopReason":"stop","inputTokens":1200,"outputTokens":100,"cachedInputTokens":0`)
	record(3, `"model":"kimi-k3","provider":"conn-1","durationMs":100,"stopReason":"error","error":"HTTP 429","inputTokens":0,"outputTokens":0,"cachedInputTokens":0`)
	record(2, `"model":"kimi-k3","provider":"conn-1","stopReason":"stop","inputTokens":9,"outputTokens":9,"cachedInputTokens":0`) // replay of seq 2
	log := statsCall(t, r, "stats.requests", map[string]any{"harness": "openai"})
	summary := log["summary"].(map[string]any)
	if number(t, summary["requests"]) != 3 || number(t, summary["failed"]) != 1 || number(t, summary["ok"]) != 2 {
		t.Fatalf("unexpected log summary: %v", summary)
	}
	if number(t, summary["medianDurationMs"]) != 1500 || number(t, summary["medianTtftMs"]) != 400 {
		t.Fatalf("unexpected medians: %v", summary)
	}
	items := log["items"].([]any)
	first := items[len(items)-1].(map[string]any)
	if number(t, first["input"]) != 200 || number(t, first["cacheRead"]) != 800 {
		t.Fatalf("cached tokens must be split from input: %v", first)
	}
	failed := statsCall(t, r, "stats.requests", map[string]any{"status": "failed"})["items"].([]any)
	if len(failed) != 1 || failed[0].(map[string]any)["error"] != "HTTP 429" {
		t.Fatalf("status filter failed: %v", failed)
	}
	if found := statsCall(t, r, "stats.requests", map[string]any{"search": "kimi"})["items"].([]any); len(found) != 3 {
		t.Fatalf("search failed: %v", found)
	}
	if found := statsCall(t, r, "stats.requests", map[string]any{"search": "100%"})["items"].([]any); len(found) != 0 {
		t.Fatalf("search wildcards must be literal: %v", found)
	}
	page := statsCall(t, r, "stats.requests", map[string]any{"limit": 2})
	if len(page["items"].([]any)) != 2 || page["hasMore"] != true {
		t.Fatalf("pagination failed: %v", page)
	}
}

func TestStatsOperationsValidateInput(t *testing.T) {
	r, _ := openTest(t, nil)
	for _, payload := range []string{`{"rangeHours":5}`, `{"harness":"gemini"}`, `{"tzOffsetMinutes":9999}`, `{"unknown":1}`} {
		if _, err := r.statsOperation(context.Background(), admin, "stats.summary", json.RawMessage(payload)); err == nil {
			t.Fatalf("%s was accepted", payload)
		}
	}
	if _, err := r.statsOperation(context.Background(), admin, "stats.requests", json.RawMessage(`{"status":"weird"}`)); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	if _, err := r.statsOperation(context.Background(), model.Principal{}, "stats.summary", json.RawMessage(`{}`)); err == nil {
		t.Fatal("stats require an administrator")
	}
}
