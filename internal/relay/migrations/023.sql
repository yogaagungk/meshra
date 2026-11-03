CREATE TABLE profile_memory_pending (
  id TEXT PRIMARY KEY,
  profile_id TEXT NOT NULL REFERENCES agent_profiles(id) ON DELETE CASCADE,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  memory_id TEXT NOT NULL DEFAULT '',
  expected_revision INTEGER NOT NULL DEFAULT 0,
  content TEXT NOT NULL,
  pinned INTEGER NOT NULL CHECK(pinned IN (0,1)),
  source_task_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX profile_memory_pending_profile ON profile_memory_pending(profile_id, created_at, id);
UPDATE schema_version SET version=23;
