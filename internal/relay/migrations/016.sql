CREATE TABLE local_skills(id TEXT PRIMARY KEY,revision INTEGER NOT NULL CHECK(revision>0),name TEXT NOT NULL,description TEXT NOT NULL,archived INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)));
CREATE TABLE local_skill_versions(skill_id TEXT NOT NULL REFERENCES local_skills(id),revision INTEGER NOT NULL CHECK(revision>0),sha256 TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(skill_id,revision));
CREATE TABLE profile_skill_bindings(profile_id TEXT NOT NULL REFERENCES agent_profiles(id),skill_id TEXT NOT NULL REFERENCES local_skills(id),version INTEGER NOT NULL,enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),revision INTEGER NOT NULL CHECK(revision>0),PRIMARY KEY(profile_id,skill_id),FOREIGN KEY(skill_id,version) REFERENCES local_skill_versions(skill_id,revision));
CREATE TABLE local_templates(id TEXT PRIMARY KEY,revision INTEGER NOT NULL CHECK(revision>0),kind TEXT NOT NULL,body TEXT NOT NULL);
CREATE TABLE template_import_previews(id TEXT PRIMARY KEY,digest TEXT NOT NULL,expires_at TEXT NOT NULL,body TEXT NOT NULL);
UPDATE schema_version SET version=16;
