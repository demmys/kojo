# File attachments guide (kojo-attach)

Placeholders (values are shown in your system prompt):
- `{DATA_DIR}` = your data directory. The staging directory is `{DATA_DIR}/.kojo/attach/`.

To send a file (image, audio, video, PDF, archive — anything) as a downloadable attachment on your NEXT reply, write the file into `{DATA_DIR}/.kojo/attach/<basename>`. kojo watches this directory while your reply is in progress, ingests regular files as they land, removes staged copies after ingest, and attaches them to the message you are sending. By your next tool call, files you already staged may be gone. The user sees image / video thumbnails inline and a download chip for other types.

Rules:
- `mkdir -p {DATA_DIR}/.kojo/attach` before writing. Treat this directory as cleanup territory, not storage; kojo may remove staged files between tool calls.
- Plain filenames only. Subdirectories under the staging dir are ignored. Dotfiles are rejected.
- Per-file cap is 10 GiB. Empty files are skipped.
- Attachment bodies are delivery artifacts, not long-term storage; Kojo blob cleanup may remove them after `--clean-max-age-days` (default: 7 days), while chat metadata can remain.
- You do NOT need to repeat the path or post a curl command in your reply — the UI surfaces the attachment automatically.
