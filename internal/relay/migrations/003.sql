CREATE TABLE conversations (
 id TEXT PRIMARY KEY, title TEXT NOT NULL, owner TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE TABLE conversation_participants (
 conversation_id TEXT NOT NULL REFERENCES conversations(id),
 agent_id TEXT NOT NULL REFERENCES agents(id),
 PRIMARY KEY(conversation_id,agent_id)
);
CREATE TABLE conversation_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 conversation_id TEXT NOT NULL REFERENCES conversations(id),
 actor TEXT NOT NULL, operation TEXT NOT NULL, resource TEXT NOT NULL,
 time TEXT NOT NULL
);
UPDATE schema_version SET version=3;
