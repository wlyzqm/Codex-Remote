const CACHE_NAME = "codex-remote-shell-v36";
const SHELL_ASSETS = [
  "./",
  "./index.html",
  "./styles.css?v=20260831.22",
  "./markdown.js?v=20260831.2",
  "./app.js?v=20260831.16",
  "./manifest.webmanifest?v=20260831.3",
  "./icon.png?v=20260831.1",
];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE_NAME).then((cache) => cache.addAll(SHELL_ASSETS)));
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((key) => key !== CACHE_NAME).map((key) => caches.delete(key))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  const url = new URL(request.url);

  // Authentication, RPC, status and SSE must always reach the Linux node.
  // They are deliberately excluded from both reads and writes to Cache Storage.
  if (request.method !== "GET" || url.origin !== self.location.origin || url.pathname.startsWith("/api/")) return;

  event.respondWith(
    fetch(request)
      .then((response) => {
        if (response.ok && response.type === "basic") {
          const copy = response.clone();
          caches.open(CACHE_NAME).then((cache) => cache.put(request, copy));
        }
        return response;
      })
      .catch(async () => {
        const cached = await caches.match(request);
        if (cached) return cached;
        if (request.mode === "navigate") return caches.match("./index.html");
        throw new Error("offline");
      }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = event.notification.data?.url || new URL("./", self.location).href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (clients) => {
      const targetUrl = new URL(target, self.location.href);
      const sameOrigin = targetUrl.origin === self.location.origin;
      const existing = clients.find((client) => {
        try { return new URL(client.url).origin === self.location.origin; }
        catch { return false; }
      });
      if (existing) {
        if (sameOrigin && "navigate" in existing) await existing.navigate(targetUrl.href);
        return existing.focus();
      }
      if (sameOrigin && self.clients.openWindow) return self.clients.openWindow(targetUrl.href);
      return undefined;
    }),
  );
});
