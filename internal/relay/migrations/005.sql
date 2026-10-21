CREATE INDEX tasks_conversation_seq ON tasks(json_extract(body,'$.conversationId'),seq);
CREATE INDEX tasks_parent ON tasks(json_extract(body,'$.parentTaskId'));
UPDATE schema_version SET version=5;
