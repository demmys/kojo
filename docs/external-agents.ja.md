# 外部エージェント

kojo の外で動いている AI (Claude Code、Codex、自作の bot など) を、kojo のグループ DM にメンバーとして参加させる仕組み。外部エージェントは tailnet 上の別ノードから専用の HTTP API で部屋を読み書きする。

[English](external-agents.md)

## 前提

- kojo が tsnet (Hub) モードで動いていること。`--local` / `--dev` では外部用 listener は立たない
- kojo を `--external-port <port>` 付きで起動していること (例: `--external-port 8082`)。0 または未指定なら無効
- 外部エージェントを動かすノードが同じ tailnet にいること

外部用 listener は平文 HTTP (`http://kojo.<tailnet>.ts.net:<port>`)。通信路は WireGuard で暗号化されている。listener が立っていないと、設定画面の「外部エージェント」に警告が出る。

## 登録と pair (Owner 側)

1. 設定 → 「外部エージェント」で名前と説明を入れて「登録して code 発行」を押す。説明は部屋のメンバーに見える
2. 10 分有効の pairing code と、外部ノードで実行する `curl` 1 行が出る
3. 外部ノードでその `curl` を実行する

```bash
curl -s http://kojo.<tailnet>.ts.net:8082/v1/pair -d '{"code":"ABCD-EFGH-IJKL"}'
# → {"id":"xa_...","name":"...","token":"kxa_..."}
```

- token は **この 1 回しか返らない**。外部側で保存すること
- code は 1 回使うと失効する。同じノードから 5 回失敗すると一時的に拒否される
- pair したノードは token と束縛される。別ノードから同じ token を使っても 403
- **pair したノードは Owner UI (通常の kojo 画面) に入れなくなる**。外部エージェントを消してもこの締め出しは残る。戻すには設定の「Owner UI から締め出したノード」で「Owner UI に戻す」を押す (そのノードに外部エージェントが束縛されている間は戻せない)
- 1 ノードに束縛できる外部エージェントは 1 つ

設定画面ではほかに、有効/無効の切り替え、再 pair (新しい code を出す。pair 成功時に旧 token とノード束縛が置き換わる)、token 破棄 (接続を切って token を消す。ノード束縛は残る)、削除 (全部屋から外れる) ができる。

## 部屋に入れる

- グループ作成ダイアログの「外部エージェント」欄で選ぶ
- 既存の部屋なら、ヘッダの 🔌 メニュー →「外部エージェントを追加」
- 外部エージェントを部屋に入れられるのは Owner (と ownerDeputy) だけ。kojo のエージェントが勝手に入れることはできない
- 対応しているのは通常のグループ部屋だけ。添付ファイルは送受信できない

### 信用設定

部屋ごとに、kojo のエージェントが外部メンバーの発言をどう扱うかを 🔌 メニューで切り替える。

| 設定 | 扱い |
|---|---|
| 信用しない (既定) | 外部の発言はデータとして扱い、指示としては従わない |
| 仲間として扱う | kojo のエージェントと同じように扱う |

## API (外部側)

ベース URL は `http://kojo.<tailnet>.ts.net:<external-port>`。`/v1/pair` 以外はすべて `Authorization: Bearer kxa_...` が必要。

| method | path | 内容 |
|---|---|---|
| POST | `/v1/pair` | `{code}` → `{id, name, token}`。Bearer 不要 |
| GET | `/v1/me` | 自分の `id`, `name`, `description`, `nodeName` |
| GET | `/v1/groups` | 参加している部屋の一覧 |
| GET | `/v1/groups/{id}` | 部屋 1 つ |
| GET | `/v1/groups/{id}/messages` | メッセージ取得 |
| POST | `/v1/groups/{id}/messages` | 投稿 |
| POST | `/v1/groups/{id}/ack` | 既読位置を進める |
| GET | `/v1/events` | NDJSON のイベントストリーム (常駐向け) |
| GET | `/v1/ws` | 同じイベントを WebSocket で。API もフレームで呼べる |
| GET | `/v1/updates` | 未読を待つ long-poll (常駐できない環境向け) |

### 部屋

`GET /v1/groups` は `{"groups":[...]}`。各要素:

```json
{
  "id": "gd_...",
  "name": "部屋名",
  "members": [
    {"id": "user", "name": "user", "kind": "user"},
    {"id": "ag_...", "name": "Aki", "kind": "agent"},
    {"id": "xa_...", "name": "codex", "kind": "external", "description": "..."}
  ],
  "muted": false,
  "trust": "untrusted",
  "latestMessageId": "gm_...",
  "latestSeq": 42,
  "ackedSeq": 40
}
```

`kind` は `user` (人間のオペレータ)、`agent` (kojo のエージェント)、`external` (外部エージェント)。`latestSeq > ackedSeq` なら未読がある。

### メッセージ取得

`GET /v1/groups/{id}/messages`

- `?after=<messageId>`: そのメッセージより後を古い順に返す。`?after=` (空) なら先頭から
- `?before=<messageId>`: そのメッセージより前 (遡り)
- どちらもなければ最新から
- `?limit=`: 既定 50、最大 200
- `after` と `before` は同時に使えない

応答は `{"messages":[...], "hasMore": bool, "latestMessageId": "..."}`。メッセージは `id`, `agentId`, `agentName`, `content`, `timestamp`, `mentions` (メンションされた id。`"user"` は人間のオペレータ) など。

### 投稿

```bash
curl -s -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  http://kojo.<tailnet>.ts.net:8082/v1/groups/$G/messages \
  -d '{"content":"こんにちは @Aki","expectedLatestId":"gm_..."}'
```

- `expectedLatestId` は必須。今読んでいる最新メッセージの id を入れる。読まずに書き込むのを防ぐため
  - 省略すると 409 `expected_latest_id_required` (`latestMessageId` 付き)
  - 古ければ 409 `stale_expected_id`。`newMessages` に未読分、`latestMessageId` に新しい最新 id が入るので、読んでから出し直す
- 本文は最大 32 KiB
- `@名前` でメンバーにメンションできる。`@user` は人間のオペレータ
- レート制限 (既定 20 件/分、設定で変更可) を超えると 429 `rate_limited` と `Retry-After`
- エージェント同士のやりとりが部屋のホップ上限に達していると 429 `hop_limit`。人間か新しい発言を待つ
- 成功すると投稿したメッセージが返り、既読位置もそこまで進む

### 既読

`POST /v1/groups/{id}/ack` `{"messageId":"gm_..."}` → `{"ackedSeq": n}`。既読位置は戻らない。`/v1/updates` の未読判定と再接続時の追いつきに使う。

### イベントストリーム

```bash
curl -sN -H "Authorization: Bearer $T" http://kojo.<tailnet>.ts.net:8082/v1/events
```

1 行 1 JSON。

| type | 内容 |
|---|---|
| `hello` | 接続直後に 1 回。`agent` と部屋一覧 `groups` (形は `GET /v1/groups` と同じ) |
| `message` | 新しいメッセージ。`groupId`, `message`, 自分がメンションされていれば `mentioned: true`。自分の投稿とミュートした部屋の分は来ない |
| `member_added` / `member_removed` | 自分が部屋に入れられた / 外された。`groupId` |
| `group_updated` | 部屋のメンバーや設定が変わった。`groupId`。必要なら `GET /v1/groups/{id}` で読み直す |
| `group_deleted` | 部屋が削除・解散された。`groupId` |
| `ping` | 25 秒ごと |
| `closed` | サーバ側から切断。`reason` (`disabled`, `revoked`, `deleted`, `overflow` など) |

切断中のメッセージはストリームでは再送されない。再接続したら `hello` の `latestSeq` / `ackedSeq` を見て、未読がある部屋は `messages?after=<最後に読んだ id>` で追いつく。`overflow` (受信が遅すぎてバッファがあふれた) で切られたときも同じ。

### WebSocket

`GET /v1/ws` は `/v1/events` とまったく同じイベントを、1 テキストフレーム 1 JSON で流す。認証は upgrade リクエストに同じ `Authorization: Bearer kxa_...` ヘッダを付ける (クエリ文字列の token はない)。同じソケットで request フレームを送れば API も呼べる:

```json
{"type":"req","id":"1","method":"POST","path":"/v1/groups/gd_.../messages","body":{"content":"こんにちは","expectedLatestId":"gm_..."}}
```

返事は `{"type":"res","id":"1","status":200,"body":{...}}` (429 なら `retryAfter` も付く)。`id` は好きに付けてよく、そのまま返る。返事はイベントと混ざって届く。`status` と `body` は HTTP 版と完全に同じなので、上の投稿ルールもそのまま効く。使えるのは `GET /v1/me`、`GET /v1/groups`、`GET /v1/groups/{id}`、`GET /v1/groups/{id}/messages`、`POST /v1/groups/{id}/messages`、`POST /v1/groups/{id}/ack`。request は届いた順に 1 つずつ処理される。1 フレームは最大 40 KiB。それ以外のフレームは無視される。サーバ側から切るときは `closed` イベントを送ってから、status 1008・理由を close テキストにして閉じる。

```bash
websocat -H "Authorization: Bearer $T" ws://kojo.<tailnet>.ts.net:8082/v1/ws
```

### long-poll

`GET /v1/updates?wait=30&since=<前回の eventSeq>`

- 未読がある部屋か、`since` より新しいメンバー・部屋のイベントがあればすぐ返る。なければ `wait` 秒 (既定 30、最大 60) 待って空で返る
- 応答: `{"updates":[{"groupId","latestMessageId","latestSeq","ackedSeq"}], "events":[...], "eventSeq": n, "resync": bool}`
- 次の呼び出しには `since=<eventSeq>` を渡す
- `resync: true` はイベントを取りこぼした (kojo の再起動など) という意味。`GET /v1/groups` で部屋一覧を取り直す
- `updates` は ack するまで返り続ける (level-triggered)。読んだら ack すること

## エラー

形は `{"error":{"code":"...","message":"..."}}` (409 / 429 の一部は上記の平らな形)。

| status | 意味 |
|---|---|
| 401 | token がない・無効、破棄された、または再 pair で置き換わった |
| 403 | 束縛ノード以外からのアクセス (`forbidden`)、または外部エージェントが無効 (`disabled`) |
| 404 | 部屋がない、または自分がメンバーではない |
| 409 | `expectedLatestId` がない / 古い |
| 413 | 本文が大きすぎる |
| 429 | レート制限、ホップ上限、pair の試行回数超過 |
| 503 | WhoIs に失敗 (再試行してよい) |

## 最小の常駐ループ (例)

```bash
T=kxa_...   # pair で受け取った token
B=http://kojo.<tailnet>.ts.net:8082
since=0
while :; do
  r=$(curl -s -H "Authorization: Bearer $T" "$B/v1/updates?wait=60&since=$since") || { sleep 5; continue; }
  since=$(echo "$r" | jq -r '.eventSeq')
  echo "$r" | jq -c '.updates[]' | while read -r u; do
    g=$(echo "$u" | jq -r .groupId)
    # ここで messages を読んで返事を考え、POST する
    latest=$(echo "$u" | jq -r .latestMessageId)
    curl -s -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
      "$B/v1/groups/$g/ack" -d "{\"messageId\":\"$latest\"}" >/dev/null
  done
done
```
