"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

function appInternals(elements = {}, globals = {}, overrides = {}) {
  const path = require.resolve("./app.js");
  const replacements = Object.keys(overrides).map((name) => `${name} = globalThis.__testOverrides.${name};`).join("\n");
  const source = fs.readFileSync(path, "utf8").replace(
    /\n\}\)\(\);\s*$/,
    `\n ${replacements}\n  globalThis.__appTest = { CLIENT_VERSION, renderDiagnostics, defaultWorkspaceDirectory, openNewThreadDialog, loadDirectory, accountStorageKey, accountTierEntries, reasoningEffortEntries, renderTimeline, renderProjectDirectory, state, loadThreads, renderGoalUsage, scheduleStateRefresh, saveComposerDraft, restoreComposerDraft, composerDraft, saveSubmission, readSubmission, sendComposer, deliverComposerSubmission, createThread, HttpError, upsertTurn, appendItemField, mergeLiveThreadSnapshot, mergeTimelineItem, incrementalReasoningItem, rateWindowHtml, renderItem, codeLanguage, highlightSource, resolveLocalImage, visibleChildTurns, composerDeliveryAccepted, setComposerDelivery, renderComposerDelivery, dismissComposerDelivery };\n})();`,
  );
  const context = { document: { addEventListener() {}, querySelector: (selector) => elements[selector] || null, querySelectorAll: () => [] }, window: {}, console, __testOverrides: overrides, ...globals };
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
  assert.deepEqual(Array.from(merged.turns[0].items, (item) => item.type), ["userMessage", "reasoning", "commandExecution", "agentMessage"]);
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

test("partial turn events preserve streamed history and snapshots restore chronological order", () => {
  const { state, upsertTurn, appendItemField, mergeLiveThreadSnapshot } = appInternals({}, {}, { scheduleTimelineRender() {} });
  const items = [
    { id: "user", type: "userMessage", content: [{ type: "text", text: "检查项目" }] },
    { id: "progress", type: "agentMessage", text: "正在检查" },
    { id: "command", type: "commandExecution", command: "pwd" },
    { id: "answer", type: "agentMessage", text: "结论", phase: "final_answer" },
  ];
  state.selectedThread = { id: "thread", turns: [{ id: "turn", status: "inProgress", items }] };
  const snapshot = structuredClone(state.selectedThread);
  // A late turn/start response must not erase items already delivered by SSE.
  upsertTurn({ id: "turn", status: "inProgress", items: [] });
  appendItemField("turn", "answer", "agentMessage", "text", "继续输出");
  state.selectedThread = mergeLiveThreadSnapshot(state.selectedThread, snapshot);
  appendItemField("turn", "answer", "agentMessage", "text", "，完成");
  const answer = { ...items[3], text: "结论继续输出，完成" };
  // The real turn/completed notification contains only the final answer.
  upsertTurn({ id: "turn", status: "completed", items: [answer] });
  assert.equal(state.selectedThread.turns[0].status, "completed");
  assert.deepEqual(Array.from(state.selectedThread.turns[0].items, item => item.id), items.map(item => item.id));
  assert.equal(state.selectedThread.turns[0].items.at(-1).text, answer.text);

  const complete = { id: "thread", turns: [{ id: "turn", status: "completed", items: [...items.slice(0, -1), answer] }] };
  // Recover a page already reduced to the answer, or previously reordered by polling.
  for (const history of [[answer], [answer, ...items.slice(0, -1)]]) {
    const previous = { id: "thread", turns: [{ id: "turn", status: "completed", items: history }] };
    const merged = mergeLiveThreadSnapshot(previous, complete);
    assert.deepEqual(Array.from(merged.turns[0].items, item => item.id), items.map(item => item.id));
  }
  const live = { id: "thread", turns: [{ id: "turn", status: "inProgress", items: items.slice(0, -1) }] };
  const sparse = { id: "thread", turns: [{ id: "turn", status: "completed", items: [items[0], items[1], answer] }] };
  assert.deepEqual(Array.from(mergeLiveThreadSnapshot(live, sparse).turns[0].items, item => item.id), items.map(item => item.id));
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

test("cross-client refresh runs without SSE events and stops when hidden", async () => {
  let callback;
  let reads = 0;
  let schedules = 0;
  const document = { addEventListener() {}, hidden: false };
  const app = appInternals({}, {
    document, navigator: { onLine: true }, clearTimeout() {},
    setTimeout(fn, delay) { callback = fn; schedules++; assert.equal(delay, 3000); return schedules; },
  }, { refreshSelectedThread: async () => { reads++; }, loadThreads: async () => {} });
  app.state.authenticated = true;
  app.state.activeTurnId = "turn-a";
  app.state.sseConnected = true;
  app.scheduleStateRefresh();
  await callback();
  assert.equal(reads, 1);
  document.hidden = true;
  await callback();
  assert.equal(reads, 1);
  assert.equal(schedules, 2);
});

test("SSE startup closes the first-login backend status race", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const events = source.slice(source.indexOf("function connectEvents"), source.indexOf("function handleSseMessage"));
  const onopen = events.slice(events.indexOf("source.onopen"), events.indexOf("source.onmessage"));
  assert.match(onopen, /refreshStatus\(\)\.catch/);
  assert.doesNotMatch(onopen, /updateConnectivity\(\)/);
  assert.match(source, /"status\.connected"/);
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
  assert.equal(resolveLocalImage("/root/.codex/generated_images/thread/image.png"), "/api/artifact?path=%2Froot%2F.codex%2Fgenerated_images%2Fthread%2Fimage.png");
  assert.equal(resolveLocalImage("/root/.codex/private.png"), "");
  assert.equal(resolveLocalImage("relative.png"), "");
  assert.equal(resolveLocalImage("/tmp/vector.svg"), "");
});

test("child timelines hide inherited parent turns without dropping child work", () => {
  const { visibleChildTurns } = appInternals();
  const parent = { turns: [{ id: "parent-turn" }] };
  const child = { turns: [{ id: "parent-turn" }, { id: "child-turn" }] };
  assert.deepEqual(visibleChildTurns(child, parent).map((turn) => turn.id), ["child-turn"]);
  assert.deepEqual(visibleChildTurns(child, null).map((turn) => turn.id), ["parent-turn", "child-turn"]);
});

test("steering delivery is accepted only by the matching thread turn", () => {
  const { composerDeliveryAccepted } = appInternals();
  const pending = { type: "userMessage", content: [{ type: "text", text: "继续检查" }] };
  const delivery = { turnId: "turn-a", pending };
  const accepted = { turns: [{ id: "turn-a", items: [{ id: "server-item", ...pending }] }] };
  const otherTurn = { turns: [{ id: "turn-b", items: [{ id: "server-item", ...pending }] }] };
  assert.equal(composerDeliveryAccepted(delivery, accepted), true);
  assert.equal(composerDeliveryAccepted(delivery, otherTurn), false);
});

test("composer delivery follows its thread and exposes a dismiss button after success", () => {
  const elements = {
    "#composerDelivery": { hidden: true, dataset: {} },
    "#composerDeliveryText": { textContent: "" },
    "#composerDeliveryClose": { hidden: true },
    "#composerDeliveryEdit": { hidden: true },
  };
  const { state, setComposerDelivery, renderComposerDelivery, dismissComposerDelivery } = appInternals(elements);
  state.selectedId = "thread-a";
  setComposerDelivery("accepted", "Codex 已接收引导", "thread-a");
  assert.equal(elements["#composerDelivery"].hidden, false);
  assert.equal(elements["#composerDeliveryClose"].hidden, false);
  state.selectedId = "thread-b";
  renderComposerDelivery();
  assert.equal(elements["#composerDelivery"].hidden, true);
  state.selectedId = "thread-a";
  renderComposerDelivery();
  assert.equal(elements["#composerDeliveryText"].textContent, "Codex 已接收引导");
  dismissComposerDelivery();
  assert.equal(elements["#composerDelivery"].hidden, true);
});

test("generated image details never render the raw base64 result", () => {
  const { renderItem } = appInternals();
  const raw = "iVBORw0KGgo".repeat(1000);
  const html = renderItem({ id: "generated-1", type: "imageGeneration", status: "completed", savedPath: "/root/.codex/generated_images/thread/image.png", revisedPrompt: "仪表盘参考图", result: raw });
  assert.match(html, /生成了图像/);
  assert.match(html, /仪表盘参考图/);
  assert.doesNotMatch(html, /iVBORw0KGgo/);
});

test("conversation images open in an in-app viewer that owns browser back", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  assert.match(source, /function handleTimelineClick\(event\) \{\s*if \(openClickedImage\(event\)\) return;/);
  assert.match(source, /history\.pushState\([^;]+imageViewer: true/);
  assert.match(source, /addEventListener\("popstate"[\s\S]+closeImageViewer\(true\)/);
});


test("thread list actions reuse rename and archive operations", () => {
  const source = fs.readFileSync(require.resolve("./app.js"), "utf8");
  const handler = source.slice(source.indexOf("function handleThreadListClick"), source.indexOf("async function selectThread"));
  assert.match(handler, /openRenameDialog\(threadId\)/);
  assert.match(handler, /toggleThreadArchived\(threadId\)/);
  assert.match(source, /data-thread-list-action="rename"/);
  assert.match(source, /data-thread-list-action="archive"/);
});

function memoryStorage() {
  const values = new Map();
  return { getItem: (key) => values.get(key) || null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) };
}

test("draft text and files stay with their thread and text survives reload", () => {
  const input = { value: "A draft" };
  const storage = memoryStorage();
  const overrides = { clearUploadProgress() {}, renderSelectedFiles() {}, resizeComposer() {} };
  const app = appInternals({ "#composerInput": input }, { localStorage: storage }, overrides);
  app.state.selectedId = "a";
  const file = { name: "a.png" };
  app.state.composerFiles = [file];
  app.saveComposerDraft();
  app.state.selectedId = "b";
  app.restoreComposerDraft("b");
  assert.equal(input.value, "");
  assert.equal(app.state.composerFiles.length, 0);
  input.value = "B draft";
  app.saveComposerDraft();
  app.state.selectedId = "a";
  app.restoreComposerDraft("a");
  assert.equal(input.value, "A draft");
  assert.equal(app.state.composerFiles[0], file);
  const reloaded = appInternals({ "#composerInput": input }, { localStorage: storage }, overrides);
  reloaded.restoreComposerDraft("b");
  assert.equal(input.value, "B draft");
});

test("lost send response keeps its ID and acknowledgment does not clear another draft", async () => {
  const storage = memoryStorage();
  const ids = [];
  const states = [];
  const app = appInternals({}, { localStorage: storage, URL: { revokeObjectURL() {} } }, {
    rpc: async (_method, _params, id) => {
      ids.push(id);
      if (ids.length === 1) throw new Error("offline");
      if (ids.length === 3) throw new app.HttpError("cannot revalidate thread", 403);
      return { turn: { id: "turn-1", status: "inProgress" } };
    },
    setComposerDelivery: (state) => states.push(state),
    updateActiveTurnControls() {}, scheduleTimelineRender() {},
  });
  app.state.selectedId = "b";
  app.composerDraft("a").text = "newer draft";
  app.composerDraft("b").text = "another task";
  const submission = { id: "same-id", method: "turn/start", params: { threadId: "a", input: [] }, text: "submitted text", uploadedPaths: [] };
  app.saveSubmission("a", submission);
  assert.equal(await app.deliverComposerSubmission("a", submission), false);
  assert.equal(app.readSubmission("a").id, "same-id");
  assert.equal(states.at(-1), "unknown");
  assert.equal(await app.deliverComposerSubmission("a", app.readSubmission("a")), true);
  assert.deepEqual(ids, ["same-id", "same-id"]);
  assert.equal(app.readSubmission("a"), null);
  assert.equal(app.composerDraft("a").text, "newer draft");
  assert.equal(app.composerDraft("b").text, "another task");
  app.saveSubmission("a", submission);
  assert.equal(await app.deliverComposerSubmission("a", submission), false);
  assert.equal(app.readSubmission("a").id, "same-id", "a retry's 403 does not prove the first request failed");
});

test("failed first turn opens the same created thread with a recoverable submission", async () => {
  const elements = new Proxy({}, { get: (target, key) => target[key] ||= { value: "", hidden: true, textContent: "", dataset: {} } });
  elements["#newCwd"].value = "/workspace";
  elements["#newPrompt"].value = "first instruction";
  const calls = [];
  let app;
  app = appInternals(elements, { localStorage: memoryStorage(), crypto: require("node:crypto") }, {
    setBusy() {}, renderThreadList() {}, rememberWorkspaceThreads() {}, closeDialog() {}, renderSelectedFiles() {},
    renderNewThreadRecovery() {}, clearUploadProgress() {}, runtimeRangeValue: () => "",
    turnInputs: async () => { calls.push("upload"); return [{ type: "text", text: "first instruction" }]; },
    rpc: async (method) => { calls.push(method); return { thread: { id: "created", cwd: "/workspace" } }; },
    deliverComposerSubmission: async (id) => { calls.push("turn/start"); assert.equal(id, "created"); return false; },
    selectThread: async (id) => { calls.push("thread/read"); assert.equal(id, "created"); },
    loadThreads: async () => {},
  });
  await app.createThread({ preventDefault() {}, submitter: elements["#createThreadSubmit"] });
  assert.deepEqual(calls, ["upload", "thread/start", "turn/start", "thread/read"]);
  assert.equal(app.readSubmission("created").params.threadId, "created");
  assert.equal(app.composerDraft("created").text, "first instruction");
  assert.equal(app.readSubmission("new").thread.id, "created");
});


test("history pagination keeps older pages, removes stale head rows, and resets project filter", async () => {
  const elements = Object.fromEntries(["#loadMoreThreads", "#threadList", "#threadListSummary"].map(key=>[key,{}]));
  const responses = [
    {data:[{id:"a"},{id:"b"}],nextCursor:"page-2"},
    {data:[{id:"b"},{id:"c"}],nextCursor:null},
    {data:[{id:"d"},{id:"a"}],nextCursor:"new-page-2"},
    {data:[{id:"match"}],nextCursor:null},
  ];
  const queries=[];
  const {state,loadThreads}=appInternals(elements,{},{rpc:async(method,params)=>{queries.push(params);return responses.shift();},rememberWorkspaceThreads(){},renderThreadList(){}});
  await loadThreads();await loadThreads(true,true);
  assert.deepEqual(Array.from(state.threads,thread=>thread.id),["a","b","c"]);
  assert.equal(queries[1].cursor,"page-2");
  await loadThreads(false);
  assert.deepEqual(Array.from(state.threads,thread=>thread.id),["d","a","c"]);
  state.selectedProject="/other";await loadThreads();
  assert.equal(queries[3].cwd,"/other");
  assert.deepEqual(Array.from(state.threads,thread=>thread.id),["match"]);
  assert.equal(state.nextCursor,null);
});

test("goal usage uses actual protocol fields and does not invent missing values", () => {
  const output={};const {state,renderGoalUsage}=appInternals({"#goalUsage":output});
  state.selectedGoal={tokensUsed:1200,tokenBudget:2000,timeUsedSeconds:90};renderGoalUsage();
  assert.match(output.textContent,/1,200/);assert.match(output.textContent,/800/);assert.match(output.textContent,/已运行/);
  state.selectedGoal={objective:"test"};renderGoalUsage();assert.equal(output.textContent,"工作站尚未提供目标用量");
});


test("retired notification subscriptions are removed when the service worker activates", async () => {
 const handlers = {}, removed = []; let unsubscribed = false, claimed = false;
 const context = { caches: { keys: async () => [], delete: async name => removed.push(name) }, self: {
   addEventListener: (name, fn) => { handlers[name] = fn; },
   registration: { pushManager: { getSubscription: async () => ({ unsubscribe: async () => { unsubscribed = true; } }) } },
   clients: { claim: async () => { claimed = true; } },
 } };
 vm.runInNewContext(fs.readFileSync(require.resolve("./sw.js"), "utf8"), context);
 let done; handlers.activate({ waitUntil: promise => { done = promise; } }); await done;
 assert.ok(unsubscribed && claimed); assert.ok(removed.includes("codex-remote-notifications"));
 assert.equal(handlers.push, undefined); assert.equal(handlers.notificationclick, undefined);
});

test("timeline keeps reading position after full DOM replacement, including details interaction", () => {
 let top = 700;
 const timeline = { querySelectorAll: () => [], scrollHeight: 1800, clientHeight: 500, get scrollTop() { return top; }, set scrollTop(v) { top = v; }, set innerHTML(_) { top = 0; }, scrollTo({top:v}) { top = v; } };
 const {state,renderTimeline} = appInternals({"#timeline":timeline,"#jumpLatest":{}}, {requestAnimationFrame:fn=>fn()}, { currentAgentRootId:()=>"a",renderTurn:()=>"message" });
 state.selectedId="a";state.selectedThread={turns:[{items:[{}]}]};
 renderTimeline();assert.equal(top,700);
 state.timelineInteractionUntil=Date.now()+10000;top=1300;renderTimeline();assert.equal(top,1300);
 state.timelineInteractionUntil=0;top=1300;renderTimeline();assert.equal(top,1800);
});


test("account storage and runtime choices follow the signed-in account", () => {
  const storage = new Map();
  const { state, accountStorageKey, saveSubmission, readSubmission, accountTierEntries, reasoningEffortEntries } = appInternals({}, { localStorage: { getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) } });
  state.user = { id: "alice", serviceTiers: ["fast"], efforts: ["low"] };
  saveSubmission("new", { id: "alice-draft" });
  assert.match(accountStorageKey("codex-remote:project-directories"), /alice/);
  assert.equal(accountStorageKey("codex-remote:theme"), "codex-remote:theme");
  assert.deepEqual(Array.from(accountTierEntries([{ id: "fast", name: "Fast" }], ""), entry => entry.value), ["fast"]);
  assert.deepEqual(Array.from(reasoningEffortEntries({ supportedReasoningEfforts: ["low"] }), entry => entry.value), ["low"]);
  state.user = { id: "bob" };
  assert.equal(readSubmission("new"), null);
  state.user = { id: "alice" };
  assert.equal(readSubmission("new").id, "alice-draft");
});


test("update notice only appears for a different receiver version", () => {
  const notice = {};
  const { CLIENT_VERSION, state, renderDiagnostics } = appInternals(
    { "#diagnostics": {}, "#clientUpdateNotice": notice },
    { navigator: { onLine: true, userAgent: "test" } },
  );
  const receiverSource = fs.readFileSync(require.resolve("../receive/cmd/codex-remote/main.go"), "utf8");
  assert.equal(CLIENT_VERSION, receiverSource.match(/var version = "([^"]+)"/)[1]);
  for (const [version, hidden] of [[undefined, true], [CLIENT_VERSION, true], ["next-version", false]]) {
    state.backendStatus = { receiverVersion: version };
    renderDiagnostics();
    assert.equal(notice.hidden, hidden);
  }
});


test("new conversations open an allowed project without selecting the virtual root", async () => {
  const storage = memoryStorage();
  const elements = new Proxy({}, { get: (target, key) => target[key] ||= { value: "", hidden: true, textContent: "", dataset: {} } });
  let requested;
  const app = appInternals(elements, { localStorage: storage }, {
    restoreDraftFiles() {}, renderWorkspaceDirectories() {}, renderNewThreadRecovery() {}, showDialog() {},
    request: async url => { requested = new URL(url, "http://test").searchParams.get("path"); return { path: requested, entries: [] }; },
  });
  app.state.workspaceDiscoveryLoaded = true;
  app.state.user = { id: "user-a", workspaces: ["/workspace/a", "/workspace/b"] };
  // Session permissions are available before the status request finishes.
  await app.openNewThreadDialog();
  assert.equal(requested, "/workspace/a");
  assert.equal(elements["#newCwd"].value, "/workspace/a");
  storage.setItem(app.accountStorageKey("codex-remote:last-cwd"), "/workspace/b/child");
  assert.equal(app.defaultWorkspaceDirectory(), "/workspace/b/child");
  app.state.selectedProject = "/workspace/a/project";
  assert.equal(app.defaultWorkspaceDirectory(), "/workspace/a/project");
  app.state.selectedProject = "/workspace/another";
  storage.setItem(app.accountStorageKey("codex-remote:last-cwd"), "/workspace/a-other");
  assert.equal(app.defaultWorkspaceDirectory(), "/workspace/a");
  await app.loadDirectory("/");
  assert.equal(elements["#newCwd"].value, "");
  assert.equal(elements["#directoryCurrentPath"].textContent, "选择工作区");
  app.state.user = { id: "user-b", workspaces: ["/workspace/b"] };
  assert.equal(app.defaultWorkspaceDirectory(), "/workspace/b");
  app.state.user = { id: "admin" };
  app.state.selectedProject = "";
  assert.equal(app.defaultWorkspaceDirectory(), "/");
  app.state.workspaceMode = "restricted"; app.state.workspaceRoots = ["/restricted"];
  assert.equal(app.defaultWorkspaceDirectory(), "/restricted");
});

test("a rejected unsent creation can be corrected while an uncertain creation keeps its ID", async () => {
  for (const code of ["submission_not_sent", "remote_policy_rejected"]) {
    const elements = new Proxy({}, { get: (target, key) => target[key] ||= { value: "", hidden: true, textContent: "", dataset: {} } });
    const app = appInternals(elements, { localStorage: memoryStorage() }, {
      setBusy() {}, clearUploadProgress() {},
      rpc: async () => { throw new app.HttpError("请选择允许的工作区", 403, { error: { code } }); },
    });
    app.saveSubmission("new", { id: "old-create-id", params: { cwd: "/" } });
    await app.createThread({ preventDefault() {} });
    assert.equal(Boolean(app.readSubmission("new")), code !== "submission_not_sent");
    assert.equal(elements["#createThreadSubmit"].textContent, code === "submission_not_sent" ? "创建并发送" : "核对创建");
    assert.equal(elements["#newThreadForm"].noValidate, code !== "submission_not_sent");
  }
});
