"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

function appInternals() {
  const path = require.resolve("./app.js");
  const source = fs.readFileSync(path, "utf8").replace(
    /\n\}\)\(\);\s*$/,
    "\n  globalThis.__appTest = { mergeLiveThreadSnapshot, mergeTimelineItem, incrementalReasoningItem, rateWindowHtml, renderItem, codeLanguage, highlightSource, resolveLocalImage };\n})();",
  );
  const context = { document: { addEventListener() {} }, window: {}, console };
  vm.runInNewContext(source, context, { filename: path });
  return context.__appTest;
}

test("live refresh keeps streaming controls that are absent from a thread snapshot", () => {
  const { mergeLiveThreadSnapshot } = appInternals();
  const liveTurn = {
    id: "turn-1",
    status: "inProgress",
    items: [
      { id: "user-1", type: "userMessage" },
      { id: "reasoning-1", type: "reasoning", summary: ["仍在推理"] },
      { id: "command-1", type: "commandExecution", command: "node --test" },
      { id: "message-1", type: "agentMessage", text: "明面文本" },
      { id: "duplicate-message", type: "agentMessage", text: "明面文本" },
    ],
  };
  const previous = { id: "thread-1", preview: "旧", turns: [liveTurn] };
  liveTurn.items[0].content = [{ type: "text", text: "发送引导" }];
  liveTurn.items[0].remotePending = true;
  const snapshot = { id: "thread-1", preview: "新", turns: [{ ...liveTurn, status: "completed", items: [
    { id: "server-user", type: "userMessage", content: [{ type: "text", text: "发送引导" }] },
    { id: "reasoning-1", type: "reasoning", summary: [], content: [] },
    { id: "server-message", type: "agentMessage", text: "明面文本" },
  ] }] };

  const merged = mergeLiveThreadSnapshot(previous, snapshot);
  assert.equal(merged.preview, "新");
  assert.deepEqual(merged.turns[0].items.map((item) => item.type), ["userMessage", "reasoning", "commandExecution", "agentMessage"]);
  assert.equal(merged.turns[0].items[0].id, "server-user");
  assert.equal(merged.turns[0].items[0].deliveryState, "accepted");
  assert.deepEqual(merged.turns[0].items[1].summary, ["仍在推理"]);
  assert.equal(merged.turns[0].items[3].id, "server-message");
  assert.equal(mergeLiveThreadSnapshot(previous, { id: "thread-1", turns: [] }).turns[0], liveTurn);
});

test("rate windows render remaining instead of used quota", () => {
  const { rateWindowHtml } = appInternals();
  const html = rateWindowHtml("短窗口", { usedPercent: 73 });
  assert.match(html, /剩余 27%/);
  assert.match(html, /class="rate-meter warn"[^>]+value="27"/);
  assert.doesNotMatch(html, /已使用/);
});

test("completed empty reasoning is hidden instead of shown as active", () => {
  const { renderItem } = appInternals();
  assert.equal(renderItem({ id: "reasoning-1", type: "reasoning", status: "completed", summary: [], content: [] }), "");
});

test("reasoning keeps its short summary visible while details stay collapsed", () => {
  const { renderItem } = appInternals();
  const html = renderItem({ id: "reasoning-1", type: "reasoning", status: "completed", summary: ["**检查实时刷新路径**"], content: ["完整推理内容"] });
  assert.match(html, /<details class="item reasoning-item"[^>]*>/);
  assert.match(html, /<strong>检查实时刷新路径<\/strong>/);
  assert.doesNotMatch(html.match(/<summary>(.*?)<\/summary>/s)[1], /\*\*/);
  assert.doesNotMatch(html, /<details[^>]+open/);
});

test("completion snapshots cannot erase a longer live reasoning summary", () => {
  const { mergeTimelineItem } = appInternals();
  const merged = mergeTimelineItem(
    { id: "reasoning-1", type: "reasoning", status: "inProgress", summary: ["已经收到的简述"], content: ["已有正文"] },
    { id: "reasoning-1", type: "reasoning", status: "completed", summary: [], content: [] },
  );
  assert.deepEqual(merged.summary, ["已经收到的简述"]);
  assert.deepEqual(merged.content, ["已有正文"]);
  assert.equal(merged.status, "completed");
});

test("cumulative reasoning items only show their newly added section", () => {
  const { incrementalReasoningItem } = appInternals();
  const items = [
    { summary: ["定位入口"], content: ["检查事件流"] },
    { summary: ["定位入口", "核对状态"], content: ["检查事件流", "比较快照"] },
    { summary: ["定位入口", "核对状态", "修改实现"], content: ["检查事件流", "比较快照", "修复前缀"] },
    { summary: ["定位入口", "核对状态", "修改实现", "完成验证"], content: ["检查事件流", "比较快照", "修复前缀", "运行测试"] },
  ];
  const visible = items.map((item, index) => incrementalReasoningItem(item, items[index - 1]));
  assert.deepEqual(visible.map((item) => item.summary), [["定位入口"], ["核对状态"], ["修改实现"], ["完成验证"]]);
  assert.deepEqual(visible.map((item) => item.content), [["检查事件流"], ["比较快照"], ["修复前缀"], ["运行测试"]]);
});

test("execution output is collapsed behind a concise activity summary", () => {
  const { renderItem } = appInternals();
  const html = renderItem({ id: "command-1", type: "commandExecution", status: "completed", command: "cat README.md", commandActions: [{ type: "read", path: "README.md" }], aggregatedOutput: "contents" });
  assert.match(html, /<details class="item tool-item execution-item command-item"/);
  assert.match(html, /已读取文件并运行命令/);
  assert.match(html, /<pre class="tool-output">contents<\/pre>/);
  assert.doesNotMatch(html, /<details[^>]+open/);
});

test("healthy SSE does not poll the complete selected thread", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  assert.doesNotMatch(source, /setInterval\(refreshSelectedThread/);
  assert.match(source, /source\.onopen[\s\S]*refreshSelectedThread\(true\)/);
});

test("SSE startup closes the first-login backend status race", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const events = source.slice(source.indexOf("function connectEvents"), source.indexOf("function handleSseMessage"));
  const onopen = events.slice(events.indexOf("source.onopen"), events.indexOf("source.onmessage"));
  assert.match(onopen, /refreshStatus\(\)\.catch/);
  assert.doesNotMatch(onopen, /updateConnectivity\(\)/);
  assert.match(source, /"status\.connected"/);
});

test("new thread starts its first turn before reading materialized history", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const createThread = source.slice(source.indexOf("async function createThread"), source.indexOf("function openNewThreadDialog"));
  const turnStart = createThread.indexOf('rpc("turn/start"');
  const selectThread = createThread.indexOf("await selectThread(thread.id)");
  assert.ok(turnStart >= 0 && selectThread >= 0 && turnStart < selectThread);
});

test("automatic system notifications are limited to completed turns", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const serverEvents = source.slice(source.indexOf("function handleEvent"), source.indexOf("function handleNotification"));
  const notifications = source.slice(source.indexOf("function handleNotification"), source.indexOf("function appendReasoningDelta"));
  assert.doesNotMatch(serverEvents, /localNotify\(/);
  assert.equal((notifications.match(/localNotify\(/g) || []).length, 1);
  assert.match(notifications, /method === "turn\/completed"[\s\S]*localNotify\(/);
});

test("read-only code preview escapes source and highlights common tokens", () => {
  const { codeLanguage, highlightSource } = appInternals();
  assert.equal(codeLanguage("src/server.go"), "go");
  const html = highlightSource('const value = "<unsafe>"; // note', "javascript");
  assert.match(html, /token-keyword">const/);
  assert.match(html, /token-string">&quot;&lt;unsafe&gt;&quot;/);
  assert.match(html, /token-comment">\/\/ note/);
  assert.doesNotMatch(html, /<unsafe>/);
});

test("local Markdown images use the protected artifact route outside the thread workspace", () => {
  const { resolveLocalImage } = appInternals();
  assert.equal(resolveLocalImage("/tmp/browser-check.png"), "/api/artifact?path=%2Ftmp%2Fbrowser-check.png");
  assert.equal(resolveLocalImage("/root/.codex/private.png"), "");
  assert.equal(resolveLocalImage("relative.png"), "");
  assert.equal(resolveLocalImage("/tmp/vector.svg"), "");
});

test("conversation images open in an in-app viewer that owns browser back", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  assert.match(source, /function handleTimelineClick\(event\) \{\s*if \(openClickedImage\(event\)\) return;/);
  assert.match(source, /history\.pushState\([^;]+imageViewer: true/);
  assert.match(source, /addEventListener\("popstate"[\s\S]+closeImageViewer\(true\)/);
});

test("Android wrapper notifications bypass web permission and service worker delivery", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const bridge = source.slice(source.indexOf("function nativeNotificationBridge"), source.indexOf("function renderRawStatus"));
  const notify = source.slice(source.indexOf("async function localNotify"), source.indexOf("function loadThemePreference"));
  assert.match(bridge, /__CODEX_REMOTE_NATIVE_NOTIFICATIONS__/);
  assert.match(bridge, /Android[\s\S]*wv/);
  assert.match(notify, /if \(nativeBridge\) new Notification/);
});

test("thread list actions reuse rename and archive operations", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const handler = source.slice(source.indexOf("function handleThreadListClick"), source.indexOf("async function selectThread"));
  assert.match(handler, /openRenameDialog\(threadId\)/);
  assert.match(handler, /toggleThreadArchived\(threadId\)/);
  assert.match(source, /data-thread-list-action="rename"/);
  assert.match(source, /data-thread-list-action="archive"/);
});
