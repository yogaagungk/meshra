ALTER TABLE profile_context_epochs ADD COLUMN rotate_pending INTEGER NOT NULL DEFAULT 0 CHECK(rotate_pending IN (0,1));
CREATE TABLE profile_memories(id TEXT PRIMARY KEY,profile_id TEXT NOT NULL REFERENCES agent_profiles(id),conversation_id TEXT REFERENCES conversations(id),scope TEXT NOT NULL CHECK(scope IN ('operator','conversation')),revision INTEGER NOT NULL CHECK(revision>0),content TEXT NOT NULL CHECK(length(CAST(content AS BLOB))<=4096),source_task_id TEXT,pinned INTEGER NOT NULL CHECK(pinned IN (0,1)),updated_at TEXT NOT NULL,CHECK((scope='operator' AND conversation_id IS NULL) OR (scope='conversation' AND conversation_id IS NOT NULL)));
CREATE INDEX profile_memories_scope ON profile_memories(profile_id,conversation_id,pinned DESC,updated_at DESC);
CREATE VIRTUAL TABLE profile_history USING fts5(task_id UNINDEXED,profile_id UNINDEXED,conversation_id UNINDEXED,text,reply);
INSERT INTO profile_history(task_id,profile_id,conversation_id,text,reply) SELECT t.id,s.profile_id,json_extract(t.body,'$.conversationId'),json_extract(t.body,'$.text'),coalesce(json_extract(t.body,'$.result'),'') FROM tasks t JOIN profile_task_snapshots s ON s.task_id=t.id;
CREATE TRIGGER profile_history_admit AFTER INSERT ON profile_task_snapshots BEGIN
 INSERT INTO profile_history(task_id,profile_id,conversation_id,text,reply) SELECT t.id,NEW.profile_id,json_extract(t.body,'$.conversationId'),json_extract(t.body,'$.text'),coalesce(json_extract(t.body,'$.result'),'') FROM tasks t WHERE t.id=NEW.task_id;
END;
CREATE TRIGGER profile_history_update AFTER UPDATE OF body ON tasks WHEN EXISTS(SELECT 1 FROM profile_task_snapshots WHERE task_id=NEW.id) AND (coalesce(json_extract(OLD.body,'$.text'),'')<>coalesce(json_extract(NEW.body,'$.text'),'') OR coalesce(json_extract(OLD.body,'$.result'),'')<>coalesce(json_extract(NEW.body,'$.result'),'')) BEGIN
 DELETE FROM profile_history WHERE task_id=OLD.id;
 INSERT INTO profile_history(task_id,profile_id,conversation_id,text,reply) SELECT NEW.id,s.profile_id,json_extract(NEW.body,'$.conversationId'),json_extract(NEW.body,'$.text'),coalesce(json_extract(NEW.body,'$.result'),'') FROM profile_task_snapshots s WHERE s.task_id=NEW.id;
END;
CREATE TRIGGER profile_history_delete BEFORE DELETE ON tasks WHEN EXISTS(SELECT 1 FROM profile_task_snapshots WHERE task_id=OLD.id) BEGIN DELETE FROM profile_history WHERE task_id=OLD.id; END;
CREATE TABLE agent_queue_state(agent_id TEXT PRIMARY KEY REFERENCES agents(id),paused INTEGER NOT NULL CHECK(paused IN (0,1)),revision INTEGER NOT NULL CHECK(revision>0));
CREATE TABLE task_dispatch_order(task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,agent_id TEXT NOT NULL REFERENCES agents(id),rank INTEGER NOT NULL);
CREATE INDEX task_dispatch_agent ON task_dispatch_order(agent_id,rank);
UPDATE schema_version SET version=11;
