import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { ttsApi, pickBestFormat, type TTSCapability } from "../lib/ttsApi";

const AUTO_KEY_PREFIX = "kojo:tts:auto:";

export function autoKey(agentId: string): string {
  return AUTO_KEY_PREFIX + agentId;
}

// useTTSAutoToggle persists the per-agent "auto-play on agent reply"
// preference in localStorage. Default: OFF.
//
// When agentId changes (navigating between agents) state is re-loaded
// from localStorage so we don't accidentally write the previous agent's
// toggle into the new agent's storage key. The initial-render value is
// also read from storage so the first paint is correct.
export function useTTSAutoToggle(agentId: string | undefined) {
  const readFromStorage = (id: string | undefined): boolean => {
    if (!id) return false;
    try {
      return localStorage.getItem(autoKey(id)) === "1";
    } catch {
      return false;
    }
  };
  const [auto, setAuto] = useState<boolean>(() => readFromStorage(agentId));
  // Track which agentId the current state belongs to so we can detect
  // an external switch and refuse to write the old value to the new key.
  const lastIdRef = useRef<string | undefined>(agentId);

  useEffect(() => {
    if (lastIdRef.current !== agentId) {
      lastIdRef.current = agentId;
      setAuto(readFromStorage(agentId));
      return;
    }
    if (!agentId) return;
    try {
      localStorage.setItem(autoKey(agentId), auto ? "1" : "0");
    } catch {
      /* localStorage may be unavailable in private mode — fail silent */
    }
  }, [agentId, auto]);
  return [auto, setAuto] as const;
}

// useTTSCapability fetches the server's capability descriptor once and
// caches it on the module. The capability rarely changes within a
// session so a single fetch is fine.
let capabilityCache: TTSCapability | null = null;
let capabilityPromise: Promise<TTSCapability> | null = null;

export function useTTSCapability() {
  const [cap, setCap] = useState<TTSCapability | null>(capabilityCache);
  useEffect(() => {
    if (cap) return;
    if (!capabilityPromise) {
      capabilityPromise = ttsApi.capability();
    }
    capabilityPromise
      .then((c) => {
        capabilityCache = c;
        setCap(c);
      })
      .catch(() => {
        // Leave cap as null — UI hides TTS controls.
      });
  }, [cap]);
  return cap;
}

export type PlayState = "idle" | "loading" | "playing" | "error";

// ─────────────────────────────────────────────────────────────────────────
// Shared audio singleton
//
// iOS Safari only keeps an <audio>/Audio() element "unlocked" for later
// programmatic play() if the SAME element was first started from a user
// gesture. Constructing a fresh Audio() per message (the old behaviour)
// re-locks on every play, so screen-off / queued auto-play breaks. We keep
// ONE element for the tab's lifetime, unlocked by the first manual play,
// and reuse it for every subsequent (including programmatic) playback.
// ─────────────────────────────────────────────────────────────────────────

interface QueueItem {
  id: string;
  url: string;
  title: string;
}

// A ~0.01s silent WAV. Playing it on the shared element inside a user
// gesture "unlocks" the element on iOS so later programmatic play() calls
// (auto-play, queued replies) succeed even though they run after an async
// synthesize (which breaks the gesture chain). Done once per tab.
const SILENT_WAV =
  "data:audio/wav;base64,UklGRsQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YaAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
let ttsUnlocked = false;

// player is the module-level controller wrapping the single audio element.
// A version counter drives React re-renders via useSyncExternalStore.
const player = {
  audio: null as HTMLAudioElement | null,
  activeId: null as string | null,
  queue: [] as QueueItem[],
  states: {} as Record<string, PlayState>,
  version: 0,
  // epoch bumps on stop()/manual play so stale queued synths (fired before
  // the reset) can detect they've been superseded and bail.
  epoch: 0,
  listeners: new Set<() => void>(),

  subscribe(fn: () => void) {
    player.listeners.add(fn);
    return () => player.listeners.delete(fn);
  },
  emit() {
    player.version++;
    player.listeners.forEach((fn) => fn());
  },
  setState(id: string, s: PlayState) {
    player.states = { ...player.states, [id]: s };
    player.emit();
  },

  ensureAudio(): HTMLAudioElement | null {
    if (typeof Audio === "undefined") return null;
    if (player.audio) return player.audio;
    const a = new Audio();
    a.preload = "auto";
    // playsinline keeps mobile Safari from going fullscreen and lets audio
    // play with the screen on; both attribute and property for coverage.
    (a as HTMLAudioElement & { playsInline?: boolean }).playsInline = true;
    a.setAttribute("playsinline", "");
    a.addEventListener("ended", () => {
      const finished = player.activeId;
      if (finished) player.setState(finished, "idle");
      player.activeId = null;
      // Chain to the next queued item on the SAME (still-unlocked, and if
      // audio was actively playing, still-alive) element — far more
      // reliable with the screen off than starting a new element.
      player.playNext();
    });
    a.addEventListener("error", () => {
      const cur = player.activeId;
      if (cur) player.setState(cur, "error");
      player.activeId = null;
      // Don't let a single decode/network error stall the queue.
      player.playNext();
    });
    player.audio = a;
    return a;
  },

  setMediaSession(title: string) {
    const ms = (navigator as Navigator & { mediaSession?: MediaSession })
      .mediaSession;
    if (!ms) return;
    try {
      if ("MediaMetadata" in window) {
        ms.metadata = new MediaMetadata({ title, artist: "kojo" });
      }
      ms.setActionHandler("play", () => {
        player.audio?.play().catch(() => {});
      });
      ms.setActionHandler("pause", () => player.audio?.pause());
      ms.setActionHandler("stop", () => player.stop());
      ms.playbackState = "playing";
    } catch {
      /* Media Session unsupported/partial — non-fatal. */
    }
  },

  // playItem starts a specific item immediately, returning the play()
  // promise so the caller can surface autoplay-policy rejections.
  async playItem(item: QueueItem): Promise<void> {
    const a = player.ensureAudio();
    if (!a) throw new Error("audio unsupported");
    a.src = item.url;
    player.activeId = item.id;
    player.setState(item.id, "playing");
    player.setMediaSession(item.title);
    try {
      await a.play();
    } catch (e) {
      // Autoplay policy or interrupted load — reflect as error so the UI
      // shows the play button instead of a silent stall.
      player.setState(item.id, "error");
      if (player.activeId === item.id) player.activeId = null;
      throw e;
    }
  },

  playNext() {
    const next = player.queue.shift();
    if (!next) {
      const ms = (navigator as Navigator & { mediaSession?: MediaSession })
        .mediaSession;
      if (ms) ms.playbackState = "none";
      player.emit();
      return;
    }
    void player.playItem(next).catch(() => {});
  },

  enqueue(item: QueueItem) {
    player.queue.push(item);
    player.setState(item.id, "loading"); // reuse "loading" as "queued"
    // If nothing is currently playing, kick the queue immediately.
    if (!player.activeId) player.playNext();
    else player.emit();
  },

  // unlock primes the shared element from within a user gesture. Must be
  // called synchronously (no await before it) from a click handler.
  unlock() {
    if (ttsUnlocked) return;
    const a = player.ensureAudio();
    if (!a) return;
    ttsUnlocked = true;
    a.src = SILENT_WAV;
    const p = a.play();
    if (p) p.then(() => a.pause()).catch(() => {});
  },

  stop() {
    player.epoch++;
    const a = player.audio;
    if (a) {
      a.pause();
      a.removeAttribute("src");
      a.load();
    }
    // Reset every affected row to idle: the active one, anything queued,
    // and anything still awaiting synth (so a spinner never gets stuck).
    const affected = [...player.queue.map((q) => q.id), ...pendingQueued];
    player.queue = [];
    pendingQueued.clear();
    // Abandon the current synth chain; in-flight synths are epoch-guarded
    // so they won't enqueue, and new auto-plays start on a fresh chain.
    queueChain = Promise.resolve();
    const cur = player.activeId;
    player.activeId = null;
    const next = { ...player.states };
    for (const id of affected) next[id] = "idle";
    if (cur) next[cur] = "idle";
    player.states = next;
    player.emit();
  },
};

// queueChain serializes queued auto-play synths so replies play in arrival
// order and none are dropped when several complete close together.
// pendingQueued tracks ids whose synth hasn't landed in player.queue yet so
// stop() can clear their "loading" state immediately.
let queueChain: Promise<void> = Promise.resolve();
const pendingQueued = new Set<string>();

// useTTSPlayer exposes a `play(messageId, text, opts)` function plus
// per-message state, backed by the shared audio singleton above.
export function useTTSPlayer(agentId: string | undefined, enabled: boolean) {
  const cap = useTTSCapability();

  // Subscribe to the module player so state/activeId re-render this hook.
  const version = useSyncExternalStore(
    player.subscribe,
    () => player.version,
    () => player.version,
  );
  void version; // referenced only to trigger re-renders

  // Track the latest immediate (non-queued) play call so a stale fetch
  // cannot transition the wrong row to "playing".
  const generationRef = useRef(0);

  const stop = useCallback(() => {
    generationRef.current++;
    player.stop();
  }, []);

  const play = useCallback(
    async (
      messageId: string,
      text: string,
      opts?: { queue?: boolean; title?: string },
    ) => {
      if (!agentId || !enabled || !cap) return;
      const title = opts?.title ?? "kojo";
      // Prime the element within the gesture for manual (non-queued) clicks
      // so the first tap unlocks it for subsequent programmatic playback.
      if (!opts?.queue) player.unlock();

      // Toggle: clicking the currently-playing row stops it.
      if (player.activeId === messageId && !opts?.queue) {
        stop();
        player.setState(messageId, "idle");
        return;
      }

      // Auto-play path: always enqueue (serialized) so replies play in
      // arrival order, none are dropped, and an actively-playing element
      // chains to the next src — which keeps going with the screen off far
      // more reliably than constructing a fresh element per reply.
      if (opts?.queue) {
        const epochAtCall = player.epoch;
        pendingQueued.add(messageId);
        player.setState(messageId, "loading"); // shown as "queued"
        queueChain = queueChain.then(async () => {
          // A stop()/manual play since this call supersedes the queue.
          if (player.epoch !== epochAtCall) {
            pendingQueued.delete(messageId);
            player.setState(messageId, "idle");
            return;
          }
          try {
            const fmt = pickBestFormat(cap.formats);
            const res = await ttsApi.synthesize(agentId, text, fmt);
            pendingQueued.delete(messageId);
            if (player.epoch !== epochAtCall) {
              player.setState(messageId, "idle");
              return;
            }
            player.enqueue({ id: messageId, url: ttsApi.audioUrl(res.url), title });
          } catch {
            pendingQueued.delete(messageId);
            // Don't stamp an error onto a row the user already stopped.
            player.setState(messageId, player.epoch !== epochAtCall ? "idle" : "error");
          }
        });
        return;
      }

      // Immediate play: supersede any in-flight immediate fetch and any
      // current playback.
      const myGen = ++generationRef.current;
      player.setState(messageId, "loading");
      player.stop();

      try {
        const fmt = pickBestFormat(cap.formats);
        const res = await ttsApi.synthesize(agentId, text, fmt);
        if (myGen !== generationRef.current) return; // superseded
        await player.playItem({
          id: messageId,
          url: ttsApi.audioUrl(res.url),
          title,
        });
      } catch {
        if (myGen !== generationRef.current) return;
        player.setState(messageId, "error");
      }
    },
    [agentId, enabled, cap, stop],
  );

  return {
    play,
    stop,
    state: player.states,
    activeId: player.activeId,
    capability: cap,
  };
}
