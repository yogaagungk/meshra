CREATE TABLE IF NOT EXISTS personal_settings (id INTEGER PRIMARY KEY CHECK(id=1), body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspaces (id TEXT PRIMARY KEY, body TEXT NOT NULL, path TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS chat_settings (conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS run_usage (
 task_id TEXT NOT NULL, attempt INTEGER NOT NULL, started_at TEXT NOT NULL,
 harness TEXT NOT NULL, model TEXT NOT NULL, provider TEXT NOT NULL,
 workspace_id TEXT NOT NULL, state TEXT NOT NULL, input_bytes INTEGER NOT NULL DEFAULT 0,
 output_bytes INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0,
 input_tokens INTEGER, output_tokens INTEGER,
 PRIMARY KEY(task_id,attempt)
);
CREATE INDEX IF NOT EXISTS usage_started ON run_usage(started_at);
UPDATE schema_version SET version=7;
