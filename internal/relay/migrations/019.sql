CREATE TABLE deleted_contexts (
 context_id TEXT PRIMARY KEY REFERENCES contexts(id), deleted_by TEXT NOT NULL, deleted_at TEXT NOT NULL
);
UPDATE schema_version SET version=19;
