# kojo Peer Auto-Update 計画

## 前提

- リリースは GitHub Releases (`gh release create vX.Y.Z`) で配布。シングルバイナリ (macOS arm64/amd64, Linux amd64/arm64, Windows amd64)。
- バージョンは `cmd/kojo/main.go` の `var version = "x.y.z"` と `/api/v1/info` の `version` フィールドで取得可能。
- 起動形態は (1) ユーザの shell から直接、(2) launchd / systemd / Windows サービスとしての daemon の両方ありうる。
- 複数の peer (Hub + 端末) が同じ cluster を構成しているので、Hub だけ古い・peer だけ新しい状態に陥らないようにしたい。

## ゴール

- 各 peer が新しい release を自分で取得して入れ替えるところまで自動化する。
- ユーザの操作は「アップデートを許可するか」の 1 タップのみ。完全無人運用も option として用意。
- 同じ cluster 内で peer の version drift を最小化する (Hub が leader として整合をとる)。

## 構成

### 1. 配信チャンネル

- GitHub Releases を一次配信元にする。`channels`:
  - `stable`: 最新の released tag (`vX.Y.Z`)。
  - `beta`: pre-release を含む。`--update-channel beta` で opt-in。
- 各 release アセット名は `kojo-<os>-<arch>(.exe)` を厳守し、`release.json` (= metadata: `version`, `protocol_min`, `protocol_max`, `schema_migration` フラグ, `signed_by` 鍵 ID, 各 asset の SHA256 + size) + `SHA256SUMS` + `SHA256SUMS.sig` (cosign 署名) を併置する。
  - **metadata も署名対象**: `release.json` を `SHA256SUMS` に含めるか、独立に `release.json.sig` を出して cosign 検証を必須にする。`protocol_min/max` / `schema_migration` は rollout 順や rollback 抑止の判定に使うので、改ざんを許すと cluster split や rollback bypass を招く。
- **署名鍵の信頼根**:
  - cosign 検証用の公開鍵を「現行鍵 + 次世代鍵 (pre-published)」の 2 つバイナリに埋め込む。次世代鍵は本番投入の N リリース前から埋め込んでおく (TUF root の事前配布と同型)。
  - `keys.json` だけで expiry を判断しない。各鍵の `not_before` / `not_after` を埋め込み定数として持ち、release metadata の `signed_by` が現在許容される鍵のいずれかと一致することを必須にする。
  - 鍵ローテートは「次鍵を事前埋め込み → ある時点から次鍵で署名 → 次々鍵をさらに埋め込み」と前進する。緊急 revoke は `keys.json` ではなく **次リリース** で前鍵を expire させる (古い binary は新 release を取得できないが、新 release を別経路で配布できる)。

### 2. クライアント側 internal/update パッケージ

責務:

- `Check()`: GitHub `/repos/loppo/kojo/releases/latest` (または `/releases?per_page=20`) を ETag 付きで叩き、最新タグと自分の `version` を semver 比較。channel フィルタを適用。monotonic version floor を強制 (downgrade を許可しない、過去 release への replay を防ぐ)。
- `Download()`: 該当 asset と SHA256SUMS / signature をダウンロードし、cosign で署名検証 → SHA256 一致を確認 → release metadata の `version` が `Check()` で取得したバージョンと一致することを確認 (asset name 詐称対策) → 自バイナリと同じ dir の `kojo.update-<ver>` に書き出す。書き出し後 `fsync` + 親ディレクトリ `fsync`。
- `StageInstall()`: 検証済みバイナリを **インストールせず** に staging 領域に置くだけ。実際の置き換えは下記の platform-specific helper に委ねる。
- `RequestRestart()`: 既存セッションを graceful detach し、supervisor (launchd / systemd / Windows service) に再起動シグナルを出す。kojo プロセス自身は exec 置き換えを行わない (実行中バイナリ自己置換の罠を避ける)。

**起動形態ごとの挙動**:

- **service/daemon (launchd / systemd / Windows service)**: 上記フルフロー。supervisor が再起動を担う。
- **shell 直起動 (前景 / `nohup`)**: supervisor が存在しないので auto-apply は **stage まで** に限定する。`kojo update apply` は staging + 「親プロセスを手動で再起動してね」案内 + small detached helper (kojo binary を再 exec する `kojo-restart-helper`) の起動オプションを提示する。helper は親 PID を `os.FindProcess` で監視し、終了を確認してから rename + 新 binary を `exec` する。Windows shell mode も同様の helper を使う (kojo-updater.exe を流用)。
- どの形態でも `--update-policy auto` が shell モードで動くときは default で「stage のみ + restart 通知」に降格する。

**実行中バイナリ置換の platform 差異**:

- `internal/atomicfile` は通常データファイル用で、実行中の自プロセスバイナリ置換 (Windows の sharing violation、macOS の code signing + Gatekeeper キャッシュ、launchd KeepAlive 競合) はカバーしない。専用 helper を別実装する。
- macOS / Linux: 自プロセス終了 → supervisor が `kojo.update-<ver>` を `os.Rename` で本体に被せる → supervisor が新本体を起動。再署名 (macOS) は notarized signed asset を配布することで回避。
- Windows: `kojo.exe` は実行中ロックされる。helper は別実行 (`kojo-updater.exe` を release に同梱) で、kojo プロセス終了後に `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` する。サービスとして動いている場合は SCM 経由で stop → 置換 → start。
- すべて idempotent。helper 起動後にクラッシュしても次回起動で staging ファイルから再開できる sentinel を残す。

進捗イベントは `internal/eventbus` に流して UI が購読する。

### 3. オーケストレーション

- **互換性宣言**: 各 release は `protocol_min` / `protocol_max` を release metadata に持ち、Hub / peer は自分の protocol 範囲とリリースの範囲が交わるときだけ採用する。Hub-first or peer-first のロールアウト順は protocol 互換性で決定する (Hub の min が peer の現行 protocol を切るときは peer を先に上げないと cluster が割れる)。リリースノートに rollout 方向を必須記載。
- **Hub-led update**: Hub は自分の version と各 peer の version (`peer_registry` heartbeat に `version` フィールドを追加) を見て、更新が必要な peer に署名付き update 指示を送る。
  - 並行更新は 1 peer ずつが default (rolling)。`--update-parallelism N` で上書き可能。canary 用に「最初の 1 peer が成功してから次」モードを用意。
  - update 中の peer は heartbeat 列とは別の `update_state` 列 (`idle` / `staging` / `applying` / `rolling_back` / `failed`) を返す。`peer_registry.status` の CHECK 制約に新値を足さず、Go の enum / TS の type / migration を一緒に伸ばす。
  - update 用に 2 つの表を分けて作る。
    - **Hub 側 (fleet orchestration)** `peer_update_jobs`: `(job_id, target_device_id, target_version, state, attempt, last_error_at, requested_by)`。Hub が fleet 全体の進行を管理する。
    - **各 peer 側 (local apply lock)** `update_local_jobs`: `(local_job_id, source_job_id NULL, target_version, state, attempt, last_error_at)`。`source_job_id` が NULL なら peer-local 起動、非 NULL なら Hub 指示由来。
  - per-peer lock は **peer 側 `update_local_jobs` が単一の active row** をもつ unique 制約で保証する。Hub の `peer_update_jobs` は 1 peer につき同時 1 件まで FK + unique で縛り、両者を `source_job_id` で相関させる。
  - apply API は idempotent: 同じ `local_job_id` (or `source_job_id`) で再呼び出しすると現在の進行状態を返すだけ。
  - 失敗 N 回で auto-disable し、Owner notification を出す。
- **Peer 単独 update**: Hub から指示が来なくても、ユーザが UI から「Check for updates」を押したら自分で Apply できる。peer 側で `update_local_jobs` に row を作るだけで、Hub の `peer_update_jobs` には反映されない (Hub fleet 視点では「自走 update」として heartbeat で観測する)。Hub 指示と peer 単独 update は同じ peer 上の `update_local_jobs` unique 制約で衝突を防ぐ。
- **無人モード**: `--update-policy auto` で daily に check + apply (深夜帯設定可能)。`--update-policy notify` で通知のみ (default)。`--update-policy off` で完全無効。`auto` でも下記 schema migration 境界では手動承認に降格する。

### 4. UI / CLI surface

CLI:

- `kojo update check` — 結果を stdout に。
- `kojo update apply [--channel beta]` — 1 ショット適用。終了コードで成否を返す。
- `kojo --update-policy {auto,notify,off}` — daemon の起動フラグ。
- `kojo --update-channel {stable,beta}` — channel 指定。

UI (Settings):

- 現在 version / 利用可能 version / channel selector / policy radio。
- Hub 視点で peer 一覧と各 peer の version drift を可視化 (peer_registry の version 列を表示)。
- 「Update all peers now」ボタン (Owner 限定)。

API:

- `GET /api/v1/update/status` — current/available/channel/policy + schema migration 有無。
- `POST /api/v1/update/apply` — Owner 認証で apply 開始。SSE で進捗。
- `POST /api/v1/update/peers/{id}/apply` — Hub から特定 peer に発射 (Hub→peer trust 必須)。
- `POST /api/v1/peers/update-request` — peer 側 RolePeer allowlist に追加する受信エンドポイント。trusted な Hub からの署名付き update 指示を受け取る (RolePeer allowlist と policy.go を明示更新)。
- `GET /api/v1/health` — version / schema_version / peer_identity_loaded / store_ok / session_count を返す dedicated health endpoint。`/api/v1/info` は表示用なので健康判定には使わない。

### 5. 安全策

- **rollback (schema 変更なしの版)**: Apply 前に旧バイナリを `kojo.prev` にコピー。新版が起動 60s 以内に `/api/v1/health` で `200 + schema_version 一致` を返さなければ supervisor が `kojo.prev` に戻して再起動する。
- **schema migration 境界では auto rollback 禁止**: SQLite schema migration を伴う update は forward-only。旧 binary が新 schema を拒否して起動不能になる二次障害を避けるため、自動 rollback を無効化する。代わりに:
  - Apply 直前に **フル snapshot** を quiesce 状態で取得する。`kojo.db` だけでは足りない (blob storage、`auth/kek.bin`、VAPID 鍵、その他 sidecar ファイルの整合性が壊れる)。実装は:
    - 全 session を graceful detach し、agent dispatch を停止して store への新規 write を凍結 (write lock)。
    - SQLite は `BEGIN IMMEDIATE` + ファイルコピー (`internal/snapshot` の既存経路を流用しつつ、`internal/blob` の root directory も合わせて固める)。
    - configdir 配下を `kojo-snapshot-<ver>-<ts>.tar.zst` にまとめ、SHA256 を残す。
  - migration 失敗時は新 binary が「migration failed, restore guidance:」を Owner notification に出して停止。手動 restore (`kojo --restore <snapshot>` + 旧 binary) を案内する。snapshot 復元は configdir 全体を入れ替える経路を `restore_cmd.go` に持たせる (現状の `--snapshot` / `--clean` 系を拡張)。
  - `--update-policy auto` であっても schema migration を伴う release は default で手動承認に降格 (`auto-allow-schema-migration` を別フラグで明示 opt-in)。
  - 互換性 matrix を release notes に必須記載 (どの旧 version からの直接 update を許容するか)。
- **署名検証必須**: cosign 検証失敗 / SHA mismatch は即 abort。エラーは Owner notification として残す。
- **同時 update の直列化**: 上記 `peer_update_jobs` の per-peer lock で保証。
- **session 退避**: 更新前に各 session に SIGHUP ではなく graceful detach 通知を出し、xterm 側でも「Updating, will resume…」表示。
  - Unix (mac/Linux): 既存の persist-across-restart の仕組みで再開する (`107df17` 系)。`internal/session.Restart` という固有 API は前提にしない (実在しないので、既存の session 復元経路を流用する)。
  - Windows: ConPTY のセッション再開は OS 側のサポートが限定的なので、active session がある状態の auto-apply は default で確認ダイアログに降格 (`--update-policy auto` でも UI / notification で operator に確認を取る)。

### 6. ネットワーク考慮

- GitHub API の rate limit (60 req/h/IP) は ETag + 24h cache で回避。
- 大企業ネット等で GitHub への直アクセスができないケース用に `--update-source <url>` で社内 mirror URL を許容。mirror も同じ cosign 公開鍵で署名された artifact を返す前提 (mirror 自体は信頼根にしない)。
- tsnet 経由でしか外に出られない peer は Hub に proxy してもらう (`POST /api/v1/update/proxy-fetch` を Hub に。Hub だけが GitHub に出る)。
- **SSRF / replay 対策**:
  - `--update-source` は host allowlist と HTTPS 必須。リダイレクトは無効 (or 同一 host 内のみ許容)。
  - proxy-fetch も同様に Hub 側で URL の host を allowlist チェック。任意 URL 取得経路にしない。
  - 取得後に署名検証 + version monotonicity チェックを必ず通す。古い valid release を投げ返す replay は monotonic floor で弾く。

## 実装順序

fleet 機能を最後に回し、単体で完結する単位から順に出す。

1. **Check + Download + Verify**: `internal/update` の readonly 経路だけ作る。`kojo update check` で stdout に結果を出すだけで、apply はしない。cosign 検証 + SHA + monotonic floor を必須化。
2. **Manual Apply + per-host lock**: `kojo update apply` を実装。platform-specific helper (mac/Linux supervisor + Windows kojo-updater.exe) を同時に出す。並行 apply を防ぐ local lock を入れる。
3. **Supervisor + Health-gated rollback**: launchd / systemd / Windows service の plist を整備し、`/api/v1/health` 60s grace で旧 binary に戻す。schema 変更なし版に限定。
4. **Schema migration ガード**: snapshot 自動取得 + auto rollback 無効化 + manual restore 案内。`auto-allow-schema-migration` フラグ。
5. **Peer fleet (Hub-led)**: `peer_registry` に version 列追加、heartbeat 報告、`peer_update_jobs` 表、Hub UI の drift 表示、`/peers/{id}/apply`、peer 側 `/peers/update-request` allowlist 追加、protocol 互換性チェック。
6. **--update-policy auto**: cron 化。schema migration は手動承認に降格する分岐込み。
7. **Mirror / proxy-fetch**: SSRF allowlist と replay 対策込みで最後に追加。

## 影響範囲

- `internal/update/` — 新規 (Check / Download / Verify / StageInstall / RequestRestart, platform_unix.go, platform_windows.go)
- `internal/update/keys.go` — 埋め込み公開鍵セット (現行 + 次世代)
- `internal/peer/registrar.go` — heartbeat に version 追加
- `internal/store/peer_registry.go` (+ migration) — `version TEXT`, `update_state TEXT` 追加。`peer_update_jobs` テーブル新規。
- `internal/server/peer_handlers.go` / `update_handlers.go` (新規) — `/update/*`, `/peers/update-request`, `/health` ハンドラ
- `internal/auth/policy.go` — RolePeer allowlist に `/api/v1/peers/update-request` を追加
- `internal/snapshot/` — schema migration 前の snapshot 取得経路を update から呼べるよう公開関数追加
- `cmd/kojo/main.go` — `--update-policy`, `--update-channel`, `--update-source`, `update` サブコマンド
- `cmd/kojo-updater/` (Windows) — 別バイナリ (release に同梱)
- `web/src/components/globalsettings/` — Update セクション / Peers 列追加 / migration 確認モーダル
- リリース pipeline — cosign 署名生成、SHA256SUMS 出力、複数 OS/arch ビルド、release metadata (protocol_min/max, schema_migration フラグ) の自動生成、macOS notarization、Windows Authenticode (`Makefile` の `release` ターゲット追加)

## 残課題

- macOS の Gatekeeper / notarization。signed Apple Developer ID で codesign 必要。リリースワークフローに組み込む。
- Windows SmartScreen 対策 (EV 証明書 or SmartScreen reputation 待ち)。当面は signed cosign artifact + 警告許容で進める。
- mobile 端末 (iOS / Android) は kojo binary を動かしていないので update 対象外。Hub の version が変わったら PWA 側の version 表示だけ更新する。
