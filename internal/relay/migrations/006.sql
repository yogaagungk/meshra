CREATE TABLE chat_conversations (
 conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE
);
UPDATE schema_version SET version=6;
