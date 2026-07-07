self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data.json();
  } catch {
    // fallback
  }

  let title = "kojo";
  let body = "";
  let tag = "kojo";
  let navData = {};
  let renotify = false;

  if (data.type === "agent_chat_done") {
    title = data.name || "Agent";
    body = data.preview || "Response ready";
    tag = `kojo-agent-${data.agentId}`;
    navData = { agentId: data.agentId };
  } else if (data.type === "agent_awaiting_input") {
    title = `回答待ち: ${data.name || "Agent"}`;
    body = "エージェントが質問への回答を待っています";
    tag = `kojo-agent-${data.agentId}`;
    navData = { agentId: data.agentId };
    // A second question raised on the same agent while an earlier
    // notification is still sitting unread would otherwise be silently
    // folded into the same tag with no re-alert.
    renotify = true;
  }

  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag,
      data: navData,
      renotify,
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const { agentId } = event.notification.data || {};
  const url = agentId ? `/agents/${agentId}` : "/";
  event.waitUntil(
    clients.matchAll({ type: "window" }).then((windowClients) => {
      for (const client of windowClients) {
        if ("focus" in client) {
          client.navigate(url);
          return client.focus();
        }
      }
      return clients.openWindow(url);
    })
  );
});
