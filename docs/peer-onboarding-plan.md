# Peer Onboarding 改善計画

## 現状の煩雑ポイント

1. 両ホストで kojo を起動して stderr の pairing spec を読む。
2. spec を反対側ホストに運ぶ。
3. 反対側で `kojo --peer-add '<spec>'` (or UI に貼り付け)。
4. これを双方向に。
5. trust は別フラグ / 別コマンド。
6. shell quoting で壊れる。
7. peer 側の URL を手書き。

## 要件 (ユーザー指定)

- Tailscale 参加端末は無条件で信頼する。
- peer は Hub を自動で探して信頼する。
- Hub は peer から参加要求があれば保留する。
- 設定画面で Owner 承認 → 正式参加。

## フロー

1. peer 側で `kojo --peer` を起動する。
2. peer が Hub を自動発見して参加要求を送る。
3. Hub の Settings UI で Owner が Approve → 双方向登録、trusted=true。

操作者が打つコマンドは peer 側で `kojo --peer` のみ。

### Hub 発見 (peer 側)

順に試す。

- `--hub <url>` フラグ。
- 環境変数 `KOJO_HUB_URL`。
- MagicDNS 既定名 `https://kojo.<tailnet>.ts.net:<port>`。tailnet 名は OS tailscaled の `LocalClient` から取得。port は `KOJO_HUB_PORT` (default 8080)。

発見した Hub の `GET /api/v1/peers/hub-info` を叩いて `{ deviceId, name, publicKey, url, version }` を取得し、peer の `peer_registry` に書く (= Hub を信頼)。

### 参加要求 (peer → Hub)

peer は `POST /api/v1/peers/join-request` に `{ deviceId, name, url, publicKey }` を送る。Hub の応答:

- 該当 deviceId が `peer_registry` に既に存在し、publicKey が一致 → `{ state: "approved", hub: <hub spec> }` を返す。row の name / url / last_seen は更新する。peer はこれで join ループを終了できる。
- 該当 deviceId が `peer_registry` に既に存在し、publicKey が不一致 → 409 (公開鍵 immutability)。
- それ以外 → `peer_pending` に書く (既存 row は上書き)。応答は `{ state: "pending" }`。

### Hub 側 pending 保留

```
device_id   TEXT PRIMARY KEY,
name        TEXT NOT NULL,
url         TEXT NOT NULL,
public_key  TEXT NOT NULL,
first_seen  INTEGER NOT NULL,
last_seen   INTEGER NOT NULL
```

peer は承認待ち中も定期的に join-request を再送 (= heartbeat、60s)。

### Owner 承認 (Settings UI)

Settings の Peers セクションに「Pending join requests」を追加。

- 列: name, deviceId (短縮), URL, last_seen。
- Approve: 承認時点の最新 pending row (deviceId / name / url / publicKey) を `peer_registry` に昇格、`trusted=true`。peer は次回の join-request で `state:"approved"` + hub spec を受け取る。Approve から peer の次回ポーリングまでに pending row が上書きされた場合、最新の値で登録される (Owner は registry を見て結果確認、必要なら `--peer-remove` で消して再 approve)。
- Reject: pending row 削除のみ。peer が再要求すれば再度 pending に並ぶ (永続拒否は v1 では非対応、運用で対応)。

## API surface (新規)

- `GET /api/v1/peers/hub-info` — Hub spec を返す。
- `POST /api/v1/peers/join-request` — 参加要求。応答は `{ state: "pending" | "approved", hub?: <hub spec when approved> }` または 409 (publicKey 不一致)。`rejected` 状態は返さない (Reject 後は再要求が再 pending に並ぶ仕様)。
- `GET /api/v1/peers/join-request/{deviceId}` — peer が状態を polling する経路 (応答形式は POST と同じ)。
- `GET /api/v1/peers/pending` — Owner 認証、Pending 一覧。
- `POST /api/v1/peers/pending/{deviceId}/approve` — Owner 認証。
- `POST /api/v1/peers/pending/{deviceId}/reject` — Owner 認証。

## 追加仕様

### `--tailnet-only` フラグ

peer mode (`--peer`) の listener を Tailscale interface IP (100.x / fd7a:115c::) のみに bind する起動フラグ。指定時に Tailscale が未起動 / IP が取れない場合は起動失敗。default は現行どおり `0.0.0.0`。LAN 経由のアクセスを止めたい運用で明示 opt-in する。

Hub mode は対象外 (既存どおり tsnet.ListenTLS で tailnet にしか出ない)。

### 公開鍵 immutability

一度 `peer_registry` に書いた peer の public_key は join-request 経由では変更不可。別公開鍵で再要求が来たら 409 を返し、Owner に「旧 row を削除 → 再 approve」の操作を要求する。

## 既存資産との関係

- `--peer-add` / `--peer-trust` / `--peer-remove` は残す。escape hatch。
- `internal/peer/auth_middleware.go` の Ed25519 PeerAuth は据え置き (正式参加後の通常路)。
- 既存 pairing spec の stderr 印字は当面残す。

## 影響範囲

- `cmd/kojo/main.go` — peer mode で Hub 自動発見 + join-request 送信。`--hub`, `--tailnet-only`。
- `internal/peer/discovery.go` (新規) — Hub 解決、hub-info 取得、join-request ループ。
- `internal/server/peer_handlers.go` — 新エンドポイント群。
- `internal/store/peer_pending.go` (新規) + migration — `peer_pending` table。
- `web/src/components/globalsettings/PeersSection.tsx` — Pending サブセクション、Approve / Reject。
- `README.md` / `README.ja.md` — Quick start を新フローに書き換え。

## 実装順序

1. `/peers/hub-info` 公開。
2. `peer_pending` table + join-request + pending API。
3. Settings UI の Pending セクション。
4. peer 側 Hub 自動発見 + join-request ループ。
5. README / docs 更新。
