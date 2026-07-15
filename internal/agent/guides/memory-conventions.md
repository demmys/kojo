# Memory conventions guide

Placeholders (values are shown in your system prompt):
- `{DATA_DIR}` = your data directory. `{DATA_DIR}/MEMORY.md` is your index; detail files live under `{DATA_DIR}/memory/`.

## MEMORY.md — keep it a LEAN index, not a dumping ground

MEMORY.md is read at the start of EVERY session. It must stay small and scannable.
Aim for ~200 lines. Structure as an index of short sections: Identity, Active Projects,
User Context, Known People, Recurring Procedures, etc.

Core rules:
1. (MEMORY.md only) Things you must always remember: one terse bullet per entry. No prose, no examples. Detail files under memory/ may be as long as needed.
2. (MEMORY.md only) Things you must not forget but don't need every session: move to a separate file under memory/ and leave only an index line in MEMORY.md noting WHEN to read it.
   Example: `- [Release procedure](memory/topics/release.md) — read when cutting a release`
3. (MEMORY.md and detail files) Delete stale entries. Don't pile new on top of old — overwrite or remove. Git keeps the history.
4. (MEMORY.md and detail files) Do NOT write dates. No `(updated 2026-04-22)`, no `recently fixed`, no `as of last week`. State facts in the present tense as if they're true now. If a fact is no longer true, delete it (rule 3).
   Exempt: the daily diary. Its `## YYYY-MM-DD` header and `HH:MM` timestamps are required and not affected by rules 3 and 4.

Other constraints:
- Do NOT keep task/todo lists in MEMORY.md or memory/ files. In-flight tasks belong in the kojo todo API (see the todos guide) — it is injected into every turn and visible in the Web UI. Memory holds diary entries, knowledge, and context; todos hold work to be done.
- When MEMORY.md exceeds ~300 lines, move the oldest / bulkiest sections to memory/archive/ and leave a one-line pointer.
- Don't dump long narratives, transcripts, error logs, or research notes into MEMORY.md — park them under memory/topics/ or memory/projects/ and link.
- Don't duplicate the daily diary's blow-by-blow. The daily diary holds turn-level detail; MEMORY.md holds what persists across days.
- Don't keep entries "just in case" you might need them later. If it's not useful at session start, move it out.

## memory/ layout

- `memory/{YYYY-MM-DD}.md` — daily running notes (mandatory)
- `memory/projects/{name}.md` — long-running project notes
- `memory/people/{name}.md` — notes about specific people
- `memory/topics/{topic}.md` — subject-matter reference
- `memory/archive/{YYYY-MM}.md` — rotated-out daily notes or obsolete projects

Create directories on demand with `mkdir -p`. Keep the structure shallow (one subdirectory level). Always use absolute paths anchored at `{DATA_DIR}` when editing memory files.
