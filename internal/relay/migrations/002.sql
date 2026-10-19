-- Run inside a transaction with foreign_keys temporarily OFF. No data is discarded.
CREATE TABLE tasks_v2 (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 sender TEXT NOT NULL,
 sender_session TEXT NOT NULL,
 recipient TEXT NOT NULL,
 operation TEXT NOT NULL,
 idempotency_key TEXT,
 state TEXT NOT NULL,
 body TEXT NOT NULL,
 UNIQUE(sender, sender_session, operation, idempotency_key)
);
INSERT INTO tasks_v2(seq,id,sender,sender_session,recipient,operation,idempotency_key,state,body)
 SELECT seq,id,sender,sender_session,json_extract(body,'$.to'),COALESCE(json_extract(body,'$.type'),'send'),idempotency_key,state,body FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_v2 RENAME TO tasks;
CREATE INDEX tasks_active ON tasks(seq) WHERE state IN ('queued','dispatching','running');
CREATE INDEX tasks_inbox ON tasks(recipient) WHERE state IN ('queued','dispatching','running');
CREATE TABLE audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 time TEXT NOT NULL,
 actor TEXT NOT NULL,
 recipient TEXT NOT NULL,
 operation TEXT NOT NULL,
 category TEXT NOT NULL
);
ALTER TABLE events ADD COLUMN metadata TEXT NOT NULL DEFAULT '{}';
UPDATE schema_version SET version=2 WHERE version=1;
