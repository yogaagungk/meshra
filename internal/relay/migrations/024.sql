CREATE TABLE skill_proposals (
 id TEXT PRIMARY KEY,
 profile_id TEXT NOT NULL,
 task_id TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected')),
 body TEXT NOT NULL
);
UPDATE schema_version SET version=24;
