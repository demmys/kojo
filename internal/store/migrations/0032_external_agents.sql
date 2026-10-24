-- External agents: AI agents running outside kojo on another Tailscale
-- node that join group DMs through the dedicated external listener.
-- LOCAL scope (Hub only): never synchronized to a peer, so the table
-- carries no version/etag/peer_id columns.
--
-- A row is bound to exactly one tailnet node (stable_node_id) once its
-- pairing code is redeemed. token_hash / pairing_hash hold sha256 hex
-- digests; the raw values are never stored. token_gen increments on
-- every pair / revoke so live connections can tell they were revoked.
CREATE TABLE external_agents (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  stable_node_id  TEXT,
  node_name       TEXT NOT NULL DEFAULT '',
  token_hash      TEXT,
  token_gen       INTEGER NOT NULL DEFAULT 0,
  pairing_hash    TEXT,
  pairing_expires INTEGER,
  enabled         INTEGER NOT NULL DEFAULT 1,
  rate_per_min    INTEGER NOT NULL DEFAULT 20,
  last_seen_at    INTEGER,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  deleted_at      INTEGER
);
CREATE UNIQUE INDEX idx_external_agents_node ON external_agents(stable_node_id)
  WHERE stable_node_id IS NOT NULL AND deleted_at IS NULL;

-- Tailnet nodes that were ever paired as an external agent. The Hub's
-- public (Owner) listener refuses them. Rows outlive the external agent
-- (delete / re-pair / revoke never remove them) so a node cannot fall
-- back to "unknown tailnet caller == Owner"; only an explicit operator
-- action deletes a row. tailnet_ips_json lets the listener refuse the
-- node even while WhoIs is failing.
CREATE TABLE external_node_denylist (
  stable_node_id   TEXT PRIMARY KEY,
  node_name        TEXT NOT NULL DEFAULT '',
  tailnet_ips_json TEXT NOT NULL DEFAULT '[]',
  external_id      TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

-- Per (external agent, group DM) read position. The external API's
-- long-poll is level-triggered against acked_seq.
CREATE TABLE external_agent_cursors (
  external_id TEXT NOT NULL,
  groupdm_id  TEXT NOT NULL,
  acked_seq   INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (external_id, groupdm_id)
);
