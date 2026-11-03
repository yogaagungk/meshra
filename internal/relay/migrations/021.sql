ALTER TABLE profile_tool_policies ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'manual' CHECK(approval_policy IN ('manual','allowlist'));
ALTER TABLE tool_invocations ADD COLUMN allow_rule_id TEXT NOT NULL DEFAULT '';
CREATE TABLE tool_allow_rules(id TEXT PRIMARY KEY,profile_id TEXT NOT NULL REFERENCES agent_profiles(id) ON DELETE CASCADE,tool TEXT NOT NULL CHECK(tool IN ('meshra_workspace_command','meshra_workspace_write')),matcher_kind TEXT NOT NULL CHECK(matcher_kind IN ('argv_exact','argv_prefix','path_glob')),matcher TEXT NOT NULL,created_by TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT,revoked_at TEXT,revision INTEGER NOT NULL DEFAULT 1 CHECK(revision>0));
CREATE INDEX tool_allow_rules_profile ON tool_allow_rules(profile_id,tool);
UPDATE schema_version SET version=21;
