CREATE TABLE browser_sessions(id TEXT PRIMARY KEY,profile_id TEXT NOT NULL REFERENCES agent_profiles(id),conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,state TEXT NOT NULL CHECK(state IN ('opening','active','taking_over','closed','error')),epoch INTEGER NOT NULL DEFAULT 1 CHECK(epoch>0),body TEXT NOT NULL);
CREATE INDEX browser_sessions_profile ON browser_sessions(profile_id,conversation_id,id);
CREATE TABLE voice_settings(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL CHECK(revision>0),body TEXT NOT NULL);
CREATE TABLE voice_jobs(id TEXT PRIMARY KEY,session_owner TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','running','completed','failed','cancelled','expired')),expires_at TEXT NOT NULL,body TEXT NOT NULL);
CREATE INDEX voice_jobs_owner ON voice_jobs(session_owner,state,expires_at);
UPDATE schema_version SET version=18;
