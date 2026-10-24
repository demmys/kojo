-- Per-room trust level for external AI agent messages. Absent row =
-- "untrusted" (the default). Kept out of groupdms so the row etag /
-- peer sync shape is unchanged.
CREATE TABLE groupdm_external_trust (
  groupdm_id TEXT PRIMARY KEY,
  trust      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
