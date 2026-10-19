CREATE TABLE artifacts (
 digest TEXT PRIMARY KEY, content BLOB NOT NULL, size INTEGER NOT NULL CHECK(size>=0 AND size<=65536)
);
CREATE TRIGGER artifacts_immutable BEFORE UPDATE ON artifacts BEGIN SELECT RAISE(ABORT,'immutable artifact'); END;
CREATE TABLE contexts (
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, title TEXT NOT NULL,
 type TEXT NOT NULL, scope TEXT NOT NULL, idempotency_key TEXT, request_hash TEXT NOT NULL,
 UNIQUE(owner,idempotency_key)
);
CREATE TABLE context_versions (
 context_id TEXT NOT NULL REFERENCES contexts(id), version INTEGER NOT NULL CHECK(version>0),
 digest TEXT NOT NULL REFERENCES artifacts(digest), created_at TEXT NOT NULL,
 PRIMARY KEY(context_id,version)
);
CREATE TRIGGER context_versions_immutable BEFORE UPDATE ON context_versions BEGIN SELECT RAISE(ABORT,'immutable version'); END;
CREATE TABLE context_acl (
 context_id TEXT NOT NULL REFERENCES contexts(id), agent_id TEXT NOT NULL REFERENCES agents(id),
 PRIMARY KEY(context_id,agent_id)
);
CREATE TABLE context_bindings (
 conversation_id TEXT NOT NULL REFERENCES conversations(id), context_id TEXT NOT NULL,
 version INTEGER NOT NULL, PRIMARY KEY(conversation_id,context_id,version),
 FOREIGN KEY(context_id,version) REFERENCES context_versions(context_id,version)
);
CREATE TABLE context_access_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, caller TEXT NOT NULL, operation TEXT NOT NULL,
 context_id TEXT NOT NULL, version INTEGER NOT NULL, time TEXT NOT NULL, outcome TEXT NOT NULL
);
CREATE TRIGGER context_log_no_update BEFORE UPDATE ON context_access_log BEGIN SELECT RAISE(ABORT,'append only'); END;
CREATE TRIGGER context_log_no_delete BEFORE DELETE ON context_access_log BEGIN SELECT RAISE(ABORT,'append only'); END;
UPDATE schema_version SET version=4;
CREATE TRIGGER context_versions_no_delete BEFORE DELETE ON context_versions BEGIN SELECT RAISE(ABORT,'immutable version'); END;
CREATE TRIGGER artifacts_no_delete BEFORE DELETE ON artifacts BEGIN SELECT RAISE(ABORT,'immutable artifact'); END;
CREATE TRIGGER contexts_immutable BEFORE UPDATE ON contexts BEGIN SELECT RAISE(ABORT,'immutable metadata'); END;
