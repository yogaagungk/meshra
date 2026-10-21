CREATE TABLE IF NOT EXISTS console_account (
 id INTEGER PRIMARY KEY CHECK (id=1),
 username TEXT NOT NULL,
 salt BLOB NOT NULL,
 password_hash BLOB NOT NULL,
 iterations INTEGER NOT NULL
);
UPDATE schema_version SET version=9;
