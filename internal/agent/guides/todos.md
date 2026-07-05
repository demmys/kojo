# Persistent Todo API guide

Placeholders (values are shown in your system prompt):
- `{AGENT_ID}` = your agent ID
- `{API_BASE}` = the kojo API base URL
- `{CURL_FLAGS}` = the curl flags shown in the "kojo Guides" section (auth header, TLS flag)

Use these endpoints to track todos that must survive across conversation sessions.
Todos are persisted server-side and re-injected at the top of every user message (in the `<context>` block) — they are immune to context compaction.
Note: for historical reasons the endpoint path segment, JSON key, and ID prefix use `tasks` / `task_*` — treat them as aliases for todos.

List todos: `curl {CURL_FLAGS} '{API_BASE}/api/v1/agents/{AGENT_ID}/tasks'`
Create todo: `curl {CURL_FLAGS} -X POST '{API_BASE}/api/v1/agents/{AGENT_ID}/tasks' -H 'Content-Type: application/json' -d '{"title":"..."}'`
Complete todo: `curl {CURL_FLAGS} -X PATCH '{API_BASE}/api/v1/agents/{AGENT_ID}/tasks/TODO_ID' -H 'Content-Type: application/json' -d '{"status":"done"}'`
Delete todo: `curl {CURL_FLAGS} -X DELETE '{API_BASE}/api/v1/agents/{AGENT_ID}/tasks/TODO_ID'`

When starting a multi-step job, create a todo so you won't forget it even if context is compressed.
Mark todos as done when completed. Delete todos that are no longer relevant.
