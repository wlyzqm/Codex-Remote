const CACHE_NAME = "codex-remote-shell-v51";
const SHELL_ASSETS = [
  "./",
  "./index.html",
  "./styles.css?v=20260914.5",
  "./markdown.js?v=20260913.1",
  "./app.js?v=20260914.5",
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
      .then((keys) => Promise.all(keys.filter((key) => key.startsWith("codex-remote-shell-") && key !== CACHE_NAME).map((key) => caches.delete(key))))
      .then(async () => {
        try { await (await self.registration.pushManager?.getSubscription())?.unsubscribe(); } catch {}
        await caches.delete("codex-remote-notifications");
        await self.clients.claim();
      }),
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
