import { useEffect, useState, useCallback } from "react";
import { api } from "../lib/api";

type PushState = "unsupported" | "default" | "granted" | "denied";

function urlBase64ToUint8Array(base64String: string): Uint8Array<ArrayBuffer> {
  const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
  const rawData = atob(base64);
  const buf = new ArrayBuffer(rawData.length);
  const view = new Uint8Array(buf);
  for (let i = 0; i < rawData.length; i++) {
    view[i] = rawData.charCodeAt(i);
  }
  return view;
}

function applicationServerKeysMatch(
  existing: PushSubscription,
  expected: Uint8Array,
): boolean {
  // PushSubscriptionOptions.applicationServerKey is an ArrayBuffer (or null).
  const buf = existing.options?.applicationServerKey;
  if (!buf) return false;
  const a = new Uint8Array(buf as ArrayBuffer);
  if (a.length !== expected.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== expected[i]) return false;
  }
  return true;
}

export function usePushNotifications() {
  const [state, setState] = useState<PushState>("unsupported");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
      setState("unsupported");
      return;
    }
    const perm = Notification.permission as PushState;
    setState(perm);

    // auto-resubscribe if already granted (handles server restart)
    if (perm === "granted") {
      resubscribe();
    }
  }, []);

  const resubscribe = async () => {
    try {
      const registration = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;
      const vapidKey = await api.push.vapidKey();
      const applicationServerKey = urlBase64ToUint8Array(vapidKey);

      let existing = await registration.pushManager.getSubscription();
      // If the server regenerated its VAPID keys, the browser still holds an
      // old subscription whose applicationServerKey no longer matches. Re-using
      // it would make every send fail VAPID validation, so drop it and
      // re-subscribe with the current key. Also tell the server to forget the
      // stale endpoint so it stops trying to push to it.
      if (existing && !applicationServerKeysMatch(existing, applicationServerKey)) {
        const staleEndpoint = existing.endpoint;
        try {
          await existing.unsubscribe();
        } catch {
          // ignore - we'll just re-subscribe below
        }
        try {
          await api.push.unsubscribe(staleEndpoint);
        } catch {
          // ignore - server-side cleanup is best-effort
        }
        existing = null;
      }
      if (!existing) {
        existing = await registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey,
        });
      }
      await api.push.subscribe(existing.toJSON());
    } catch {
      // silent - best effort
    }
  };

  const subscribe = useCallback(async () => {
    if (state === "unsupported" || state === "denied") return;
    setLoading(true);
    try {
      const registration = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;

      const vapidKey = await api.push.vapidKey();
      const applicationServerKey = urlBase64ToUint8Array(vapidKey);

      // Drop any stale subscription whose applicationServerKey doesn't match
      // the current server VAPID key. PushManager.subscribe would otherwise
      // throw InvalidStateError when a mismatched subscription already exists.
      const stale = await registration.pushManager.getSubscription();
      if (stale && !applicationServerKeysMatch(stale, applicationServerKey)) {
        const staleEndpoint = stale.endpoint;
        try {
          await stale.unsubscribe();
        } catch {
          // ignore - subscribe() below will surface a real error if any
        }
        try {
          await api.push.unsubscribe(staleEndpoint);
        } catch {
          // ignore - server-side cleanup is best-effort
        }
      }

      const subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey,
      });

      await api.push.subscribe(subscription.toJSON());
      setState("granted");
    } catch (err) {
      console.error("Push subscription failed:", err);
      if (Notification.permission === "denied") {
        setState("denied");
      }
    } finally {
      setLoading(false);
    }
  }, [state]);

  return { state, loading, subscribe };
}
