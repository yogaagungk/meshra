CREATE TABLE model_requests(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 source TEXT NOT NULL,
 harness TEXT NOT NULL,
 ext_id TEXT NOT NULL,
 ts INTEGER NOT NULL,
 model TEXT NOT NULL,
 provider TEXT NOT NULL,
 project TEXT NOT NULL DEFAULT '',
 agent_type TEXT NOT NULL DEFAULT 'main',
 status TEXT NOT NULL DEFAULT 'ok' CHECK(status IN ('ok','aborted','failed')),
 stop_reason TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 duration_ms INTEGER,
 ttft_ms INTEGER,
 input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_tokens INTEGER NOT NULL DEFAULT 0,
 cache_write_tokens INTEGER NOT NULL DEFAULT 0,
 premium REAL NOT NULL DEFAULT 0,
 cost REAL,
 UNIQUE(source,ext_id)
);
CREATE INDEX model_requests_ts ON model_requests(ts);
CREATE INDEX model_requests_harness_ts ON model_requests(harness,ts);
CREATE TABLE stats_import_state(
 source TEXT PRIMARY KEY,
 cursor TEXT NOT NULL DEFAULT '',
 last_run_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 available INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE stats_import_files(
 path TEXT PRIMARY KEY,
 offset INTEGER NOT NULL,
 mtime INTEGER NOT NULL
);
UPDATE schema_version SET version=26;
