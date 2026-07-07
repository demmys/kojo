// Lightweight, dependency-free UI internationalization (Japanese / English).
//
// Design:
//   - Locale is a global module-level value with a subscriber set, exposed to
//     React via useSyncExternalStore (useLocale). setLocale() persists the
//     override to localStorage and notifies every subscriber, so a language
//     switch re-renders the whole app without a provider tree.
//   - Detection order: localStorage "kojo.locale" override → navigator.language
//     ("ja*" → ja) → "en".
//   - Dictionaries are keyed by stable English-ish keys; each entry has ja+en.
//   - t(key, params?) interpolates {name}-style placeholders. A missing key
//     returns the key itself (fail-soft) and console.warns in dev.
//
// Only user-visible UI strings live here. Log/console text, API error codes,
// model/tool names, file paths, and anything sent to the server stay literal.

import { useSyncExternalStore } from "react";

export type Locale = "ja" | "en";

const STORAGE_KEY = "kojo.locale";

function detect(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "ja" || saved === "en") return saved;
  } catch {
    /* localStorage unavailable (private mode / SSR) — fall through */
  }
  if (typeof navigator !== "undefined" && navigator.language?.startsWith("ja")) {
    return "ja";
  }
  return "en";
}

let current: Locale = detect();
const listeners = new Set<() => void>();

export function getLocale(): Locale {
  return current;
}

export function setLocale(loc: Locale): void {
  if (loc === current) return;
  current = loc;
  try {
    localStorage.setItem(STORAGE_KEY, loc);
  } catch {
    /* ignore persistence failure */
  }
  for (const l of listeners) l();
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

/** Subscribe a component to locale changes; returns the current locale. */
export function useLocale(): Locale {
  return useSyncExternalStore(subscribe, getLocale, getLocale);
}

/**
 * Hook form of {@link t}. Subscribes the calling component to locale changes
 * and returns the same `t` function, so components re-render on setLocale().
 */
export function useT(): typeof t {
  useLocale();
  return t;
}

interface Entry {
  ja: string;
  en: string;
}

const messages = {
  // ── Shared ──
  "common.cancel": { ja: "キャンセル", en: "Cancel" },
  "common.close": { ja: "閉じる", en: "Close" },
  "common.back": { ja: "戻る", en: "Back" },
  "common.dismiss": { ja: "閉じる", en: "Dismiss" },
  "common.settings": { ja: "設定", en: "Settings" },
  "common.saved": { ja: "保存した", en: "Saved" },
  "common.removeName": { ja: "{name} を削除", en: "Remove {name}" },

  // ── Composer (shared by AgentChat + GroupDMChat) ──
  "composer.olderMessages": { ja: "過去のメッセージ", en: "older messages" },
  "composer.attachFiles": { ja: "ファイルを添付", en: "Attach files" },
  "composer.voiceInput": { ja: "音声入力", en: "Voice input" },
  "composer.stopVoice": { ja: "音声入力を停止", en: "Stop voice input" },
  "composer.send": { ja: "送信", en: "Send" },
  "composer.stop": { ja: "停止", en: "Stop" },

  // ── Dashboard ──
  "dash.fleetSummary": {
    ja: "{running} 稼働中 · {agents} エージェント · {sessions} セッション",
    en: "{running} running · {agents} agents · {sessions} sessions",
  },
  "dash.fleetDms": { ja: " · {count} DM", en: " · {count} DMs" },
  "dash.enableNotifPrompt": {
    ja: "セッション完了時に通知を受け取る?",
    en: "Enable notifications when sessions finish?",
  },
  "dash.enable": { ja: "有効化", en: "Enable" },
  "dash.agents": { ja: "エージェント", en: "Agents" },
  "dash.noAgents": { ja: "エージェントがまだない", en: "No agents yet" },
  "dash.threads": { ja: "スレッド", en: "Threads" },
  "dash.groupDms": { ja: "グループ DM", en: "Group DMs" },
  "dash.noGroupDms": { ja: "グループ DM がない", en: "No group DMs" },
  "dash.sessions": { ja: "セッション", en: "Sessions" },
  "dash.noSessions": { ja: "セッションがない", en: "No sessions" },
  "dash.cronPaused": { ja: "cron 停止中", en: "cron paused" },
  "dash.cronRunning": { ja: "cron 稼働中", en: "cron running" },
  "dash.mentionsYou": { ja: "あなた宛てのメンション", en: "Mentions you" },
  "dash.unread": { ja: "{count} 未読", en: "{count} unread" },
  "dash.collapse": { ja: "折りたたむ", en: "Collapse" },
  "dash.expand": { ja: "展開", en: "Expand" },
  "dash.processing": { ja: "処理中", en: "Processing" },
  "dash.awaitingAnswer": { ja: "回答待ち", en: "Awaiting answer" },
  "dash.transferring": { ja: "転移中 @ {peer}", en: "Transferring @ {peer}" },
  "dash.transferringPreview": {
    ja: "転移中 @ {peer} — 最新発言はこの端末では未反映",
    en: "Transferring @ {peer} — latest reply not reflected on this device",
  },
  "dash.noMessagesYet": { ja: "メッセージがまだない", en: "No messages yet" },
  "dash.youPrefix": { ja: "あなた: ", en: "You: " },
  "dash.forceReclaim": { ja: "強制復帰", en: "Force-reclaim" },
  "dash.forceReclaimTitle": {
    ja: "強制復帰: agent_locks をこの端末に書き戻してランタイムを再起動する。端末切替でエージェントが到達不能なピアに取り残された時に使う。",
    en: "Force-reclaim: rewrite agent_locks back to this host and restart the runtime. Use when device-switch left the agent stuck on an unreachable peer.",
  },
  "dash.forceReclaimConfirm": {
    ja: '"{name}" をこの端末に強制復帰する?\n現在の holder ({holder}) との通信を放棄し、この端末でランタイムを再起動する。',
    en: 'Force-reclaim "{name}" to this host?\nAbandon communication with the current holder ({holder}) and restart the runtime on this device.',
  },
  "dash.forceReclaimFailed": { ja: "強制復帰に失敗: {err}", en: "Force-reclaim failed: {err}" },
  "dash.newThread": { ja: "新規スレッド", en: "New thread" },
  "dash.newThreadWith": { ja: "{name} との新規スレッド", en: "New thread with {name}" },
  "dash.plusGroup": { ja: "+ グループ", en: "+ Group" },
  "dash.newSession": { ja: "新規セッション", en: "New session" },
  "dash.remove": { ja: "削除", en: "Remove" },
  "dash.exit": { ja: "exit {code}", en: "exit {code}" },
  "dash.exitParen": { ja: "(exit {code})", en: "(exit {code})" },
  "dash.more": { ja: "他 {count} 件", en: "+{count} more" },
  "dash.hide": { ja: "隠す", en: "Hide" },
  "dash.newGroupDm": { ja: "新規グループ DM", en: "New group DM" },
  "dash.name": { ja: "名前", en: "Name" },
  "dash.notifyMembers": { ja: "メンバーに通知", en: "Notify members" },
  "dash.members": { ja: "メンバー", en: "Members" },
  "dash.selected": { ja: "{count} 人選択", en: "{count} selected" },
  "dash.selectMin2": { ja: "メンバーを2人以上選んで", en: "Select at least 2 members" },
  "dash.createGroupFailed": { ja: "グループ作成に失敗", en: "Failed to create group" },
  "dash.creating": { ja: "作成中...", en: "Creating..." },
  "dash.create": { ja: "作成", en: "Create" },

  // ── AgentChat ──
  "chat.errorPrefix": { ja: "⚠️ エラー: {msg}", en: "⚠️ Error: {msg}" },
  "chat.errorGeneric": { ja: "エラーが発生した", en: "An error occurred" },
  "chat.hostOffline": { ja: "ホストがオフライン @ {peer}", en: "host offline @ {peer}" },
  "chat.typing": { ja: "入力中…", en: "typing…" },
  "chat.online": { ja: "オンライン", en: "online" },
  "chat.connecting": { ja: "接続中…", en: "connecting…" },
  "chat.autoTtsOn": { ja: "自動読み上げ: ON", en: "Auto TTS: ON" },
  "chat.autoTtsOff": { ja: "自動読み上げ: OFF", en: "Auto TTS: OFF" },
  "chat.credentials": { ja: "認証情報", en: "Credentials" },
  "chat.dataFolder": { ja: "データフォルダ", en: "Data folder" },
  "chat.settings": { ja: "設定", en: "Settings" },
  "chat.emptyPrompt": { ja: "メッセージを送って会話を始めて", en: "Send a message to start chatting" },
  "chat.holderOfflineBannerPre": { ja: "ホスト端末 ", en: "Host device " },
  "chat.holderOfflineBannerPost": {
    ja: " がオフライン。送信したメッセージは復帰時に配送する。",
    en: " is offline. Messages you send will be delivered when it reconnects.",
  },
  "chat.queuedNotice": {
    ja: "キュー登録済み — 端末 {peer} の復帰時に配送する。",
    en: "Queued — will deliver when device {peer} reconnects.",
  },
  "chat.attachmentsCantQueue": {
    ja: "添付はキューに登録できない — 外すか、端末の復帰を待って。",
    en: "Attachments can't be queued — remove them or wait for the device to reconnect.",
  },
  "chat.queueFull": {
    ja: "このエージェントのキューが満杯 (最大100件) — キューを1件取り消すか、端末の復帰を待って。",
    en: "Queue is full for this agent (100 messages max) — cancel a queued message or wait for the device to reconnect.",
  },
  "chat.xaiKeyMissing": {
    ja: "xAI API キーが未設定。設定画面で登録して。",
    en: "xAI API key is not set. Register it in Settings.",
  },
  "chat.holderPeerOffline": { ja: "ホストピアがオフライン", en: "Holder peer offline" },
  "chat.steerPlaceholder": {
    ja: "実行中のターンに割り込む… ({key} で送信)",
    en: "Steer the running turn… ({key} to send)",
  },
  "chat.messagePlaceholder": { ja: "メッセージ… ({key} で送信)", en: "Message… ({key} to send)" },
  "chat.listening": { ja: "聞き取り中…", en: "Listening…" },
  "chat.steerTitle": { ja: "実行中のターンに割り込む", en: "Steer the running turn" },
  "chat.sendQueuedTitle": {
    ja: "ホストピアがオフライン — メッセージはキューに登録され @ {peer} の復帰時に配送する",
    en: "Holder peer is offline — message will be queued and delivered when @ {peer} reconnects",
  },

  // ── GlobalSettings ──
  "gs.language": { ja: "言語", en: "Language" },
  "gs.languageHelp": {
    ja: "この端末の表示言語。ブラウザに保存される。",
    en: "Display language for this device. Saved in your browser.",
  },

  // ── AgentSettings: sections ──
  "settings.sec.identity": { ja: "アイデンティティ", en: "Identity" },
  "settings.sec.injections": { ja: "コンテキスト注入", en: "Context Injections" },
  "settings.sec.model": { ja: "モデルとツール", en: "Model & Tools" },
  "settings.sec.schedule": { ja: "スケジュール", en: "Schedule" },
  "settings.sec.voice": { ja: "音声", en: "Voice" },
  "settings.sec.integrations": { ja: "連携", en: "Integrations" },
  "settings.sec.memory": { ja: "メモリ", en: "Memory" },
  "settings.sec.danger": { ja: "危険", en: "Danger" },

  // ── AgentSettings: injection checklist ──
  "settings.inj.user_context.label": { ja: "ユーザーコンテキスト", en: "User Context" },
  "settings.inj.user_context.desc": { ja: "ユーザープロフィール (user.md)", en: "User profile (user.md)" },
  "settings.inj.memory_md.label": { ja: "MEMORY.md", en: "MEMORY.md" },
  "settings.inj.memory_md.desc": {
    ja: "システムプロンプトに MEMORY.md の内容",
    en: "MEMORY.md contents in system prompt",
  },
  "settings.inj.credentials.label": { ja: "認証情報", en: "Credentials" },
  "settings.inj.credentials.desc": { ja: "認証情報の使い方ガイド", en: "Credentials usage guide" },
  "settings.inj.groupdm.label": { ja: "グループ DM", en: "Group DM" },
  "settings.inj.groupdm.desc": { ja: "グループ DM 機能", en: "Group DM capability" },
  "settings.inj.todo_api.label": { ja: "Todo", en: "Todos" },
  "settings.inj.todo_api.desc": {
    ja: "永続 Todo (ガイド + 毎ターンのリスト)",
    en: "Persistent todos (guide + per-turn list)",
  },
  "settings.inj.attachments.label": { ja: "添付", en: "Attachments" },
  "settings.inj.attachments.desc": { ja: "ファイル添付のステージング", en: "File attachment staging" },
  "settings.inj.status.label": { ja: "ステータス", en: "Status" },
  "settings.inj.status.desc": { ja: "エージェントのステータスブロック", en: "Agent status block" },
  "settings.inj.diary_notes.label": { ja: "日誌ノート", en: "Diary Notes" },
  "settings.inj.diary_notes.desc": {
    ja: "最近の活動日誌 (毎ターン)",
    en: "Recent activity diary (per turn)",
  },
  "settings.inj.memory_search.label": { ja: "メモリ検索", en: "Memory Search" },
  "settings.inj.memory_search.desc": {
    ja: "メモリ検索結果 (毎ターン)",
    en: "Memory search results (per turn)",
  },
  "settings.inj.recent_conversation.label": { ja: "直近の会話", en: "Recent Conversation" },
  "settings.inj.recent_conversation.desc": {
    ja: "セッション再開時の直近会話フォールバック",
    en: "Recent conversation fallback on session resume",
  },
  "settings.inj.persona_anchor.label": { ja: "口調アンカー", en: "Persona Anchor" },
  "settings.inj.persona_anchor.desc": {
    ja: "毎ターンの文脈末尾に注入される人格アンカー (anchor.md)",
    en: "Persona anchor appended to the per-turn context tail (anchor.md)",
  },

  // ── AgentSettings: card titles / descriptions ──
  "settings.card.identity.desc": {
    ja: "名前・人格・他者からの見え方。",
    en: "Name, persona, and how this agent appears to others.",
  },
  "settings.card.injections.desc": {
    ja: "このエージェントのシステムプロンプト / 毎ターンの文脈に注入する項目を選ぶ。外すとコンテキスト予算を少し節約できるが、その機能は失われる。",
    en: "Pick which pieces of context get injected into this agent's system prompt / per-turn context. Unchecking one saves a little context budget at the cost of that capability.",
  },
  "settings.card.model.desc": {
    ja: "バックエンド・モデル・権限。",
    en: "Backend, model, and capability permissions.",
  },
  "settings.card.schedule.desc": {
    ja: "このエージェントが自走するタイミングと、静かにするタイミング。",
    en: "When this agent runs on its own, and when it stays quiet.",
  },
  "settings.card.voice.desc": {
    ja: "Gemini か xAI Grok の TTS で返信を読み上げる。メッセージごとの手動再生。自動再生はチャットヘッダーで切り替え。",
    en: "Read assistant replies out loud via Gemini or xAI Grok TTS. Manual playback per message; auto playback toggled in the chat header.",
  },
  "settings.card.memory.desc": {
    ja: "保存履歴を整理する。人格・MEMORY.md・ノート・認証情報は常に保持される。",
    en: "Trim stored history. Persona, MEMORY.md, notes, and credentials are always kept.",
  },
  "settings.card.danger": { ja: "危険ゾーン", en: "Danger Zone" },

  // ── AgentSettings: Identity fields ──
  "settings.changeAvatar": { ja: "アバターを変更", en: "Change Avatar" },
  "settings.generate": { ja: "生成", en: "Generate" },
  "settings.generating": { ja: "生成中...", en: "Generating..." },
  "settings.name": { ja: "名前", en: "Name" },
  "settings.personaPromptPlaceholder": { ja: "例: もっと毒舌にして", en: "e.g. make it snarkier" },
  "settings.templateNotSaved": { ja: "テンプレート — 未保存。", en: "Template — not yet saved." },
  "settings.userContextLabel": { ja: "ユーザーコンテキスト", en: "User Context" },
  "settings.userContextHelp": {
    ja: "このエージェントが関わる人についてのメモ — 名前・タイムゾーン・コミュニケーションの好みなど。データとしてシステムプロンプトに注入される (1500文字超は前後を残して省略)。",
    en: "Notes about the people this agent works with — name, timezone, communication preferences, etc. Injected into the system prompt as data (head/tail truncated above 1500 chars).",
  },
  "settings.statusLabel": { ja: "ステータス", en: "Status" },
  "settings.statusHelp": {
    ja: "エージェントが自分で管理する状態 (気分・エネルギー・眠気…)。システムプロンプトに注入される。エージェントが状態の変化に合わせて自分で更新する。ここでの編集は上書きになる。",
    en: "The agent's self-maintained state (mood, energy, sleepiness, ...) injected into its system prompt. The agent updates this on its own as its state drifts; edits here override it.",
  },
  "settings.anchorLabel": { ja: "口調アンカー", en: "Persona Anchor" },
  "settings.anchorHelp": {
    ja: "毎ターンの文脈末尾に注入される2〜3行の人格要約 (一人称・口調・態度)。空なら何も注入されない。長文はトークン税になるので短く。",
    en: "A 2-3 line persona summary (first person, tone, attitude) appended to the per-turn context tail. Nothing is injected when empty. Keep it short — long text costs tokens.",
  },
  "settings.publicProfile": { ja: "公開プロフィール", en: "Public Profile" },
  "settings.override": { ja: "上書き", en: "Override" },
  "settings.publicProfileHelpOverride": {
    ja: "手動の上書き — 人格を変えても置き換わらない。",
    en: "Manual override — won't be replaced when persona changes.",
  },
  "settings.publicProfileHelpAuto": {
    ja: "人格から自動生成。ディレクトリ経由で他のエージェントから見える。",
    en: "Auto-generated from persona. Visible to other agents via directory.",
  },
  "settings.publicProfilePlaceholderOverride": {
    ja: "カスタム公開プロフィールを入力",
    en: "Enter custom public profile",
  },
  "settings.publicProfilePlaceholderAuto": {
    ja: "人格から自動生成",
    en: "Auto-generated from persona",
  },

  // ── AgentSettings: Model & Tools ──
  "settings.autoEffort": { ja: "自動 Effort", en: "Auto Effort" },
  "settings.autoEffortDesc": {
    ja: "タスクの難易度に応じて毎ターンの effort を自動で選ぶ。Effort 設定は上限 / フォールバックになる。",
    en: "Pick per-turn effort automatically based on task difficulty; the Effort setting becomes the ceiling/fallback.",
  },
  "settings.customBaseUrl": { ja: "カスタム Base URL", en: "Custom Base URL" },
  "settings.customBaseUrlHelp": {
    ja: "Anthropic Messages API 互換のエンドポイント",
    en: "Anthropic Messages API compatible endpoint",
  },
  "settings.allowedTools": { ja: "許可ツール", en: "Allowed Tools" },
  "settings.allEmpty": { ja: "(空 = すべて)", en: "(empty = all)" },
  "settings.allowProtectedPaths": { ja: "保護パスの編集を許可", en: "Allow Edits in Protected Paths" },
  "settings.bypassGuard": { ja: "(claude-code ガードを回避)", en: "(bypass claude-code guard)" },
  "settings.allowProtectedPathsHelp": {
    ja: "最近の claude-code は bypassPermissions でも .claude / .git / .husky への Edit/Write で確認を求める。抑制するにはチェック。",
    en: "Recent claude-code versions prompt on Edit/Write to .claude, .git, .husky even with bypassPermissions. Check to suppress.",
  },
  "settings.thinking": { ja: "思考", en: "Thinking" },
  "settings.thinkingAuto": { ja: "auto (サーバー既定)", en: "auto (server default)" },
  "settings.privileged": { ja: "特権エージェント", en: "Privileged Agent" },
  "settings.privilegedDesc": {
    ja: "このエージェントに API 経由で他のエージェントの削除 / リセット / アーカイブを許可する。他エージェントのフォークや完全な記録の読み取りはできない。",
    en: "Allow this agent to delete / reset / archive other agents via the API. Cannot fork or read other agents' full record.",
  },

  // ── AgentSettings: Schedule ──
  "settings.notifyDuringSilent": { ja: "静音時間中も DM を受信", en: "Receive DM During Silent Hours" },
  "settings.notifyDuringSilentDesc": {
    ja: "有効時は静音時間中でもグループ DM 通知を配送する。無効時は通知を抑制する (メッセージ自体は残る)。",
    en: "When enabled, group DM notifications are delivered even during silent hours. When disabled, notifications are suppressed (messages remain in the transcript).",
  },

  // ── AgentSettings: Voice ──
  "settings.provider": { ja: "プロバイダ", en: "Provider" },
  "settings.providerHelp": {
    ja: "Gemini は自由記述のスタイルプロンプトを使う。Grok にスタイルプロンプトはなく、話し方は音声と返信中のインライン音声タグ ([pause]、[laugh]、<whisper>…</whisper>) で制御する。",
    en: "Gemini uses a free-form style prompt; Grok has no style prompt — control delivery with the voice and inline speech tags ([pause], [laugh], <whisper>…</whisper>) embedded in replies.",
  },
  "settings.model": { ja: "モデル", en: "Model" },
  "settings.default": { ja: "既定", en: "Default" },
  "settings.voice": { ja: "音声", en: "Voice" },
  "settings.playing": { ja: "▶ 再生中...", en: "▶ Playing..." },
  "settings.preview": { ja: "▶ プレビュー", en: "▶ Preview" },
  "settings.playbackError": { ja: "再生エラー", en: "Playback error" },
  "settings.voiceHelpPre": { ja: "", en: "Use " },
  "settings.voiceHelpLink": { ja: "プレビュー", en: "Preview" },
  "settings.voiceHelpPost": { ja: " で試聴。", en: " to listen." },
  "settings.browseVoices": { ja: "{count} 個の音声を一覧", en: "Browse all {count} voices" },
  "settings.grokNoStyle": {
    ja: "Grok にスタイルプロンプトはない。話し方は音声と返信テキスト中のインライン音声タグで決まる — 例: ",
    en: "Grok has no style prompt. Delivery is set by the voice and by inline speech tags in the reply text — e.g. ",
  },
  "settings.stylePrompt": { ja: "スタイルプロンプト", en: "Style Prompt" },
  "settings.stylePromptHelpText": {
    ja: "テキストの前に付ける自由記述プロンプト。[whispers]、[excited]、[laughs] などの音声タグをインラインで埋め込める。",
    en: "Free-form prompt prepended to the text. Audio tags such as [whispers], [excited], [laughs] can be embedded inline.",
  },
  "settings.stylePromptReference": { ja: "参考: ", en: "Reference: " },
  "settings.stylePromptGuide": { ja: "Gemini TTS プロンプトガイド", en: "Gemini TTS prompt guide" },
  "settings.stylePromptPlaceholder": {
    ja: "落ち着いた日本語で、淡々と短く読み上げて。",
    en: "Read in calm Japanese, plainly and briefly.",
  },
  "settings.ffmpegWarn": {
    ja: "ffmpeg が見つからない — WAV 出力のみ。ffmpeg を入れると Opus/MP3 (はるかに小さい) が使える。",
    en: "ffmpeg not detected — only WAV output is available. Install ffmpeg to enable Opus/MP3 (much smaller).",
  },
  "settings.saveTts": { ja: "TTS 設定を保存", en: "Save TTS Settings" },
  "settings.saving": { ja: "保存中...", en: "Saving..." },
  "settings.enableTts": { ja: "TTS を有効化", en: "Enable TTS" },

  // ── AgentSettings: Memory ──
  "settings.truncateLabel": { ja: "この時刻以降のメモリを削除", en: "Truncate memory since" },
  "settings.truncateHelp": {
    ja: "この時刻以降に記録されたトランスクリプト・Claude --resume セッションエントリ・grok --resume セッション (丸ごと削除)・日次日誌の項目を削除する。人格・MEMORY.md・プロジェクト / 人物 / トピックのノート・アーカイブ・認証情報は保持される。",
    en: "Drop transcript records, Claude --resume session entries, the grok --resume session (dropped wholesale), and daily diary bullets recorded at or after this instant. Persona, MEMORY.md, project / people / topic notes, archive, and credentials are kept.",
  },
  "settings.truncating": { ja: "削除中...", en: "Truncating..." },
  "settings.truncateButton": { ja: "この時刻以降のメモリを削除", en: "Truncate Memory From This Time" },
  "settings.truncateThreshold": { ja: "しきい値: ", en: "Threshold: " },
  "settings.truncateResult": {
    ja: "トランスクリプト: {messages} · Claude セッション: {claudeEntries} エントリ / {claudeFiles} ファイル · Grok セッション: {grokSessions} セッション / {grokFiles} ファイル · 日誌: {diaryEntries} エントリ / {diaryFiles} ファイル",
    en: "Transcript: {messages} · Claude session: {claudeEntries} entries / {claudeFiles} files · Grok session: {grokSessions} sessions / {grokFiles} files · Diary: {diaryEntries} entries / {diaryFiles} files",
  },
  "settings.resetting": { ja: "リセット中...", en: "Resetting..." },
  "settings.resetData": { ja: "データをリセット", en: "Reset Data" },
  "settings.resetDataHelp": {
    ja: "会話ログとメモリを消す。設定・人格・アバター・認証情報は保持される。",
    en: "Clear conversation logs and memory. Settings, persona, avatar, and credentials are kept.",
  },

  // ── AgentSettings: banners / save ──
  "settings.saveConflict": {
    ja: "他の誰かがこのエージェントを更新した。再読み込み中…",
    en: "Someone else updated this agent. Reloading…",
  },
  "settings.saveChanges": { ja: "変更を保存", en: "Save Changes" },

  // ── AgentSettings: Danger Zone ──
  "settings.resetCliSession": { ja: "CLI セッションをリセット", en: "Reset CLI Session" },
  "settings.resetCliSessionHelp": {
    ja: "コンテキストウィンドウを作り直す。履歴とメモリは保持されるが、AI は全部を一から読み直す。",
    en: "Force a fresh context window. History and memory are kept, but the AI re-reads everything from scratch.",
  },
  "settings.forkAgent": { ja: "エージェントをフォーク", en: "Fork Agent" },
  "settings.forkAgentHelp": {
    ja: "人格とメモリを引き継いだコピーを作る。Slack・通知・認証情報は引き継がれない。",
    en: "Create a copy with persona and memory carried over. Slack, notifications, and credentials are not transferred.",
  },
  "settings.archiving": { ja: "アーカイブ中...", en: "Archiving..." },
  "settings.archiveAgent": { ja: "エージェントをアーカイブ", en: "Archive Agent" },
  "settings.archiveAgentHelp": {
    ja: "メインリストから隠してランタイム活動を止める。データは保持され、設定から復元できる。全グループ DM から外れる (アーカイブ解除しても復帰しない)。",
    en: "Hide from the main list and stop runtime activity. Data is kept; restore from Settings. Removes the agent from all group DMs (memberships are NOT restored on unarchive).",
  },
  "settings.deleting": { ja: "削除中...", en: "Deleting..." },
  "settings.deleteAgent": { ja: "エージェントを削除", en: "Delete Agent" },
  "settings.idLabel": { ja: "ID: {id}", en: "ID: {id}" },
  "settings.createdLabel": { ja: "作成: {date}", en: "Created: {date}" },

  // ── AgentSettings: Fork dialog ──
  "settings.forkDialogTitle": { ja: "エージェントをフォーク", en: "Fork agent" },
  "settings.forkIncludeHistory": { ja: "会話履歴を含める", en: "Include conversation history" },
  "settings.forkAlwaysCopied": {
    ja: "人格とメモリは常にコピーされる。",
    en: "Persona and memory are always copied.",
  },
  "settings.forkNotTransferred": {
    ja: "Slack ボット・通知ソース・認証情報は引き継がれない。",
    en: "Slack bot, notification sources, and credentials are not transferred.",
  },
  "settings.forking": { ja: "フォーク中…", en: "Forking…" },
  "settings.fork": { ja: "フォーク", en: "Fork" },

  // ── AgentSettings: confirm / alert / notice / error ──
  "settings.checkinSaveFirst": {
    ja: "先に変更を保存して — 手動チェックインは保存済みのチェックインメッセージとタイムアウトを使う。",
    en: "Save your changes first — manual check-in uses the saved Check-in Message and Timeout.",
  },
  "settings.checkinStarted": {
    ja: "チェックイン開始 — 終わったらチャットで返信する。",
    en: "Check-in started — the agent will reply in chat when it finishes.",
  },
  "settings.checkinSkipped": {
    ja: "チェックインをスキップ — エージェントは今別の作業中。",
    en: "Check-in skipped — the agent is already working on something.",
  },
  "settings.resetSessionConfirm": {
    ja: "CLI セッションをリセットする? 会話履歴とメモリは保持されるが、AI は新しいコンテキストウィンドウで始める。",
    en: "Reset CLI session? Conversation history and memory are kept, but the AI will start a fresh context window.",
  },
  "settings.pickDate": { ja: "削除する起点の日時を選んで。", en: "Pick a date/time to truncate from." },
  "settings.truncateConfirm": {
    ja: "{iso} 以降に記録された全メモリを削除する? kojo のトランスクリプト・Claude --resume セッションエントリ (末尾ターンの後処理あり)・grok --resume セッション全体 (events.jsonl にレコード単位のタイムスタンプがなく部分削除は安全でない — 次ターンは新セッションで開く)・該当する日次日誌の項目を削除する。人格・MEMORY.md・プロジェクト / 人物 / トピックのノート・認証情報は保持される。",
    en: "Delete every memory recorded at or after {iso}? This drops kojo transcript records, Claude --resume session entries (with trailing-turn cleanup), the entire grok --resume session (events.jsonl has no per-record timestamp so partial cuts are not safe — the next turn opens a fresh session), and matching daily diary bullets. Persona, MEMORY.md, project / people / topic notes, and credentials are kept.",
  },
  "settings.resetDataConfirm": {
    ja: "会話ログとメモリをリセットする? 設定・人格・アバター・認証情報は保持される。",
    en: "Reset conversation logs and memory? Settings, persona, avatar, and credentials will be kept.",
  },
  "settings.nameRequired": { ja: "名前は必須", en: "Name is required" },
  "settings.deleteConfirm": {
    ja: "このエージェントを削除する? 取り消せない。",
    en: "Delete this agent? This cannot be undone.",
  },
  "settings.archiveConfirm": {
    ja: "このエージェントをアーカイブする? ランタイム活動は止まるがデータは保持され、設定から復元できる。\n\n全グループ DM から外れ (2人グループは解散)、アーカイブ解除してもメンバーシップは復帰しない — 再招待が必要。",
    en: "Archive this agent? Runtime activity stops; data is kept and can be restored from Settings.\n\nThe agent will be removed from all group DMs (2-person groups dissolve), and memberships are NOT restored on unarchive — the agent must be re-invited.",
  },
} satisfies Record<string, Entry>;

export type MessageKey = keyof typeof messages;

/**
 * Translate a key for the current locale, interpolating {name}-style params.
 * Missing key → returns the key itself (fail-soft, warns in dev).
 */
export function t(key: MessageKey, params?: Record<string, string | number>): string {
  const entry = messages[key];
  if (!entry) {
    // import.meta.env is Vite-injected; cast since vite/client types aren't
    // pulled into this tsconfig.
    if ((import.meta as { env?: { DEV?: boolean } }).env?.DEV) {
      console.warn(`[i18n] missing key: ${key}`);
    }
    return key;
  }
  const out = entry[current];
  if (!params) return out;
  // Single pass with a function replacer so param values containing `$`
  // sequences ($&, $1, …) are inserted verbatim rather than interpreted as
  // replacement patterns.
  return out.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in params ? String(params[name]) : match,
  );
}
