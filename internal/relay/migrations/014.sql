CREATE TABLE external_mcp_servers(id TEXT PRIMARY KEY,revision INTEGER NOT NULL CHECK(revision>0),name TEXT NOT NULL,transport TEXT NOT NULL CHECK(transport IN ('stdio','http')),body TEXT NOT NULL);
CREATE INDEX external_mcp_servers_name ON external_mcp_servers(name);
CREATE TABLE external_mcp_tools(server_id TEXT NOT NULL REFERENCES external_mcp_servers(id) ON DELETE CASCADE,name TEXT NOT NULL,digest TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(server_id,name));
CREATE TABLE profile_mcp_bindings(profile_id TEXT NOT NULL REFERENCES agent_profiles(id) ON DELETE CASCADE,server_id TEXT NOT NULL REFERENCES external_mcp_servers(id) ON DELETE CASCADE,revision INTEGER NOT NULL CHECK(revision>0),tool_names TEXT NOT NULL,enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),PRIMARY KEY(profile_id,server_id));
CREATE INDEX profile_mcp_bindings_server ON profile_mcp_bindings(server_id);
CREATE TABLE mcp_auth_flows(id TEXT PRIMARY KEY,server_id TEXT NOT NULL REFERENCES external_mcp_servers(id) ON DELETE CASCADE,session_owner TEXT NOT NULL,state_hash TEXT NOT NULL UNIQUE,expires_at TEXT NOT NULL,body TEXT NOT NULL);
CREATE INDEX mcp_auth_flows_expiry ON mcp_auth_flows(expires_at);
CREATE TABLE mcp_auth_tokens(server_id TEXT PRIMARY KEY REFERENCES external_mcp_servers(id) ON DELETE CASCADE,secret_id TEXT NOT NULL,expires_at TEXT NOT NULL,scopes TEXT NOT NULL,body TEXT NOT NULL);
UPDATE schema_version SET version=14;
