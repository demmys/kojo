# Credentials guide

Placeholders (values are shown in your system prompt):
- `{AGENT_ID}` = your agent ID
- `{API_BASE}` = the kojo API base URL
- `{CURL_FLAGS}` = the curl flags shown in the "kojo Guides" section (auth header, TLS flag)

Your credentials are stored in an encrypted database and accessible only via API.
Do NOT try to read credentials from files.

**List credentials** (labels/usernames only, secrets masked):

```
curl {CURL_FLAGS} {API_BASE}/api/v1/agents/{AGENT_ID}/credentials
```

**Get password** for a credential (use the Python example below instead of raw curl):
  Endpoint: `{API_BASE}/api/v1/agents/{AGENT_ID}/credentials/CRED_ID/password` → `{"password":"..."}`

**Get TOTP code** (for 2FA-enabled credentials, capture programmatically):
  Endpoint: `{API_BASE}/api/v1/agents/{AGENT_ID}/credentials/CRED_ID/totp` → `{"code":"123456","remaining":15}`

Replace CRED_ID with the credential's `id` from the list response.

**IMPORTANT: Shell escaping** — Passwords often contain special characters (`$`, `!`, `"`, `'`, `\`, `&`, etc.) that break when interpolated into shell strings.
When using a retrieved password in another command, use Python to avoid shell escaping:

```python
import json, os, ssl, urllib.request
url = '{API_BASE}/api/v1/agents/{AGENT_ID}/credentials/CRED_ID/password'
req = urllib.request.Request(url, headers={'X-Kojo-Token': os.environ['KOJO_AGENT_TOKEN']})
kwargs = {}
if url.startswith('https://'):
    # Skip TLS verification for local/Tailscale self-signed cert only
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    kwargs['context'] = ctx
with urllib.request.urlopen(req, **kwargs) as resp:
    password = json.loads(resp.read())['password']
# Use password directly in Python — never paste into shell strings
```

Pass secrets via stdin when possible, or environment variables if the tool requires it. Never interpolate into shell strings.

Security rules:
- NEVER display passwords or TOTP secrets in chat. When asked about credentials, mention only labels and usernames.
- NEVER write passwords, TOTP secrets, or any credential values to MEMORY.md, diary files, or any other files. If you accidentally wrote credentials to a file, remove them. If you find credentials written by someone else, alert the user.
- Always retrieve credentials fresh from the API when needed.
