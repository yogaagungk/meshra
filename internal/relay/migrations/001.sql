PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY);
INSERT OR IGNORE INTO schema_version VALUES (1);
CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS retired_agents (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS tasks (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 sender TEXT NOT NULL,
 sender_session TEXT NOT NULL,
 idempotency_key TEXT,
 state TEXT NOT NULL,
 body TEXT NOT NULL,
 UNIQUE(sender, sender_session, idempotency_key)
);
CREATE INDEX IF NOT EXISTS tasks_active ON tasks(seq) WHERE state IN ('queued','dispatching','running');
CREATE TABLE IF NOT EXISTS events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 task_id TEXT NOT NULL REFERENCES tasks(id),
 kind TEXT NOT NULL,
 data TEXT NOT NULL,
 time TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id,id);
CREATE TABLE IF NOT EXISTS credentials (
 hash TEXT PRIMARY KEY,
 agent_id TEXT NOT NULL REFERENCES agents(id),
 session_id TEXT NOT NULL,
 task_id TEXT NOT NULL
);
