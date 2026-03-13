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

  if (data.type === "agent_chat_done") {
    title = data.name || "Agent";
    body = data.preview || "Response ready";
    tag = `kojo-agent-${data.agentId}`;
    navData = { agentId: data.agentId };
  }

  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag,
      data: navData,
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
