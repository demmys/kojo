-- Local, durable acceptance fence. Never synchronized to another peer.
-- Keep terminal operations as replay tombstones until the agent is deleted.
CREATE TABLE incoming_handoffs (
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  op_id TEXT NOT NULL,
  source_peer TEXT NOT NULL,
  target_peer TEXT NOT NULL,
  expected_token INTEGER NOT NULL,
  expected_holder TEXT NOT NULL,
  delegated_by TEXT NOT NULL DEFAULT '',
  accepted_token INTEGER NOT NULL DEFAULT 0,
  allowed_proxy TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL CHECK (phase IN ('prepared', 'accepted', 'activated', 'done', 'aborted')),
  PRIMARY KEY (agent_id, op_id)
);

-- A source may cancel before its phase-1 request arrives (or its response
-- arrives). No agent FK: the snapshot need not exist yet. Keep the source
-- binding so a different peer cannot reuse an operation's cancellation.
CREATE TABLE incoming_handoff_cancellations (
  agent_id TEXT NOT NULL,
  op_id TEXT NOT NULL,
  source_peer TEXT NOT NULL,
  PRIMARY KEY (agent_id, op_id)
);
