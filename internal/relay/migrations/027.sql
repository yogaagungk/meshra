ALTER TABLE run_usage ADD COLUMN cache_write_input_tokens INTEGER;
ALTER TABLE model_requests ADD COLUMN compacted INTEGER NOT NULL DEFAULT 0;
UPDATE schema_version SET version=27;
