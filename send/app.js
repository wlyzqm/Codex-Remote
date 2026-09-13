(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
  const API_TIMEOUT_MS = 60000;
  const IMAGE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
  const SUBAGENT_SOURCE_KINDS = ["subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther"];
  const THEME_STORAGE_KEY = "codex-remote:theme";
  const TEXT_SIZE_STORAGE_KEY = "codex-remote:text-size";
  const DRAFT_STORAGE_KEY = "codex-remote:draft:";
  const SUBMISSION_STORAGE_KEY = "codex-remote:submission:";

  const state = {
    authenticated: false,
    threads: [],
    models: [],
    archived: false,
    selectedProject: "",
    selectedId: null,
    selectedThread: null,
    selectedRuntime: null,
    selectedGoal: null,
    activeTurnId: null,
    selectedRefreshBusy: false,
    refreshTimer: null,
    lastListRefresh: 0,
    lastSyncAt: 0,
    syncError: false,
    threadListSequence: 0,
    nextCursor: null,
    loadingMore: false,
    timelineLimit: 80,
    requestHistory: [],
    draftDB: null,
    selectSequence: 0,
    pendingRequests: new Map(),
    composerDeliveries: new Map(),
    composerDrafts: new Map(),
    sendingThreads: new Set(),
    creatingThread: false,
    currentRequestKey: null,
    eventSource: null,
    reconnectTimer: null,
    reconnectAttempt: 0,
    lastEventId: "",
    eventInstanceId: "",
    sseConnected: false,
    backendStatus: null,
    rates: null,
    timelineRenderFrame: null,
    forceScrollAfterRender: false,
    timelineInteractionUntil: 0,
    subagents: new Map(),
    agentRootId: null,
    agentTrail: [],
    subagentPanelOpen: false,
    subagentLoading: false,
    subagentLoadSequence: 0,
    subagentListError: "",
    agentPreviewId: "",
    agentPreviewThread: null,
    agentPreviewSequence: 0,
    agentPreviewTimer: null,
    composerSettingsThreadId: null,
    composerDirty: { model: false, effort: false, serviceTier: false, personality: false },
    serviceWorkerRegistration: null,
    theme: "system",
    textSize: 16,
    workspaceDirectories: [],
    workspaceThreads: new Map(),
    workspaceDiscoveryLoaded: false,
    workspaceDiscoveryRunning: false,
    workspaceMode: "unrestricted",
    workspaceRoots: [],
    directoryPath: "/",
    directoryParent: "",
    composerFiles: [],
    newThreadFiles: [],
    projectFileSelection: new Set(),
    projectFileSource: "",
    projectPreviewSequence: 0,
    projectFilesPath: "",
    projectFilesParent: "",
  };

  class HttpError extends Error {
    constructor(message, status, payload) {
      super(message);
      this.name = "HttpError";
      this.status = status;
      this.payload = payload;
    }
  }

  const textInput = (text) => ({ type: "text", text, text_elements: [] });
  const localImageInput = (path) => ({ type: "localImage", path, detail: "auto" });

  document.addEventListener("DOMContentLoaded", bootstrap);

  async function bootstrap() {
    loadThemePreference();
    loadTextSizePreference();
    loadStoredWorkspaceDirectories();
    bindEvents();
    registerServiceWorker();
    try {
      const session = await request("/api/session");
      if (session && session.authenticated === false) throw new HttpError("登录已失效", 401, session);
      await enterApp();
    } catch (error) {
      showLogin(error.status === 401 ? "" : "暂时无法连接工作站，请检查地址后重试。");
    }
  }

  function bindEvents() {
    $("#loginForm").addEventListener("submit", handleLogin);
    $("#togglePassword").addEventListener("click", togglePasswordVisibility);
    $$('[data-action="new-thread"]').forEach((button) => button.addEventListener("click", openNewThreadDialog));
    $("#newThreadForm").addEventListener("submit", createThread);
    $("#syncWorkspaceDirectories").addEventListener("click", () => syncWorkspaceDirectories(true));
    $("#knownWorkspaceSelect").addEventListener("change", (event) => {
      if (event.target.value) loadDirectory(event.target.value);
    });
    $("#directoryUpButton").addEventListener("click", () => loadDirectory(state.directoryParent || "/"));
    $("#directoryEntries").addEventListener("click", (event) => {
      const button = event.target.closest("[data-directory-path]");
      if (button) loadDirectory(button.dataset.directoryPath);
    });
    $("#projectSelect").addEventListener("change", (event) => {
      state.selectedProject = event.target.value;
      try { localStorage.setItem("codex-remote:selected-project", state.selectedProject); } catch {}
      loadThreads(true).catch(() => {});
    });
    $("#archivedToggle").addEventListener("change", (event) => {
      state.archived = event.target.checked;
      loadThreads(true);
    });
    $("#refreshThreads").addEventListener("click", () => loadThreads(true));
    $("#threadList").addEventListener("click", handleThreadListClick);

    $("#threadMenuButton").addEventListener("click", toggleThreadMenu);
    $("#threadMenu").addEventListener("click", (event) => {
      const button = event.target.closest("[data-thread-action]");
      if (button) handleThreadAction(button.dataset.threadAction);
    });
    document.addEventListener("click", (event) => {
      if (!event.target.closest(".thread-actions")) closeThreadMenu();
      if (!event.target.closest(".composer-settings, .mobile-settings-button")) closeComposerSettings();
    });
    $("#renameForm").addEventListener("submit", renameThread);
    $("#goalButton").addEventListener("click", openGoalDialog);
    $("#goalForm").addEventListener("submit", saveGoal);
    $("#clearGoalButton").addEventListener("click", clearGoal);
    $("#projectFilesBack").addEventListener("click", () => loadProjectDirectory(state.projectFilesParent));
    $("#projectFilesRefresh").addEventListener("click", () => loadProjectDirectory(state.projectFilesPath));
    $("#projectFilesList").addEventListener("click", handleProjectFileClick);
    $("#projectFilesList").addEventListener("change", event => {
      const box = event.target.closest("[data-project-select]");
      if (!box) return;
      if (box.checked) state.projectFileSelection.add(box.dataset.projectSelect);
      else state.projectFileSelection.delete(box.dataset.projectSelect);
      renderFileSelection();
    });
    $("#downloadSelection").addEventListener("click", downloadFileSelection);
    $("#clearFileSelection").addEventListener("click", () => { state.projectFileSelection.clear(); loadProjectDirectory(state.projectFilesPath); });
    $("#projectPreviewMode").addEventListener("change", renderProjectPreview);
    $("#projectWrap").addEventListener("change", () => $("#projectFilePreview").classList.toggle("wrap-lines", $("#projectWrap").checked));
    $("#projectFilePreviewBack").addEventListener("click", closeProjectFilePreview);

    $("#loadMoreThreads").addEventListener("click", () => loadThreads(true, true).catch(() => {}));
    $("#reloadClientButton").addEventListener("click", () => location.reload());
    $("#copyDiagnostics").addEventListener("click", () => copyText(diagnosticText(), "已复制连接诊断"));
    $("#resultsBody").addEventListener("click", event => { const button = event.target.closest("[data-result-path]"); if (button) openProjectPath(button.dataset.resultPath); });
    $("#uploadsButton").addEventListener("click", openUploads);
    $("#requestHistory").addEventListener("click", event => { const button = event.target.closest("[data-history-thread]"); if (button) { closeDialog($("#requestsDialog")); selectThread(button.dataset.historyThread); } });
    $("#uploadsList").addEventListener("click", handleUploadAction);
    $$("[data-cancel-upload]").forEach(button => button.addEventListener("click", () => {
      state[button.dataset.cancelUpload].forEach(file => file.xhr?.abort());
    }));
    $("#composerAttachments").addEventListener("click", previewSelectedFile);
    $("#newThreadAttachments").addEventListener("click", previewSelectedFile);
    $("#composerForm").addEventListener("submit", sendComposer);
    $("#composerInput").addEventListener("input", () => { resizeComposer(); saveComposerDraft(); });
    $("#composerFileInput").addEventListener("change", (event) => addSelectedFiles(event.target, "composerFiles"));
    $("#newThreadFileInput").addEventListener("change", (event) => addSelectedFiles(event.target, "newThreadFiles"));
    $("#composerAttachments").addEventListener("click", (event) => removeSelectedFile(event, "composerFiles"));
    $("#newThreadAttachments").addEventListener("click", (event) => removeSelectedFile(event, "newThreadFiles"));
    $("#composerSettingsButton").addEventListener("click", () => {
      const form = $("#composerForm");
      const open = form.classList.toggle("settings-open");
      $("#composerSettingsButton").setAttribute("aria-expanded", String(open));
    });
    $$('.runtime-range input[type="range"]').forEach((input) => input.addEventListener("input", () => {
      updateRangeOutput(input);
      if (input.id === "composerEffort") state.composerDirty.effort = true;
      if (input.id === "composerServiceTier") state.composerDirty.serviceTier = true;
    }));
    $("#composerInput").addEventListener("keydown", (event) => {
      if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
        event.preventDefault();
        $("#composerForm").requestSubmit();
      }
    });
    $("#timeline").addEventListener("scroll", updateJumpLatest);
    $("#jumpLatest").addEventListener("click", () => scrollTimelineToEnd(true));
    $("#timeline").addEventListener("click", handleTimelineClick);
    $("#timeline").addEventListener("error", handleArtifactError, true);
    $("#imageViewerClose").addEventListener("click", () => closeImageViewer());
    $("#imageViewerDialog").addEventListener("cancel", (event) => {
      event.preventDefault();
      closeImageViewer();
    });
    $("#subagentsToggle").addEventListener("click", toggleSubagentsPanel);
    $("#closeSubagents").addEventListener("click", closeSubagentsPanel);
    $("#refreshSubagents").addEventListener("click", () => loadSubagents(currentAgentRootId(), true));
    $("#subagentTree").addEventListener("click", handleSubagentPanelClick);
    $("#subagentPreviewBack").addEventListener("click", closeAgentPreview);
    $("#openAgentInMain").addEventListener("click", () => {
      const threadId = state.agentPreviewId;
      if (!threadId) return;
      closeAgentPreview(false);
      state.subagentPanelOpen = false;
      navigateToAgentThread(threadId);
    });
    $("#subagentPreviewTimeline").addEventListener("click", handleAgentPreviewClick);
    $("#subagentPreviewTimeline").addEventListener("error", handleArtifactError, true);
    $("#returnToParentButton").addEventListener("click", returnToAgentParent);
    $("#composerDeliveryClose").addEventListener("click", dismissComposerDelivery);
    $("#composerDeliveryEdit").addEventListener("click", releaseUncertainSubmission);
    $("#subagentPromptButton").addEventListener("click", createSubagentPromptTemplate);
    $("#subagentModelPreference").addEventListener("change", persistSubagentPreferences);
    $("#subagentEffortPreference").addEventListener("change", persistSubagentPreferences);
    $("#composerModel").addEventListener("change", () => {
      state.composerDirty.model = true;
      updateComposerCapabilities(false);
    });
    $("#composerPersonality").addEventListener("change", () => { state.composerDirty.personality = true; });
    $("#newModel").addEventListener("change", updateNewThreadCapabilities);
    $("#reviewTargetType").addEventListener("change", renderReviewTargetField);
    $("#reviewForm").addEventListener("submit", startReview);

    $("#requestsButton").addEventListener("click", openRequestsCenter);
    $("#requestsList").addEventListener("click", (event) => {
      const card = event.target.closest("[data-request-key]");
      if (card) openRequest(card.dataset.requestKey);
    });
    $("#requestActions").addEventListener("click", handleRequestAction);
    $("#requestForm").addEventListener("submit", submitStructuredRequest);
    $$('[data-close-request]').forEach((button) => button.addEventListener("click", () => closeDialog($("#requestDialog"))));

    $("#openCodexSettings").addEventListener("click", async () => {
      closeDialog($("#statusDialog"));
      showDialog($("#codexSettingsDialog"));
      selectConfigTab("config");
      $("#codexSettingsMessage").textContent = "正在读取工作站配置…";
      try { await loadCodexSettings(); $("#codexSettingsMessage").textContent = "保存后下次启动 Codex 生效，当前任务继续运行。"; }
      catch (error) { $("#codexSettingsMessage").textContent = readableError(error); }
    });
    $("#readCodexProfile").addEventListener("click", async () => {
      if (state.codexSettingsDirty && !confirm("重新读取将放弃尚未保存的编辑，继续？")) return;
      try { await loadCodexSettings($("#codexProfile").value); } catch (error) { $("#codexSettingsMessage").textContent = readableError(error); }
    });
    for (const [id, action] of [["saveCodexFiles", "save"], ["saveCodexProfile", "profile"], ["applyCodexProfile", "apply"], ["deleteCodexProfile", "delete"], ["reloadCodex", "reload"]]) $("#" + id).addEventListener("click", () => codexSettingsAction(action));
    for (const id of ["codexConfigSource", "codexAuthSource"]) {
      const input = $("#" + id);
      input.addEventListener("input", () => { state.codexSettingsDirty = true; renderConfigEditor(id); });
      input.addEventListener("scroll", () => { const highlight = $("#" + id.replace("Source", "Highlight")); highlight.scrollTop = input.scrollTop; highlight.scrollLeft = input.scrollLeft; });
    }
    $$("[data-config-tab]").forEach(button => {
      button.addEventListener("click", () => selectConfigTab(button.dataset.configTab));
      button.addEventListener("keydown", event => { if (["ArrowLeft", "ArrowRight"].includes(event.key)) { event.preventDefault(); const next = button.dataset.configTab === "config" ? "auth" : "config"; selectConfigTab(next); $(`[data-config-tab="${next}"]`).focus(); } });
    });
    $("#codexSettingsDialog").addEventListener("cancel", event => { event.preventDefault(); closeDialog($("#codexSettingsDialog")); });
    $("#codexSettingsDialog").addEventListener("close", () => {
      state.codexSettingsReadSequence = (state.codexSettingsReadSequence || 0) + 1;
      state.codexSettingsDirty = false;
      $("#codexConfigSource").value = ""; $("#codexAuthSource").value = "";
      $("#codexConfigHighlight").replaceChildren(); $("#codexAuthHighlight").replaceChildren();
      selectConfigTab("config");
      state.codexSettingsRevision = "";
    });
    $("#statusButton").addEventListener("click", openStatusDialog);
    $("#refreshRates").addEventListener("click", refreshRateLimits);
    $("#reconnectButton").addEventListener("click", reconnectNow);
    $("#logoutButton").addEventListener("click", logout);
    $("#themeSelect").addEventListener("change", (event) => setThemePreference(event.target.value));
    $("#bodyTextSize").addEventListener("input", (event) => setTextSizePreference(event.target.value));

    $$('[data-close-dialog]').forEach((button) => {
      button.addEventListener("click", () => closeDialog(button.closest("dialog")));
    });
    $$("dialog").forEach((dialog) => {
      dialog.addEventListener("click", (event) => {
        if (event.target === dialog) dialog.id === "imageViewerDialog" ? closeImageViewer() : closeDialog(dialog);
      });
    });

    $$('[data-route="threads"]').forEach((button) => button.addEventListener("click", showThreadRoute));
    $$('[data-route="requests"]').forEach((button) => button.addEventListener("click", openRequestsCenter));
    $$('[data-route="status"]').forEach((button) => button.addEventListener("click", openStatusDialog));
    $("#mobileBackButton").addEventListener("click", () => {
      if (state.selectedId && state.selectedId !== currentAgentRootId()) returnToAgentParent();
      else showThreadRoute();
    });

    window.addEventListener("online", reconnectNow);
    window.addEventListener("popstate", () => {
      if ($("#imageViewerDialog").open) closeImageViewer(true);
    });
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
      if (state.theme === "system") setThemePreference("system", false);
    });
    window.addEventListener("offline", () => {
      state.sseConnected = false;
      updateConnectivity();
    });
    document.addEventListener("visibilitychange", () => {
      clearTimeout(state.refreshTimer);
      if (document.hidden || !state.authenticated) return;
      if (!state.sseConnected) reconnectNow();
      refreshSelectedThread(true);
      scheduleStateRefresh();
    });
  }

  async function registerServiceWorker() {
    if (!("serviceWorker" in navigator) || !window.isSecureContext) return;
    try {
      state.serviceWorkerRegistration = await navigator.serviceWorker.register("./sw.js", { scope: "./" });
    } catch (error) {
      console.info("Service Worker 未启用", error);
    }
  }

  async function request(path, options = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), options.timeout || API_TIMEOUT_MS);
    const headers = new Headers(options.headers || {});
    if (options.body !== undefined && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");

    try {
      const response = await fetch(path, {
        ...options,
        credentials: "same-origin",
        cache: "no-store",
        headers,
        signal: controller.signal,
      });
      const bodyText = await response.text();
      let payload = null;
      if (bodyText) {
        try { payload = JSON.parse(bodyText); }
        catch { payload = { message: bodyText }; }
      }
      if (!response.ok) {
        const message = payload?.error?.message || payload?.message || payload?.error || `请求失败（${response.status}）`;
        if (response.status === 401 && state.authenticated) showLogin("登录已失效，请重新输入访问密码。");
        throw new HttpError(String(message), response.status, payload);
      }
      return payload;
    } catch (error) {
      if (error.name === "AbortError") throw new Error("请求超时，工作站没有及时响应");
      throw error;
    } finally {
      clearTimeout(timeout);
    }
  }

  async function rpc(method, params = {}, clientRequestId = "") {
    const payload = await request("/api/rpc", {
      method: "POST",
      body: JSON.stringify({ method, params, ...(clientRequestId ? { clientRequestId } : {}) }),
    });
    if (payload?.error) {
      const message = payload.error.message || payload.error.data?.message || payload.error;
      throw new Error(typeof message === "string" ? message : safeStringify(message));
    }
    return payload && Object.prototype.hasOwnProperty.call(payload, "result") ? payload.result : payload;
  }

  async function handleLogin(event) {
    event.preventDefault();
    const passwordInput = $("#passwordInput");
    const password = passwordInput.value;
    const submit = event.submitter || $("#loginForm button[type=submit]");
    if (!password) return;
    setBusy(submit, true, "正在连接…");
    $("#loginError").hidden = true;
    try {
      await request("/api/login", { method: "POST", body: JSON.stringify({ password }) });
      passwordInput.value = "";
      await enterApp();
    } catch (error) {
      $("#loginError").textContent = readableError(error);
      $("#loginError").hidden = false;
    } finally {
      setBusy(submit, false);
    }
  }

  function togglePasswordVisibility() {
    const input = $("#passwordInput");
    const visible = input.type === "text";
    input.type = visible ? "password" : "text";
    $("#togglePassword").textContent = visible ? "显示" : "隐藏";
    $("#togglePassword").setAttribute("aria-label", visible ? "显示密码" : "隐藏密码");
  }

  async function enterApp() {
    state.authenticated = true;
    scheduleStateRefresh();
    $("#bootView").hidden = true;
    $("#loginView").hidden = true;
    $("#appView").hidden = false;
    connectEvents();
    const settled = await Promise.allSettled([
      loadModels(),
      loadThreads(true),
      refreshStatus(),
      loadRequests(),
      refreshRateLimits(),
    ]);
    const failed = settled.filter((entry) => entry.status === "rejected");
    if (failed.length) toast(`有 ${failed.length} 项初始数据暂未加载`, "warning");
    syncWorkspaceDirectories(false).catch(() => {});
    if (readSubmission("new")) toast("有一次任务创建待确认，可从“新建”继续核对", "warning", 7000);
    const linkedThreadId = new URLSearchParams(location.hash.slice(1)).get("thread");
    if (linkedThreadId) selectThread(linkedThreadId, { silent: true });
  }

  function showLogin(message = "") {
    saveComposerDraft();
    state.authenticated = false;
    clearTimeout(state.refreshTimer);
    disconnectEvents();
    state.lastEventId = "";
    state.eventInstanceId = "";
    closeAllDialogs();
    $("#bootView").hidden = true;
    $("#appView").hidden = true;
    $("#loginView").hidden = false;
    $("#loginError").textContent = message;
    $("#loginError").hidden = !message;
    setTimeout(() => $("#passwordInput").focus(), 40);
  }

  async function logout() {
    saveComposerDraft();
    const button = $("#logoutButton");
    setBusy(button, true, "正在退出…");
    try {
      await request("/api/logout", { method: "POST", body: "{}" });
    } catch (error) {
      toast(readableError(error), "warning");
    } finally {
      state.pendingRequests.clear();
      state.composerDeliveries.clear();
      state.selectedThread = null;
      state.selectedId = null;
      setBusy(button, false);
      showLogin("");
    }
  }

  async function loadModels() {
    const result = await rpc("model/list", { limit: 100, includeHidden: false });
    const models = normalizeArray(result, ["data", "models"])
      .filter((model) => model && !model.hidden);
    state.models = models;
    const select = $("#newModel");
    select.innerHTML = models.length
      ? models.map((model) => `<option value="${escapeHtml(model.model || model.id)}"${model.isDefault ? " selected" : ""}>${escapeHtml(model.displayName || model.model || model.id)}</option>`).join("")
      : '<option value="">服务器默认</option>';
    const agentSelect = $("#subagentModelPreference");
    const savedModel = localStorage.getItem("codex-remote:subagent-model") || "";
    agentSelect.innerHTML = '<option value="">继承父会话</option>' + models.map((model) => {
      const value = model.model || model.id;
      return `<option value="${escapeHtml(value)}"${value === savedModel ? " selected" : ""}>${escapeHtml(model.displayName || value)}</option>`;
    }).join("");
    const savedEffort = localStorage.getItem("codex-remote:subagent-effort") || "";
    if ([...$("#subagentEffortPreference").options].some((option) => option.value === savedEffort)) $("#subagentEffortPreference").value = savedEffort;
    const composerModel = $("#composerModel");
    composerModel.innerHTML = '<option value="">继承会话模型</option>' + models.map((model) => `<option value="${escapeHtml(modelValue(model))}">${escapeHtml(model.displayName || modelValue(model))}</option>`).join("");
    updateNewThreadCapabilities();
    syncComposerSettings(state.selectedThread, true);
  }

  function modelValue(model) {
    return model?.model || model?.id || "";
  }

  function serviceTierEntries(model) {
    const raw = model?.serviceTiers || model?.service_tiers || model?.additionalSpeedTiers || [];
    const seen = new Set();
    return (Array.isArray(raw) ? raw : []).map((entry) => {
      if (typeof entry === "string") return { id: entry, name: entry, description: "" };
      return {
        id: entry?.id || entry?.serviceTier || entry?.service_tier || entry?.value || "",
        name: entry?.name || entry?.displayName || entry?.label || "",
        description: entry?.description || "",
      };
    }).filter((entry) => entry.id && !seen.has(entry.id) && seen.add(entry.id));
  }

  function reasoningEffortEntries(model) {
    const efforts = (model?.supportedReasoningEfforts || []).map((entry) => typeof entry === "string"
      ? { value: entry, label: effortLabel(entry) }
      : { value: entry.reasoningEffort, label: effortLabel(entry.reasoningEffort) }).filter((entry) => entry.value);
    return [{ value: "", label: "默认" }, ...efforts];
  }

  function effortLabel(value) {
    return ({ minimal: "最低", low: "低", medium: "中", high: "高", xhigh: "超高", max: "最大", ultra: "极限" })[value] || value || "默认";
  }

  function configureRuntimeRange(input, entries, selected = "") {
    const values = entries.length ? entries : [{ value: "", label: "默认" }];
    input.dataset.entries = JSON.stringify(values);
    input.max = String(values.length - 1);
    const index = values.findIndex((entry) => entry.value === selected);
    input.value = String(index >= 0 ? index : 0);
    input.disabled = values.length === 1;
    updateRangeOutput(input);
  }

  function runtimeRangeEntry(input) {
    try {
      const entries = JSON.parse(input.dataset.entries || "[]");
      return entries[Number(input.value)] || entries[0] || { value: "", label: "默认" };
    } catch {
      return { value: "", label: "默认" };
    }
  }

  function runtimeRangeValue(input) {
    return runtimeRangeEntry(input).value;
  }

  function updateRangeOutput(input) {
    const output = $(`#${input.dataset.output}`);
    if (output) output.textContent = runtimeRangeEntry(input).label;
  }

  function setRuntimeRangeValue(input, value) {
    let entries = [];
    try { entries = JSON.parse(input.dataset.entries || "[]"); } catch {}
    const index = entries.findIndex((entry) => entry.value === value);
    if (index >= 0) input.value = String(index);
    updateRangeOutput(input);
  }

  function currentServiceTier() {
    return state.selectedRuntime?.serviceTier || state.selectedRuntime?.service_tier || state.selectedThread?.serviceTier || state.selectedThread?.service_tier || state.selectedThread?.settings?.serviceTier || "";
  }

  function updateNewThreadCapabilities() {
    const selected = $("#newModel")?.value || "";
    const model = state.models.find((entry) => modelValue(entry) === selected) || state.models.find((entry) => entry.isDefault) || null;
    const tiers = serviceTierEntries(model);
    configureRuntimeRange($("#newEffort"), reasoningEffortEntries(model), model?.defaultReasoningEffort || "");
    configureRuntimeRange($("#newServiceTier"), [{ value: "", label: "默认" }, ...(tiers[0] ? [{ value: tiers[0].id, label: "高" }] : [])]);
    $("#newServiceTierField")?.classList.toggle("field-disabled", tiers.length === 0);
  }

  function selectedComposerModel() {
    const selected = $("#composerModel").value;
    const current = state.selectedRuntime?.model || state.selectedThread?.model || state.selectedThread?.settings?.model || "";
    const value = selected || current;
    return state.models.find((model) => modelValue(model) === value) || state.models.find((model) => model.isDefault) || null;
  }

  function syncComposerSettings(thread, force = false) {
    if (!thread || !state.models.length) return;
    if (!force && state.composerSettingsThreadId === thread.id) return;
    state.composerSettingsThreadId = thread.id;
    const currentModel = state.selectedRuntime?.model || thread.model || thread.settings?.model || "";
    $("#composerModel").value = [...$("#composerModel").options].some((option) => option.value === currentModel) ? currentModel : "";
    updateComposerCapabilities(true);
    const currentEffort = state.selectedRuntime?.reasoningEffort || state.selectedRuntime?.effort || thread.reasoningEffort || thread.settings?.reasoningEffort || "";
    setRuntimeRangeValue($("#composerEffort"), currentEffort);
    const currentTier = currentServiceTier();
    setRuntimeRangeValue($("#composerServiceTier"), currentTier || "__default__");
    const currentPersonality = state.selectedRuntime?.personality || thread.personality || "none";
    if ([...$("#composerPersonality").options].some((option) => option.value === currentPersonality)) $("#composerPersonality").value = currentPersonality;
    state.composerDirty = { model: false, effort: false, serviceTier: false, personality: false };
  }

  function updateComposerCapabilities(resetEffort = false) {
    const model = selectedComposerModel();
    const effortInput = $("#composerEffort");
    const previousEffort = resetEffort ? model?.defaultReasoningEffort || "" : runtimeRangeValue(effortInput);
    configureRuntimeRange(effortInput, reasoningEffortEntries(model), previousEffort);
    const tierInput = $("#composerServiceTier");
    const tiers = serviceTierEntries(model);
    const previousTier = resetEffort ? (currentServiceTier() || "__default__") : runtimeRangeValue(tierInput);
    const activeTier = currentServiceTier();
    const highTier = tiers[0]?.id || "";
    configureRuntimeRange(tierInput, [{ value: "__default__", label: "默认" }, ...(highTier ? [{ value: highTier, label: "高" }] : [])], activeTier === highTier ? highTier : previousTier);
    $("#composerServiceTierField").classList.toggle("field-disabled", !highTier);
    const supportsPersonality = Boolean(model?.supportsPersonality);
    $("#composerPersonalityField").hidden = !supportsPersonality;
    $("#composerPersonality").disabled = !supportsPersonality;
    if (!supportsPersonality) $("#composerPersonality").value = "none";
  }

  async function loadThreads(feedback = true, more = false) {
    const queryKey = JSON.stringify([state.archived, state.selectedProject]);
    if (state.loadingMore && (!feedback || more)) return;
    if (more && queryKey !== state.listQueryKey) more = false;
    if (more && !state.nextCursor) return;
    state.loadingMore = more;
    const sequence = ++state.threadListSequence;
    $("#loadMoreThreads").disabled = true;
    if (feedback && !more) $("#threadList").innerHTML = '<div class="list-skeleton"><i></i><i></i><i></i></div>';
    const params = {
      cursor: more ? state.nextCursor : null,
      limit: 100,
      cwd: state.selectedProject || null,
      sortKey: "updated_at",
      sortDirection: "desc",
      archived: state.archived,
    };
    try {
      const result = await rpc("thread/list", params);
      if (sequence !== state.threadListSequence) return;
      state.lastListRefresh = Date.now();
      const page = normalizeArray(result, ["data", "threads"]);
      const keepTail = (more || !feedback) && state.listQueryKey === queryKey;
      const tail = keepTail ? state.threads.filter(thread => more || !state.headThreadIds?.has(thread.id)) : [];
      state.threads = [...new Map((more ? [...tail, ...page] : [...page, ...tail]).map(thread => [thread.id, thread])).values()];
      if (!keepTail || more) state.nextCursor = result.nextCursor || null;
      if (!more) state.headThreadIds = new Set(page.map(thread => thread.id));
      state.listQueryKey = queryKey;
      $("#loadMoreThreads").hidden = !state.nextCursor;
      $("#threadListSummary").textContent = `已加载 ${state.threads.length} 个任务${state.nextCursor ? " · 还有更早记录" : " · 已全部加载"}`;
      rememberWorkspaceThreads(state.threads);
      renderThreadList();
    } catch (error) {
      if (!feedback || sequence !== state.threadListSequence) throw error;
      if (more) { toast(readableError(error), "error"); throw error; }
      $("#threadList").innerHTML = `<div class="list-message">${escapeHtml(readableError(error))}<br><button class="text-button" type="button" data-retry-threads>重新加载</button></div>`;
      $("[data-retry-threads]")?.addEventListener("click", () => loadThreads());
      throw error;
    } finally {
      if (sequence === state.threadListSequence) { state.loadingMore = false; $("#loadMoreThreads").disabled = false; }
    }
  }

  function renderThreadList() {
    const list = $("#threadList");
    const projects = [...new Set(state.threads.map((thread) => pathText(thread.cwd)).filter(Boolean))];
    const projectSelect = $("#projectSelect");
    projectSelect.innerHTML = '<option value="">全部项目</option>' + [...new Set([...projects, ...state.workspaceDirectories, state.selectedProject].filter(Boolean))].map(path => `<option value="${escapeHtml(path)}"${path === state.selectedProject ? " selected" : ""}>${escapeHtml(path)}</option>`).join("");
    const threads = state.threads.filter(thread => !state.selectedProject || pathText(thread.cwd) === state.selectedProject);
    if (!state.threads.length) {
      list.innerHTML = `<div class="list-message">${state.archived ? "归档区为空" : "还没有会话"}</div>`;
      return;
    }
    if (!threads.length) {
      list.innerHTML = '<div class="list-message">这个项目下没有会话</div>';
      return;
    }
    list.innerHTML = threads.map((thread) => {
      const status = statusType(thread.status);
      const title = threadTitle(thread);
      const preview = thread.preview || "暂无消息摘要";
      return `<div class="thread-card-row" data-thread-row="${escapeHtml(thread.id)}">
        <button class="thread-card${thread.id === state.selectedId ? " active" : ""}" type="button" data-thread-id="${escapeHtml(thread.id)}" data-status="${escapeHtml(status)}">
          <span class="thread-card-top"><i class="status-dot ${escapeHtml(status)}"></i><h3>${escapeHtml(title)}</h3><time>${escapeHtml(relativeTime(thread.recencyAt || thread.updatedAt || thread.createdAt))}</time></span>
          <p>${escapeHtml(preview)}</p>
          <span class="thread-card-meta"><span>${escapeHtml(thread.modelProvider || sourceLabel(thread.source))}</span></span>
        </button>
        <div class="thread-card-actions" aria-label="${escapeHtml(title)}的会话操作">
          <button type="button" data-thread-list-action="rename" aria-label="重命名 ${escapeHtml(title)}"><svg><use href="#i-edit"/></svg><span>重命名</span></button>
          <button class="${state.archived ? "restore" : "archive"}" type="button" data-thread-list-action="archive" aria-label="${state.archived ? "取消归档" : "归档"} ${escapeHtml(title)}"><svg><use href="#i-archive"/></svg><span>${state.archived ? "取消归档" : "归档"}</span></button>
        </div>
      </div>`;
    }).join("");
  }

  function handleThreadListClick(event) {
    const action = event.target.closest("[data-thread-list-action]");
    if (action) {
      const row = action.closest("[data-thread-row]");
      const threadId = row?.dataset.threadRow;
      if (!threadId) return;
      row.scrollLeft = 0;
      if (action.dataset.threadListAction === "rename") openRenameDialog(threadId);
      else if (action.dataset.threadListAction === "archive") toggleThreadArchived(threadId);
      return;
    }
    const card = event.target.closest(".thread-card[data-thread-id]");
    if (card) selectThread(card.dataset.threadId);
  }

  async function selectThread(threadId, options = {}) {
    saveComposerDraft();
    if (!options.preserveAgentNavigation) {
      state.agentRootId = threadId;
      state.agentTrail = [];
      closeAgentPreview(false);
    }
    const sequence = ++state.selectSequence;
    state.selectedId = threadId;
    state.timelineLimit = 80;
    state.projectFileSelection.clear();
    restoreComposerDraft(threadId);
    state.lastSyncAt = 0;
    state.syncError = false;
    state.selectedThread = state.threads.find((thread) => thread.id === threadId) || state.subagents.get(threadId)?.thread || null;
    if (state.selectedThread?.cwd) state.selectedProject = pathText(state.selectedThread.cwd);
    state.selectedRuntime = null;
    state.selectedGoal = null;
    state.activeTurnId = null;
    state.composerSettingsThreadId = null;
    renderThreadList();
    renderThreadDetail(true);
    if (window.matchMedia("(max-width: 720px)").matches) {
      $("#appView").classList.add("mobile-detail");
    }
    try {
      const [threadResult, goalResult] = await Promise.all([
        rpc("thread/read", { threadId, includeTurns: true }),
        rpc("thread/goal/get", { threadId }).catch(() => null),
      ]);
      if (sequence !== state.selectSequence) return;
      state.lastSyncAt = Date.now();
      state.syncError = false;
      updateConnectivity();
      state.selectedThread = mergeLiveThreadSnapshot(state.selectedThread, threadResult?.thread || threadResult);
      if (state.selectedThread?.cwd) state.selectedProject = pathText(state.selectedThread.cwd);
      state.selectedRuntime = state.selectedThread?.runtime || {};
      state.selectedGoal = goalResult?.goal ?? goalResult ?? null;
      state.activeTurnId = findActiveTurn(state.selectedThread)?.id || null;
      syncComposerSettings(state.selectedThread);
      if (threadId === currentAgentRootId()) mergeSelectedIntoList();
      else mergeSubagentThread(state.selectedThread, state.subagents.get(threadId)?.parentId || state.agentTrail.at(-1)?.threadId || currentAgentRootId(), currentAgentRootId());
      reconcileSubagentsFromThread(threadId, state.selectedThread);
      reconcileComposerDelivery(threadId, state.selectedThread);
      renderThreadDetail(false);
      scheduleTimelineRender(true);
      loadSubagents(currentAgentRootId(), false).catch(() => {});
      if (!options.silent) history.replaceState({ threadId }, "", `#thread=${encodeURIComponent(threadId)}`);
    } catch (error) {
      if (sequence !== state.selectSequence) return;
      $("#timeline").innerHTML = `<div class="timeline-empty">${escapeHtml(readableError(error))}</div>`;
      toast(readableError(error), "error");
    }
  }

  async function refreshSelectedThread(force = false) {
    const threadId = state.selectedId;
    const mobileListVisible = window.matchMedia("(max-width: 720px)").matches && !$("#appView").classList.contains("mobile-detail");
    if (!state.authenticated || !threadId || document.hidden || mobileListVisible || state.selectedRefreshBusy) return;
    state.selectedRefreshBusy = true;
    try {
      const result = await rpc("thread/read", { threadId, includeTurns: true });
      if (state.selectedId !== threadId) return;
      const latest = result?.thread || result;
      if (!latest) return;
      state.lastSyncAt = Date.now();
      state.syncError = false;
      updateConnectivity();
      const merged = mergeLiveThreadSnapshot(state.selectedThread, latest);
      if (!force && threadProgressKey(merged) === threadProgressKey(state.selectedThread)) return;
      const previousRuntime = JSON.stringify(state.selectedRuntime || {});
      state.selectedThread = merged;
      state.selectedRuntime = state.selectedThread.runtime || state.selectedRuntime || {};
      state.activeTurnId = findActiveTurn(state.selectedThread)?.id || null;
      if (JSON.stringify(state.selectedRuntime) !== previousRuntime) {
        state.composerSettingsThreadId = null;
        syncComposerSettings(state.selectedThread);
      }
      if (threadId === currentAgentRootId()) mergeSelectedIntoList();
      else mergeSubagentThread(state.selectedThread, state.subagents.get(threadId)?.parentId || currentAgentRootId(), currentAgentRootId());
      reconcileSubagentsFromThread(threadId, state.selectedThread);
      reconcileComposerDelivery(threadId, state.selectedThread);
      renderThreadDetail(false);
      scheduleTimelineRender(false);
    } catch (error) {
      if (state.selectedId === threadId) {
        state.syncError = true;
        updateConnectivity();
      }
      if (force) console.info("会话实时同步暂时失败", error);
    } finally {
      state.selectedRefreshBusy = false;
    }
  }

  function scheduleStateRefresh() {
    clearTimeout(state.refreshTimer);
    if (!state.authenticated || document.hidden) return;
    // ponytail: bounded snapshot polling covers other Codex clients; switch to
    // revision-based reads when the backend exposes a reliable revision.
    state.refreshTimer = setTimeout(async () => {
      if (!state.authenticated || document.hidden || !navigator.onLine) return scheduleStateRefresh();
      await Promise.allSettled([
        refreshSelectedThread(),
        Date.now() - state.lastListRefresh >= 15000 ? loadThreads(false) : Promise.resolve(),
      ]);
      scheduleStateRefresh();
    }, state.activeTurnId ? 3000 : 10000);
  }

  function composerDraft(threadId) {
    if (!state.composerDrafts.has(threadId)) {
      let text = "";
      try { text = localStorage.getItem(DRAFT_STORAGE_KEY + threadId) || ""; } catch {}
      state.composerDrafts.set(threadId, { text, files: [] });
    }
    return state.composerDrafts.get(threadId);
  }

  function persistComposerDraft(threadId) {
    const draft = composerDraft(threadId);
    try {
      if (draft.text) localStorage.setItem(DRAFT_STORAGE_KEY + threadId, draft.text);
      else localStorage.removeItem(DRAFT_STORAGE_KEY + threadId);
    } catch {
      toast("浏览器未能保存草稿，请勿关闭页面", "warning");
    }
  }

  function saveComposerDraft() {
    if (!state.selectedId) return;
    const draft = composerDraft(state.selectedId);
    draft.text = $("#composerInput").value;
    draft.files = state.composerFiles;
    persistComposerDraft(state.selectedId);
  }

  function restoreComposerDraft(threadId) {
    const draft = composerDraft(threadId);
    $("#composerInput").value = draft.text;
    state.composerFiles = draft.files;
    restoreDraftFiles(threadId);
    clearUploadProgress("composerFiles");
    renderSelectedFiles("composerFiles");
    resizeComposer();
    if (readSubmission(threadId)) setComposerDelivery("unknown", "上次发送尚待确认，点击“核对发送”获取结果", threadId);
  }

  function readSubmission(key) {
    try { return JSON.parse(localStorage.getItem(SUBMISSION_STORAGE_KEY + key) || "null"); }
    catch { return null; }
  }

  function saveSubmission(key, value) {
    if (value) localStorage.setItem(SUBMISSION_STORAGE_KEY + key, JSON.stringify(value));
    else localStorage.removeItem(SUBMISSION_STORAGE_KEY + key);
  }

  function submissionIsUncertain(error) {
    // A failed retry's auth/path validation says nothing about the first send.
    return !(error instanceof HttpError) || (error.status !== 422 && error.payload?.error?.code !== "submission_not_sent");
  }

  function renderThreadDetail(loading = false) {
    $("#emptyDetail").hidden = true;
    $("#threadDetail").hidden = false;
    const thread = state.selectedThread || { id: state.selectedId, name: "正在读取会话…", turns: [] };
    const status = statusType(thread.status);
    $("#threadStatusDot").className = `status-dot ${status}`;
    $("#threadStatusLabel").textContent = statusLabel(status, thread.status);
    $("#threadModelLabel").textContent = runtimeModel(thread);
    const selectedAgent = state.subagents.get(state.selectedId);
    const inChild = Boolean(state.selectedId && currentAgentRootId() && state.selectedId !== currentAgentRootId());
    $("#threadTitle").textContent = inChild && selectedAgent ? agentDisplayName(selectedAgent) : threadTitle(thread);
    const cwd = pathText(thread.cwd || state.selectedRuntime?.cwd);
    $("#threadMeta").textContent = [cwd, compactId(thread.id)].filter(Boolean).join("  ·  ");
    $("#archiveActionText").textContent = state.archived ? "取消归档" : "归档会话";
    const canResume = status === "notLoaded" && !state.archived;
    $("#resumeMenuButton").hidden = !canResume;
    $("#archiveMenuButton").hidden = canResume;
    renderAgentContext();
    renderGoalBanner();
    renderSubagentPanel();
    renderComposerDelivery();
    updateActiveTurnControls();
    if (loading) $("#timeline").innerHTML = '<div class="timeline-empty">正在读取完整记录…</div>';
  }

  function renderGoalBanner() {
    const goal = state.selectedGoal;
    const active = Boolean(goal && goal.status !== "complete");
    $("#goalButton").classList.toggle("has-active-goal", active);
    $("#goalButton").title = active ? goal.objective || "进行中的会话目标" : "设置会话目标";
  }

  function currentAgentRootId() {
    return state.agentRootId || state.selectedId || "";
  }

  function toggleSubagentsPanel() {
    state.subagentPanelOpen = !state.subagentPanelOpen;
    if (!state.subagentPanelOpen) closeAgentPreview(false);
    renderSubagentPanel();
  }

  function closeSubagentsPanel() {
    state.subagentPanelOpen = false;
    closeAgentPreview(false);
    renderSubagentPanel();
  }

  function persistSubagentPreferences() {
    localStorage.setItem("codex-remote:subagent-model", $("#subagentModelPreference").value);
    localStorage.setItem("codex-remote:subagent-effort", $("#subagentEffortPreference").value);
  }

  function createSubagentPromptTemplate() {
    const model = $("#subagentModelPreference").value;
    const effort = $("#subagentEffortPreference").value;
    const preferences = [model && `模型使用 ${model}`, effort && `推理强度使用 ${effort}`].filter(Boolean);
    const text = [
      "请创建一个子代理完成以下任务：",
      "- 任务：",
      "- 角色或昵称：",
      "- 期望交付：",
      preferences.length ? `- 运行偏好：${preferences.join("；")}` : "- 运行偏好：继承当前会话",
      "创建后请持续汇报该子代理的状态和最终结果。",
    ].join("\n");
    writeComposerDraft(text, "已生成委派模板，检查后再发送");
  }

  async function queueAgentInstruction(action, agentId) {
    const record = state.subagents.get(agentId);
    if (!record) return;
    const parentId = record.parentId || record.rootId || currentAgentRootId();
    if (parentId && state.selectedId !== parentId) await navigateToAgentThread(parentId);
    const label = agentDisplayName(record);
    const target = `子代理「${label}」（thread ${agentId}）`;
    const instructions = {
      steer: `请向${target}发送以下后续指令：\n`,
      stop: `请停止${target}，并在停止后汇报它已经完成的工作、未完成事项和可复用结果。`,
      close: `请关闭${target}。如果它仍在运行，请先确认当前状态并妥善停止，再汇报关闭结果。`,
    };
    writeComposerDraft(instructions[action] || instructions.steer, "已写入父会话，尚未发送");
  }

  function writeComposerDraft(text, message) {
    const input = $("#composerInput");
    input.value = [input.value.trimEnd(), text].filter(Boolean).join("\n\n");
    saveComposerDraft();
    resizeComposer();
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
    toast(message, "success");
  }

  function handleSubagentPanelClick(event) {
    const action = event.target.closest("[data-agent-action]");
    if (action) {
      queueAgentInstruction(action.dataset.agentAction, action.dataset.agentId);
      return;
    }
    const open = event.target.closest("[data-open-agent-thread]");
    if (open) openAgentPreview(open.dataset.openAgentThread);
  }

  async function openAgentPreview(threadId, quiet = false) {
    if (!threadId) return;
    state.subagentPanelOpen = true;
    state.agentPreviewId = threadId;
    state.agentPreviewThread = state.subagents.get(threadId)?.thread || null;
    const sequence = ++state.agentPreviewSequence;
    renderSubagentPanel();
    try {
      const result = await rpc("thread/read", { threadId, includeTurns: true });
      if (sequence !== state.agentPreviewSequence || threadId !== state.agentPreviewId) return;
      state.agentPreviewThread = result?.thread || result;
      mergeSubagentThread(state.agentPreviewThread, state.subagents.get(threadId)?.parentId || currentAgentRootId(), currentAgentRootId());
      renderSubagentPanel();
    } catch (error) {
      if (sequence !== state.agentPreviewSequence || threadId !== state.agentPreviewId) return;
      state.agentPreviewThread = { id: threadId, turns: [], previewError: readableError(error) };
      renderSubagentPanel();
      if (!quiet) toast(`无法读取子代理：${readableError(error)}`, "warning");
    }
  }

  function closeAgentPreview(render = true) {
    clearTimeout(state.agentPreviewTimer);
    state.agentPreviewTimer = null;
    state.agentPreviewId = "";
    state.agentPreviewThread = null;
    state.agentPreviewSequence += 1;
    if (render) renderSubagentPanel();
  }

  function scheduleAgentPreviewRefresh(threadId) {
    if (!threadId || threadId !== state.agentPreviewId || state.agentPreviewTimer) return;
    state.agentPreviewTimer = setTimeout(() => {
      state.agentPreviewTimer = null;
      if (threadId === state.agentPreviewId) openAgentPreview(threadId, true);
    }, 500);
  }

  function handleAgentPreviewClick(event) {
    if (openClickedImage(event)) return;
    const open = event.target.closest("[data-open-agent-thread]");
    if (open) {
      openAgentPreview(open.dataset.openAgentThread);
      return;
    }
    const path = event.target.closest("[data-copy-path]");
    if (path) {
      copyText(path.dataset.copyPath, "已复制本地路径");
      return;
    }
    const button = event.target.closest("[data-copy-item]");
    if (!button) return;
    const item = findItemInThread(state.agentPreviewThread, button.dataset.copyItem);
    const text = item?.type === "agentMessage" ? item.text : (item?.content || []).filter((part) => part.type === "text").map((part) => part.text).join("\n");
    if (!text) return;
    copyText(text, "已复制到剪贴板");
  }

  async function navigateToAgentThread(threadId) {
    if (!threadId) return;
    const rootId = currentAgentRootId();
    if (threadId === rootId) {
      state.agentTrail = [];
    } else {
      const reverseTrail = [];
      const seen = new Set([threadId]);
      let cursor = state.subagents.get(threadId);
      while (cursor?.parentId && !seen.has(cursor.parentId)) {
        const parentId = cursor.parentId;
        seen.add(parentId);
        reverseTrail.push({ threadId: parentId, name: agentThreadLabel(parentId) });
        if (parentId === rootId) break;
        cursor = state.subagents.get(parentId);
      }
      if (!reverseTrail.some((entry) => entry.threadId === rootId)) reverseTrail.push({ threadId: rootId, name: agentThreadLabel(rootId) });
      state.agentTrail = reverseTrail.reverse();
    }
    await selectThread(threadId, { preserveAgentNavigation: true });
  }

  function returnToAgentParent() {
    const record = state.subagents.get(state.selectedId);
    const parentId = record?.parentId || state.agentTrail.at(-1)?.threadId || currentAgentRootId();
    if (parentId && parentId !== state.selectedId) navigateToAgentThread(parentId);
    else showThreadRoute();
  }

  function renderAgentContext() {
    const bar = $("#agentContextBar");
    const rootId = currentAgentRootId();
    const record = state.subagents.get(state.selectedId);
    const inChild = Boolean(state.selectedId && rootId && state.selectedId !== rootId);
    bar.hidden = !inChild;
    if (!inChild) return;
    const status = agentStatusValue(record?.status || state.selectedThread?.status);
    const parentId = record?.parentId || state.agentTrail.at(-1)?.threadId || rootId;
    $("#agentContextIdentity").innerHTML = `<span class="agent-context-kicker">子代理 · ${escapeHtml(agentStatusLabel(status))}</span><strong>${escapeHtml(record ? agentDisplayName(record) : threadTitle(state.selectedThread))}</strong><small>父会话：${escapeHtml(agentThreadLabel(parentId))}</small>`;
    $("#returnToParentButton").setAttribute("aria-label", `返回父会话 ${agentThreadLabel(parentId)}`);
  }

  async function loadSubagents(rootId, force = false) {
    if (!rootId) return;
    if (state.subagentLoading && state.subagentLoadingRoot === rootId && !force) return;
    const sequence = ++state.subagentLoadSequence;
    state.subagentLoading = true;
    state.subagentLoadingRoot = rootId;
    state.subagentListError = "";
    renderSubagentPanel();
    const base = {
      limit: 100,
      sortKey: "updated_at",
      sortDirection: "desc",
      archived: false,
      sourceKinds: SUBAGENT_SOURCE_KINDS,
    };
    const requests = [
      rpc("thread/list", { ...base, parentThreadId: rootId }),
      rpc("thread/list", { ...base, ancestorThreadId: rootId }),
    ];
    const settled = await Promise.allSettled(requests);
    if (sequence !== state.subagentLoadSequence || rootId !== currentAgentRootId()) return;
    const directIds = new Set(settled[0].status === "fulfilled" ? normalizeArray(settled[0].value, ["data", "threads"]).map((thread) => thread?.id).filter(Boolean) : []);
    let loaded = 0;
    settled.forEach((entry) => {
      if (entry.status !== "fulfilled") return;
      normalizeArray(entry.value, ["data", "threads"]).forEach((thread) => {
        if (!thread?.id || thread.id === rootId) return;
        mergeSubagentThread(thread, threadParentId(thread) || (directIds.has(thread.id) ? rootId : rootId), rootId);
        loaded += 1;
      });
    });
    if (!settled.some((entry) => entry.status === "fulfilled")) {
      state.subagentListError = readableError(settled[0].reason || settled[1].reason);
      if (force) toast(`子代理同步失败：${state.subagentListError}`, "warning");
    } else if (force) {
      toast(`已同步 ${loaded} 条子代理记录`, "success");
    }
    state.subagentLoading = false;
    state.subagentLoadingRoot = "";
    renderSubagentPanel();
  }

  function reconcileSubagentsFromThread(parentThreadId, thread) {
    for (const turn of thread?.turns || []) {
      for (const item of turn?.items || []) if (isSubagentItem(item)) ingestSubagentItem(parentThreadId, item);
    }
  }

  function isSubagentItem(item) {
    return ["collabToolCall", "collabAgentToolCall", "subAgentActivity"].includes(item?.type);
  }

  function ingestSubagentItem(eventThreadId, item) {
    if (!item || typeof item !== "object") return;
    const payload = item.activity && typeof item.activity === "object" ? { ...item, ...item.activity } : item;
    const owner = state.subagents.get(eventThreadId);
    const rootId = owner?.rootId || (eventThreadId === currentAgentRootId() ? eventThreadId : currentAgentRootId() || eventThreadId);
    const senderId = payload.senderThreadId || payload.sender_thread_id || payload.sender?.threadId || eventThreadId;
    const receiverIds = uniqueIds([
      payload.receiverThreadId,
      payload.receiver_thread_id,
      ...(Array.isArray(payload.receiverThreadIds) ? payload.receiverThreadIds : []),
      ...(Array.isArray(payload.receiver_thread_ids) ? payload.receiver_thread_ids : []),
    ]);
    const newChildIds = uniqueIds([
      payload.newThreadId,
      payload.new_thread_id,
      ...(Array.isArray(payload.newThreadIds) ? payload.newThreadIds : []),
      payload.childThreadId,
      payload.agentThreadId,
    ]);
    const agentStates = normalizeAgentStates(payload.agentsStates || payload.agentStates);
    const targetIds = uniqueIds([...newChildIds, ...receiverIds, ...agentStates.map((entry) => entry.threadId)]);
    const common = {
      rootId,
      senderThreadId: senderId,
      receiverThreadIds: receiverIds,
      newThreadId: newChildIds[0] || "",
      tool: payload.tool || payload.action || payload.activityType || payload.kind || "",
      prompt: readableAgentText(payload.prompt),
      message: readableAgentText(payload.message || payload.lastMessage || payload.output) || (payload.type === "subAgentActivity" ? [payload.kind, payload.agentPath].filter(Boolean).join(" · ") : ""),
      model: payload.model || payload.agentModel || payload.config?.model,
      effort: payload.effort || payload.reasoningEffort || payload.reasoning_effort || payload.config?.effort,
      role: payload.role || payload.agentRole || payload.agent_type,
      nickname: payload.nickname || payload.agentName || payload.agent_name || agentPathName(payload.agentPath),
      agentPath: payload.agentPath || payload.agent_path,
      agentStatus: payload.agentStatus,
      agentsStates: payload.agentsStates || payload.agentStates,
      updatedAt: Date.now(),
    };
    if (senderId && senderId !== rootId && state.subagents.has(senderId)) mergeAgentRecord(senderId, { ...common, parentId: state.subagents.get(senderId).parentId, status: payload.senderStatus });
    targetIds.forEach((targetId) => {
      if (!targetId || targetId === rootId || targetId === eventThreadId) return;
      const previous = state.subagents.get(targetId);
      const matchingState = agentStates.find((entry) => entry.threadId === targetId);
      const spawned = newChildIds.includes(targetId);
      const parentId = previous?.parentId || (spawned ? senderId : senderId && senderId !== targetId ? senderId : eventThreadId) || rootId;
      const explicitStatus = matchingState?.status || matchingState?.agentStatus || payload.agentStatus || (payload.type === "subAgentActivity" ? subagentActivityStatus(payload.kind || payload.status) : "");
      mergeAgentRecord(targetId, {
        ...common,
        parentId: parentId === targetId ? rootId : parentId,
        status: explicitStatus || (spawned ? "running" : previous?.status),
        role: matchingState?.role || common.role,
        nickname: matchingState?.nickname || matchingState?.name || common.nickname,
        model: matchingState?.model || common.model,
        effort: matchingState?.effort || matchingState?.reasoningEffort || common.effort,
        message: readableAgentText(matchingState?.message) || common.message,
      });
    });
    renderSubagentPanel();
    renderAgentContext();
  }

  function mergeSubagentThread(thread, fallbackParentId, rootId) {
    if (!thread?.id || thread.id === rootId) return;
    const source = thread.source && typeof thread.source === "object" ? thread.source : {};
    const sourceMeta = subagentSourceMetadata(thread);
    mergeAgentRecord(thread.id, {
      rootId,
      parentId: sourceMeta.parentId || threadParentId(thread) || fallbackParentId || rootId,
      thread,
      name: thread.name,
      nickname: thread.nickname || sourceMeta.nickname || source.nickname || source.agentName,
      role: thread.role || sourceMeta.role || source.role || source.agentRole || source.type,
      agentPath: sourceMeta.agentPath,
      depth: sourceMeta.depth,
      status: thread.status,
      model: thread.model || thread.settings?.model || source.model,
      effort: thread.reasoningEffort || thread.effort || thread.settings?.reasoningEffort || source.reasoningEffort,
      updatedAt: thread.updatedAt || thread.recencyAt || Date.now(),
    });
  }

  function mergeAgentRecord(agentId, data) {
    if (!agentId) return null;
    const previous = state.subagents.get(agentId) || { id: agentId };
    const next = { ...previous, id: agentId };
    Object.entries(data || {}).forEach(([key, value]) => {
      if (key === "status" && isAgentDone(previous.status) && ["idle", "notloaded", "unknown"].includes(agentStatusValue(value).replace(/[\s_-]/g, "").toLowerCase())) return;
      if (value !== undefined && value !== null && value !== "") next[key] = value;
    });
    if (!next.rootId) next.rootId = currentAgentRootId();
    if (!next.parentId || next.parentId === agentId) next.parentId = next.rootId;
    state.subagents.set(agentId, next);
    return next;
  }

  function renderSubagentPanel() {
    const panel = $("#subagentsPanel");
    const toggle = $("#subagentsToggle");
    const rootId = currentAgentRootId();
    const records = [...state.subagents.values()].filter((record) => record.rootId === rootId && record.id !== rootId);
    const active = records.filter((record) => !isAgentDone(record.status));
    const done = records.filter((record) => isAgentDone(record.status));
    $("#subagentActiveCount").textContent = String(active.length);
    $("#subagentDoneCount").textContent = String(done.length);
    $("#subagentsBadge").textContent = String(active.length || records.length);
    $("#subagentsBadge").hidden = records.length === 0;
    toggle.classList.toggle("has-active-agents", active.length > 0);
    toggle.setAttribute("aria-expanded", String(state.subagentPanelOpen));
    toggle.setAttribute("aria-label", `${state.subagentPanelOpen ? "收起" : "展开"}子代理面板，${active.length} 个运行中，${done.length} 个已结束`);
    panel.hidden = !state.subagentPanelOpen;
    panel.classList.toggle("previewing", Boolean(state.agentPreviewId));
    panel.dataset.loading = String(Boolean(state.subagentLoading && state.subagentLoadingRoot === rootId));
    if (!state.subagentPanelOpen) return;
    $("#subagentTree").hidden = Boolean(state.agentPreviewId);
    $(".subagent-command-settings", panel).hidden = Boolean(state.agentPreviewId);
    $("#subagentPreview").hidden = !state.agentPreviewId;
    if (state.agentPreviewId) {
      renderAgentPreview();
      return;
    }
    const tree = $("#subagentTree");
    if (!records.length) {
      const message = state.subagentLoading ? "正在扫描父会话的子代理…" : state.subagentListError ? `无法读取子代理列表：${state.subagentListError}` : "尚未发现子代理。可以先生成委派模板交给当前 Codex。";
      tree.innerHTML = `<div class="subagent-empty">${escapeHtml(message)}</div>`;
      return;
    }
    const compare = (left, right) => agentSortPath(left, rootId).localeCompare(agentSortPath(right, rootId)) || Number(right.updatedAt || 0) - Number(left.updatedAt || 0);
    active.sort(compare);
    done.sort(compare);
    tree.innerHTML = `<div class="subagent-groups">${renderAgentGroup("ACTIVE", active, rootId)}${renderAgentGroup("DONE", done, rootId)}</div>`;
  }

  function renderAgentPreview() {
    const threadId = state.agentPreviewId;
    const record = state.subagents.get(threadId);
    const thread = state.agentPreviewThread;
    $("#subagentPreviewTitle").textContent = record ? agentDisplayName(record) : threadTitle(thread);
    $("#subagentPreviewMeta").textContent = [
      agentStatusLabel(record?.status || thread?.status),
      record?.model || thread?.model,
      record?.effort && `EFFORT ${record.effort}`,
    ].filter(Boolean).join(" · ");
    const timeline = $("#subagentPreviewTimeline");
    if (!thread) {
      timeline.innerHTML = '<div class="timeline-empty">正在读取子代理记录…</div>';
      return;
    }
    if (thread.previewError) {
      timeline.innerHTML = `<div class="timeline-empty">${escapeHtml(thread.previewError)}</div>`;
      return;
    }
    const turns = visibleChildTurns(thread, agentParentThread(threadId));
    timeline.innerHTML = turns.length
      ? `<div class="timeline-inner">${turns.map((turn, index) => renderTurn(turn, index)).join("")}</div>`
      : '<div class="timeline-empty">这个子代理还没有可显示的消息。</div>';
    requestAnimationFrame(() => {
      $$('[data-artifact-image], [data-message-image]', timeline).forEach((image) => {
        if (image.complete && image.naturalWidth === 0) handleArtifactError({ target: image });
      });
      timeline.scrollTop = timeline.scrollHeight;
    });
  }

  function agentParentThread(threadId) {
    const parentId = state.subagents.get(threadId)?.parentId || threadParentId(state.subagents.get(threadId)?.thread);
    return state.threads.find((thread) => thread.id === parentId) || state.subagents.get(parentId)?.thread || (state.selectedId === parentId ? state.selectedThread : null);
  }

  function visibleChildTurns(thread, parentThread) {
    const turns = Array.isArray(thread?.turns) ? thread.turns : [];
    const parentTurnIds = new Set((parentThread?.turns || []).map((turn) => turn?.id).filter(Boolean));
    return parentTurnIds.size ? turns.filter((turn) => !parentTurnIds.has(turn?.id)) : turns;
  }

  function renderAgentGroup(label, records, rootId) {
    const content = records.length ? records.map((record) => renderAgentCard(record, agentDepth(record, rootId))).join("") : '<p class="agent-group-empty">暂无</p>';
    return `<section class="agent-group" aria-label="${label === "ACTIVE" ? "运行中的子代理" : "已结束的子代理"}"><header><span>${label}</span><b>${records.length}</b></header><div role="list">${content}</div></section>`;
  }

  function renderAgentCard(record, depth) {
    const status = agentStatusValue(record.status || record.agentStatus);
    const done = isAgentDone(status);
    const name = agentDisplayName(record);
    const role = record.role || "SUBAGENT";
    const parent = record.parentId && record.parentId !== record.rootId ? `PARENT ${compactId(record.parentId)}` : "ROOT CHILD";
    const metadata = [record.agentPath, record.model, record.effort && `EFFORT ${record.effort}`, compactId(record.id)].filter(Boolean).join(" · ");
    const message = record.message || record.prompt || record.thread?.preview || "暂无活动摘要";
    return `<article class="agent-card ${done ? "done" : "active"} depth-${Math.min(depth, 6)}" role="listitem" data-agent-id="${escapeHtml(record.id)}">
      <button class="agent-open" type="button" data-open-agent-thread="${escapeHtml(record.id)}" aria-label="打开子代理 ${escapeHtml(name)} 的线程详情">
        <span class="agent-node" aria-hidden="true"></span>
        <span class="agent-card-copy"><span class="agent-card-title"><strong>${escapeHtml(name)}</strong><em>${escapeHtml(role)}</em></span><small>${escapeHtml(parent)} · ${escapeHtml(metadata || compactId(record.id))}</small><span class="agent-last-message">${escapeHtml(shorten(message, 180))}</span></span>
        <span class="agent-state ${done ? "done" : "active"}"><i></i>${escapeHtml(agentStatusLabel(status))}</span>
        <svg class="agent-open-arrow"><use href="#i-chevron"/></svg>
      </button>
      <div class="agent-card-actions" aria-label="生成给父 Codex 的指令">
        <button type="button" data-agent-action="steer" data-agent-id="${escapeHtml(record.id)}">引导</button>
        <button type="button" data-agent-action="stop" data-agent-id="${escapeHtml(record.id)}">停止</button>
        <button type="button" data-agent-action="close" data-agent-id="${escapeHtml(record.id)}">关闭</button>
      </div>
    </article>`;
  }

  function agentDepth(record, rootId) {
    let depth = 0;
    let cursor = record;
    const seen = new Set([record.id]);
    while (cursor?.parentId && cursor.parentId !== rootId && !seen.has(cursor.parentId) && depth < 12) {
      seen.add(cursor.parentId);
      depth += 1;
      cursor = state.subagents.get(cursor.parentId);
    }
    return depth;
  }

  function agentSortPath(record, rootId) {
    const parts = [agentDisplayName(record)];
    let cursor = record;
    const seen = new Set([record.id]);
    while (cursor?.parentId && cursor.parentId !== rootId && !seen.has(cursor.parentId)) {
      seen.add(cursor.parentId);
      cursor = state.subagents.get(cursor.parentId);
      if (!cursor) break;
      parts.unshift(agentDisplayName(cursor));
    }
    return parts.join("/");
  }

  function agentDisplayName(record) {
    return record?.nickname || record?.name || record?.thread?.name || agentPathName(record?.agentPath) || (record?.role && record.role !== "subAgent" ? record.role : "") || `AGENT ${compactId(record?.id)}`;
  }

  function agentThreadLabel(threadId) {
    if (threadId === currentAgentRootId()) return threadTitle(state.threads.find((thread) => thread.id === threadId) || (state.selectedId === threadId ? state.selectedThread : null));
    return agentDisplayName(state.subagents.get(threadId));
  }

  function agentStatusValue(status) {
    if (!status) return "unknown";
    if (typeof status === "string") return status;
    return status.type || status.status || status.state || safeStringify(status);
  }

  function agentStatusLabel(status) {
    const value = agentStatusValue(status);
    const key = value.replace(/[\s_-]/g, "").toLowerCase();
    const labels = {
      pending: "等待中", pendinginit: "初始化中", starting: "启动中", started: "运行中", interacted: "运行中", running: "运行中", active: "运行中", inprogress: "运行中",
      waiting: "等待中", idle: "待机", notloaded: "未加载", completed: "已完成", complete: "已完成", done: "已完成",
      failed: "失败", error: "错误", errored: "错误", interrupted: "已中断", stopped: "已停止", cancelled: "已取消",
      canceled: "已取消", closed: "已关闭", shutdown: "已关闭", notfound: "未找到", unknown: "状态未知",
    };
    return labels[key] || value;
  }

  function isAgentDone(status) {
    const value = agentStatusValue(status).replace(/[\s_-]/g, "").toLowerCase();
    // App Server 0.149 emits only started/interacted subAgentActivity items for
    // many completed agents. Their persisted child threads then settle at
    // idle (recent) or notLoaded (historical), so both belong in DONE rather
    // than being shown as indefinitely ACTIVE.
    return ["idle", "notloaded", "completed", "complete", "done", "failed", "error", "errored", "interrupted", "stopped", "cancelled", "canceled", "closed", "shutdown", "notfound"].includes(value);
  }

  function normalizeAgentStates(value) {
    if (!value) return [];
    if (Array.isArray(value)) return value.map((entry) => {
      if (typeof entry === "string") return { threadId: entry };
      return { ...entry, threadId: entry?.threadId || entry?.agentId || entry?.id || "", status: entry?.status || entry?.state };
    }).filter((entry) => entry.threadId);
    if (typeof value !== "object") return [];
    if (value.threadId || value.agentId || value.id) return normalizeAgentStates([value]);
    return Object.entries(value).map(([threadId, entry]) => typeof entry === "object" && entry !== null
      ? { ...entry, threadId: entry.threadId || entry.agentId || threadId, status: entry.status || entry.state || entry.agentStatus }
      : { threadId, status: entry });
  }

  function threadParentId(thread) {
    const source = thread?.source && typeof thread.source === "object" ? thread.source : {};
    const spawn = source.subAgent?.thread_spawn || source.subAgent?.threadSpawn || {};
    return thread?.parentThreadId || thread?.parent_thread_id || thread?.parent?.id || source.parentThreadId || source.parent_thread_id || source.parent?.id || source.subAgent?.parentThreadId || spawn.parent_thread_id || spawn.parentThreadId || "";
  }

  function sourceKind(thread) {
    const source = thread?.source;
    if (typeof source === "string") return source;
    if (!source || typeof source !== "object") return "";
    if (source.subAgent !== undefined) {
      if (typeof source.subAgent === "string") return `subAgent${source.subAgent[0]?.toUpperCase() || ""}${source.subAgent.slice(1)}`;
      if (source.subAgent?.thread_spawn || source.subAgent?.threadSpawn) return "subAgentThreadSpawn";
      if (source.subAgent?.other !== undefined) return "subAgentOther";
      return "subAgent";
    }
    return source.type || source.kind || Object.keys(source).find((key) => key.toLowerCase().startsWith("subagent")) || "";
  }

  function subagentSourceMetadata(thread) {
    const subAgent = thread?.source && typeof thread.source === "object" ? thread.source.subAgent : null;
    if (!subAgent) return {};
    if (typeof subAgent === "string") return { role: subAgent };
    const spawn = subAgent.thread_spawn || subAgent.threadSpawn;
    if (spawn) return {
      parentId: spawn.parent_thread_id || spawn.parentThreadId || "",
      nickname: spawn.agent_nickname || spawn.agentNickname || "",
      role: spawn.agent_role || spawn.agentRole || "",
      agentPath: spawn.agent_path || spawn.agentPath || "",
      depth: spawn.depth,
    };
    if (subAgent.other !== undefined) return { role: String(subAgent.other || "other") };
    return {};
  }

  function subagentActivityStatus(kind) {
    const value = String(kind || "").toLowerCase();
    if (value === "interrupted") return "interrupted";
    if (value === "started" || value === "interacted") return "running";
    return kind || "unknown";
  }

  function agentPathName(value) {
    const path = String(value || "").trim();
    if (!path) return "";
    return path.split(/[\\/]/).filter(Boolean).at(-1) || path;
  }

  function isSubagentThread(thread) {
    return Boolean(threadParentId(thread)) || sourceKind(thread).toLowerCase().startsWith("subagent");
  }

  function uniqueIds(values) {
    return [...new Set(values.flatMap((value) => Array.isArray(value) ? value : [value]).filter((value) => value !== undefined && value !== null && value !== "").map(String))];
  }

  function readableAgentText(value) {
    if (value === undefined || value === null || value === "") return "";
    return typeof value === "string" ? value : safeStringify(value, 2);
  }

  function scheduleTimelineRender(forceBottom = false) {
    state.forceScrollAfterRender ||= forceBottom;
    if (state.timelineRenderFrame) return;
    state.timelineRenderFrame = requestAnimationFrame(() => {
      state.timelineRenderFrame = null;
      renderTimeline(state.forceScrollAfterRender);
      state.forceScrollAfterRender = false;
    });
  }

  function renderTimeline(forceBottom = false) {
    const container = $("#timeline");
    const previousScrollTop = container.scrollTop;
    const openDetails = new Set($$("details.item[open][data-item-id]", container).map((details) => details.dataset.itemId));
    const userInteracting = openDetails.size > 0 || Date.now() < state.timelineInteractionUntil;
    const nearBottom = !userInteracting && (forceBottom || container.scrollHeight - container.scrollTop - container.clientHeight < 130);
    const turns = state.selectedId !== currentAgentRootId()
      ? visibleChildTurns(state.selectedThread, agentParentThread(state.selectedId))
      : Array.isArray(state.selectedThread?.turns) ? state.selectedThread.turns : [];
    if (!turns.length) {
      container.innerHTML = '<div class="timeline-empty">此会话还没有消息。可以从下方发送第一条指令。</div>';
      return;
    }
    const totalItems = turns.reduce((sum, turn) => sum + (turn.items?.length || 0), 0);
    let skip = Math.max(0, totalItems - state.timelineLimit);
    const hiddenItems = skip;
    const visible = turns.map((turn, index) => {
      const count = turn.items?.length || 0;
      if (skip >= count && count) { skip -= count; return ""; }
      const sliced = skip ? { ...turn, items: turn.items.slice(skip) } : turn;
      skip = 0;
      return renderTurn(sliced, index);
    }).join("");
    container.innerHTML = `<div class="timeline-inner">${hiddenItems ? `<button type="button" class="secondary-button load-older" data-load-older>加载更早消息 · 还有 ${hiddenItems} 项</button>` : ""}${visible}</div>`;
    $$("details.item[data-item-id]", container).forEach((details) => { details.open = openDetails.has(details.dataset.itemId); });
    requestAnimationFrame(() => {
      $$('[data-artifact-image], [data-message-image]', container).forEach((image) => {
        if (image.complete && image.naturalWidth === 0) handleArtifactError({ target: image });
      });
    });
    if (nearBottom) scrollTimelineToEnd(false);
    else { container.scrollTop = previousScrollTop; updateJumpLatest(); }
  }

  function renderTurn(turn, index) {
    const status = turn.status || "completed";
    const items = Array.isArray(turn.items) ? turn.items : [];
    const active = status === "inProgress";
    const lastAgentIndex = active ? findLastIndex(items, (item) => item.type === "agentMessage") : -1;
    const activeReasoningIndex = active ? findLastIndex(items, (item, itemIndex) => item.type === "reasoning" && item.status !== "completed" && itemIndex > lastAgentIndex) : -1;
    let previousReasoning = null;
    const rendered = items.map((item, itemIndex) => {
      const visibleItem = item.type === "reasoning" ? incrementalReasoningItem(item, previousReasoning) : item;
      if (item.type === "reasoning") previousReasoning = item;
      return renderItem(visibleItem, active && itemIndex === lastAgentIndex, itemIndex === activeReasoningIndex);
    }).join("");
    const error = turn.error ? `<div class="error-item">${escapeHtml(turn.error.message || safeStringify(turn.error))}</div>` : "";
    return `<section class="turn-block" data-turn-id="${escapeHtml(turn.id || String(index))}">
      <div class="turn-label"><span>第 ${index + 1} 轮</span><span class="turn-state ${escapeHtml(status)}">${escapeHtml(turnStatusLabel(status))}</span>${turn.durationMs ? `<span>${formatDuration(turn.durationMs)}</span>` : ""}</div>
      ${rendered || '<div class="timeline-empty">等待事件…</div>'}${error}
    </section>`;
  }

  function reasoningTextParts(value) {
    return (Array.isArray(value) ? value : [value])
      .flatMap((part) => typeof part === "string" ? [part] : typeof part?.text === "string" ? [part.text] : [])
      .filter(Boolean);
  }

  function incrementalReasoningValue(current, previous) {
    const currentParts = reasoningTextParts(current);
    const previousParts = reasoningTextParts(previous);
    if (!currentParts.length || !previousParts.length) return currentParts;
    if (currentParts.length > previousParts.length && previousParts.every((part, index) => currentParts[index] === part)) {
      return currentParts.slice(previousParts.length);
    }
    const currentText = currentParts.join("\n\n");
    const previousText = previousParts.join("\n\n");
    if (currentText.length > previousText.length && currentText.startsWith(previousText) && /^\s/.test(currentText.slice(previousText.length))) {
      return [currentText.slice(previousText.length).trimStart()];
    }
    return currentParts;
  }

  function incrementalReasoningItem(item, previous) {
    if (!previous) return item;
    return {
      ...item,
      summary: incrementalReasoningValue(item.summary, previous.summary),
      content: incrementalReasoningValue(item.content, previous.content),
    };
  }

  function executionDetails(title, body, { icon = "i-status", status = "", classes = "", id = "" } = {}) {
    const statusKey = String(status || "").replace(/[\s_-]/g, "").toLowerCase();
    const statusChip = status && !["completed", "complete", "success", "succeeded"].includes(statusKey)
      ? `<span class="status-chip ${escapeHtml(status)}">${escapeHtml(commandStatusLabel(status))}</span>` : "";
    return `<details class="item tool-item execution-item${classes ? ` ${escapeHtml(classes)}` : ""}"${id ? ` data-item-id="${escapeHtml(id)}"` : ""}>
      <summary><svg class="execution-icon"><use href="#${escapeHtml(icon)}"/></svg><strong>${escapeHtml(title)}</strong>${statusChip}<svg class="execution-chevron"><use href="#i-chevron"/></svg></summary>
      <div class="execution-body">${body}</div>
    </details>`;
  }

  function renderItem(item, streaming = false, activeReasoning = false) {
    if (!item || typeof item !== "object") return "";
    const id = escapeHtml(item.id || "");
    if (item.type === "userMessage") {
      const content = Array.isArray(item.content) ? item.content : [];
      const text = content.filter((part) => part?.type === "text").map((part) => part.text || "").join("\n");
      return messageHtml("user", "你", text, id, false, content, item.deliveryState || (item.remotePending ? "queued" : ""));
    }
    if (item.type === "agentMessage") return messageHtml("agent", "Codex", item.text || "", id, streaming);
    if (item.type === "reasoning") {
      const summaryParts = reasoningTextParts(item.summary);
      const summary = [...summaryParts, ...reasoningTextParts(item.content)].join("\n\n");
      const inProgress = activeReasoning && item.status !== "completed";
      if (!summary && !inProgress) return "";
      const preview = shorten(firstLine(summaryParts[0] || summary || "正在思考…").replace(/^#{1,6}\s+/, "").replace(/^(\*\*|__)(.+)\1$/, "$2"), 100);
      return `<details class="item reasoning-item" data-item-id="${id}"><summary><span>${inProgress ? "推理中" : "推理过程"}</span><strong>${escapeHtml(preview)}</strong><svg><use href="#i-chevron"/></svg></summary><div class="reasoning-copy markdown-body${inProgress ? " stream-cursor" : ""}">${markdownHtml(summary || "正在思考…")}</div></details>`;
    }
    if (item.type === "plan") {
      return `<article class="item plan-item"><div class="plan-head"><strong>执行计划</strong><span class="status-chip ${streaming ? "inProgress" : ""}">${streaming ? "更新中" : "PLAN"}</span></div><div class="plan-text markdown-body${streaming ? " stream-cursor" : ""}">${markdownHtml(item.text || "")}</div></article>`;
    }
    if (item.type === "commandExecution") return renderCommandItem(item);
    if (item.type === "fileChange") {
      const changes = (item.changes || []).map((change) => `<details class="file-change"><summary><span class="file-change-kind">${escapeHtml(changeKindLabel(change.kind))}</span><button class="file-change-path" type="button" data-project-path="${escapeHtml(change.path || "")}">${escapeHtml(change.path || "未知文件")}</button></summary>${change.diff ? `<pre class="patch-text">${escapeHtml(change.diff)}</pre>` : ""}</details>`).join("");
      return executionDetails(`编辑文件 · ${(item.changes || []).length} 项`, `<div class="file-list">${changes || '<div class="command-text">正在生成补丁…</div>'}</div>`, { icon: "i-edit", status: item.status, id: item.id });
    }
    if (item.type === "mcpToolCall" || item.type === "dynamicToolCall") {
      const name = item.type === "mcpToolCall" ? `${item.server || "MCP"} / ${item.tool || "tool"}` : `${item.namespace ? `${item.namespace} / ` : ""}${item.tool || "tool"}`;
      const args = item.arguments === undefined ? "" : `<pre class="command-text">${escapeHtml(safeStringify(item.arguments, 2))}</pre>`;
      const result = item.result || item.error || item.contentItems;
      const output = result ? `<pre class="tool-output">${escapeHtml(shorten(safeStringify(result, 2), 12000))}</pre>` : "";
      return executionDetails(`调用工具 · ${name}`, `${args}${output}`, { icon: "i-compact", status: item.status || (item.success === true ? "success" : ""), id: item.id });
    }
    if (isSubagentItem(item)) return renderCollabItem(item);
    if (item.type === "turnDiff") return renderTurnDiff(item);
    if (item.type === "webSearch") return compactEventItem("WEB SEARCH", "网页检索", item);
    if (item.type === "imageView" || item.type === "imageGeneration") return renderArtifactItem(item);
    if (item.type === "contextCompaction") return compactEventItem("CONTEXT", "上下文已压缩", item);
    if (item.type === "enteredReviewMode" || item.type === "exitedReviewMode") return compactEventItem("REVIEW", item.review || item.type, item);
    return compactEventItem(String(item.type || "ITEM").toUpperCase(), item.title || item.name || "事件", item);
  }

  function renderCommandItem(item) {
    const meta = [
      item.cwd && `CWD ${pathText(item.cwd)}`,
      item.processId !== undefined && item.processId !== null && `PID ${item.processId}`,
      item.source && `SOURCE ${typeof item.source === "string" ? item.source : safeStringify(item.source)}`,
      item.durationMs !== undefined && item.durationMs !== null && formatDuration(item.durationMs),
    ].filter(Boolean);
    const actions = Array.isArray(item.commandActions) ? item.commandActions : item.commandActions ? [item.commandActions] : [];
    const interactions = Array.isArray(item.terminalInteractions) ? item.terminalInteractions : [];
    const activity = [...new Set(actions.map((action) => ({ read: "读取文件", search: "搜索内容", list: "查看目录", write: "编辑文件", update: "编辑文件", delete: "编辑文件" })[String(action?.type || "").toLowerCase()]).filter(Boolean))];
    const status = String(item.status || "").replace(/[\s_-]/g, "").toLowerCase();
    const title = item.exitCode > 0 || ["failed", "error", "errored"].includes(status) ? "命令执行失败"
      : `${["inprogress", "running"].includes(status) ? "正在" : "已"}${activity.length ? `${activity.join("、")}并` : ""}运行命令`;
    const body = `
      ${meta.length ? `<div class="command-meta">${meta.map((value) => `<span>${escapeHtml(value)}</span>`).join("")}</div>` : ""}
      <pre class="command-text">${escapeHtml(item.command || "")}</pre>
      ${item.aggregatedOutput ? `<pre class="tool-output">${escapeHtml(item.aggregatedOutput)}</pre>` : ""}
      ${actions.length ? `<details class="command-actions"><summary>解析后的动作 · ${actions.length}</summary><pre>${escapeHtml(safeStringify(actions, 2))}</pre></details>` : ""}
      ${interactions.length ? `<div class="terminal-interactions"><span class="terminal-interactions-title">TERMINAL INPUT · ${interactions.length}</span>${interactions.map((entry) => `<div><code>${escapeHtml(entry.stdin || "")}</code><small>${entry.processId !== undefined ? `PID ${escapeHtml(entry.processId)}` : "STDIN"}${entry.receivedAt ? ` · ${escapeHtml(relativeTime(entry.receivedAt))}` : ""}</small></div>`).join("")}</div>` : ""}
    `;
    return executionDetails(title, body, { icon: "i-status", status: ["inprogress", "running"].includes(status) ? "进行中" : item.exitCode > 0 ? `EXIT ${item.exitCode}` : "", classes: "command-item", id: item.id });
  }

  function renderTurnDiff(item) {
    return executionDetails("更新了工作区差异", `<pre class="patch-text">${escapeHtml(item.diff || "暂未产生差异")}</pre>`, { icon: "i-edit", classes: "diff-item", id: item.id });
  }

  function artifactUrl(path) {
    return `/api/artifact?path=${encodeURIComponent(String(path || ""))}`;
  }

  function openClickedImage(event) {
    const image = event.target.closest?.("[data-artifact-image], [data-message-image]");
    if (!image) return false;
    event.preventDefault();
    openImageViewer(image);
    return true;
  }

  function openImageViewer(image) {
    const source = image.currentSrc || image.src;
    if (!source) return;
    const dialog = $("#imageViewerDialog");
    const title = (image.closest("figure") ? $("figcaption span, figcaption", image.closest("figure")) : null)?.textContent?.trim() || image.alt || "图像预览";
    $("#imageViewerImage").src = source;
    $("#imageViewerImage").alt = image.alt || "图像预览";
    $("#imageViewerTitle").textContent = title;
    showDialog(dialog);
    if (!history.state?.imageViewer) history.pushState({ ...(history.state || {}), imageViewer: true }, "", location.href);
  }

  function closeImageViewer(fromHistory = false) {
    const dialog = $("#imageViewerDialog");
    if (!dialog.open) return;
    closeDialog(dialog);
    $("#imageViewerImage").removeAttribute("src");
    if (!fromHistory && history.state?.imageViewer) history.back();
  }

  function renderArtifactItem(item) {
    const path = item.type === "imageGeneration" ? item.savedPath : item.path;
    const generated = item.type === "imageGeneration";
    const details = [item.status, item.revisedPrompt, item.failure].filter(Boolean).map((value) => typeof value === "string" ? value : safeStringify(value));
    const preview = path ? `<figure class="artifact-preview" data-artifact-path="${escapeHtml(path)}">
      <a href="${escapeHtml(artifactUrl(path))}" target="_blank" rel="noopener noreferrer" aria-label="在新标签页打开图像 ${escapeHtml(path)}"><img data-artifact-image src="${escapeHtml(artifactUrl(path))}" alt="${escapeHtml(generated ? item.revisedPrompt || "Codex 生成图像" : `Codex 查看图像 ${path}`)}" loading="lazy" decoding="async"></a>
      <figcaption><span>${escapeHtml(shortPath(path))}</span><small>点击查看原图</small></figcaption>
      <div class="artifact-fallback" hidden><strong>图像无法载入</strong><code>${escapeHtml(path)}</code></div>
    </figure>` : `<div class="artifact-fallback visible"><strong>${generated ? "生成结果尚未保存为本地文件" : "没有可读取的图像路径"}</strong></div>`;
    return executionDetails(generated ? "生成了图像" : "查看了图像", `${preview}${details.length ? `<div class="artifact-notes">${details.map((value) => `<p>${escapeHtml(value)}</p>`).join("")}</div>` : ""}`, { icon: "i-image", classes: "artifact-item", id: item.id });
  }

  function handleArtifactError(event) {
    const image = event.target.closest?.("[data-artifact-image], [data-message-image]");
    if (!image) return;
    const markdownImage = image.closest(".md-image");
    if (markdownImage) {
      image.hidden = true;
      markdownImage.classList.add("failed");
      const label = $("span", markdownImage);
      if (label) label.textContent = `图像无法载入：${image.alt || "未知图像"}`;
      return;
    }
    const messageImage = image.closest(".message-image");
    if (messageImage) {
      image.hidden = true;
      messageImage.classList.add("failed");
      const fallback = $(".message-image-fallback", messageImage);
      if (fallback) fallback.hidden = false;
      return;
    }
    const figure = image.closest(".artifact-preview");
    image.closest("a")?.setAttribute("hidden", "");
    const fallback = $(".artifact-fallback", figure);
    if (fallback) fallback.hidden = false;
    figure?.classList.add("failed");
  }

  function messageHtml(role, label, text, id, streaming, content = [], deliveryState = "") {
    const attachments = role === "user" ? renderMessageImages(content) : "";
    const copy = text ? `<div class="message-text markdown-body${streaming ? " stream-cursor" : ""}">${markdownHtml(text)}</div>` : attachments ? "" : `<div class="message-text markdown-body${streaming ? " stream-cursor" : ""}">${markdownHtml(streaming ? "" : "（空消息）")}</div>`;
    const delivery = ({ sending: "正在上传并提交", queued: "Remote 已接收 · 排队中", accepted: "Codex 已接收" })[deliveryState] || "";
    return `<article class="item message-item ${role === "user" ? "user-message" : "agent-message"}" data-item-id="${id}">
      <div class="message-avatar">${role === "user" ? "YOU" : "CX"}</div>
      <div class="message-body"><div class="item-label"><span>${escapeHtml(label.toUpperCase())}</span>${delivery ? `<span class="message-pending ${escapeHtml(deliveryState)}">${delivery}</span>` : ""}${text ? `<button class="copy-item-button" type="button" data-copy-item="${id}" aria-label="复制文本"><svg><use href="#i-copy"/></svg></button>` : ""}</div>${attachments}${copy}</div>
    </article>`;
  }

  function renderMessageImages(content) {
    const images = (Array.isArray(content) ? content : []).filter((part) => ["image", "localImage", "input_image"].includes(part?.type));
    if (!images.length) return "";
    return `<div class="message-attachments">${images.map((part, index) => {
      const raw = String(part.url || part.image_url || part.path || "");
      const dataImage = raw.length <= 32 * 1024 * 1024 && /^data:image\/(?:png|jpe?g|gif|webp);base64,[a-z0-9+/=\r\n]+$/i.test(raw);
      const localSource = part.type === "localImage" ? resolveLocalImage(raw) : "";
      const source = dataImage ? raw : localSource;
      const label = part.type === "localImage" ? shortPath(raw) : `图像附件 ${index + 1}`;
      if (!source) return `<div class="message-image message-image-unavailable"><strong>图像未自动载入</strong><small>${escapeHtml(raw ? "仅显示会话内嵌图像和当前工作区本地图像" : "图像内容不可用")}</small></div>`;
      return `<figure class="message-image">
        <img data-message-image src="${escapeHtml(source)}" alt="${escapeHtml(label)}" loading="lazy" decoding="async">
        <figcaption>${escapeHtml(label)}</figcaption>
        <div class="message-image-fallback" hidden><strong>图像无法载入</strong><small>${escapeHtml(label)}</small></div>
      </figure>`;
    }).join("")}</div>`;
  }

  function resolveLocalImage(path) {
    const value = String(path || "");
    const generatedImage = value.includes("/.codex/generated_images/");
    const receiverPrivate = (value.includes("/.codex/") && !generatedImage) || value.includes("/.local/state/codex-remote/");
    return !receiverPrivate && /^\/[^?#]+\.(?:png|jpe?g|gif|webp)$/i.test(value) ? artifactUrl(value) : "";
  }

  function markdownHtml(value) {
    const renderer = window.CodexRemoteMarkdown;
    if (!renderer?.render) return `<p>${escapeHtml(value)}</p>`;
    return renderer.render(String(value ?? ""), {
      breaks: true,
      resolveImage(destination) {
        return resolveLocalImage(destination);
      },
    });
  }

  function compactEventItem(kicker, title, item) {
    return executionDetails(title, item.query ? `<pre class="command-text">${escapeHtml(String(item.query))}</pre>` : "", { icon: item.type === "webSearch" ? "i-review" : "i-compact", status: "", id: item.id });
  }

  function renderCollabItem(item) {
    const payload = item.activity && typeof item.activity === "object" ? { ...item, ...item.activity } : item;
    const senderId = payload.senderThreadId || payload.sender_thread_id || payload.sender?.threadId || "";
    const receiverIds = uniqueIds([payload.receiverThreadId, payload.receiver_thread_id, payload.receiverThreadIds, payload.receiver_thread_ids]);
    const childIds = uniqueIds([payload.newThreadId, payload.new_thread_id, payload.newThreadIds, payload.childThreadId, payload.agentThreadId]);
    const model = payload.model || payload.agentModel || payload.config?.model || "";
    const effort = payload.effort || payload.reasoningEffort || payload.reasoning_effort || payload.config?.effort || "";
    const agentStatus = payload.agentStatus || "";
    const states = normalizeAgentStates(payload.agentsStates || payload.agentStates);
    const message = readableAgentText(payload.message || payload.lastMessage);
    const prompt = readableAgentText(payload.prompt);
    const result = readableAgentText(payload.result || payload.output || payload.error);
    const status = agentStatusValue(payload.type === "subAgentActivity" ? subagentActivityStatus(payload.kind) : payload.status || agentStatus || "AGENT");
    const fields = [
      ["SENDER", senderId ? `<code title="${escapeHtml(senderId)}">${escapeHtml(compactId(senderId))}</code>` : "—"],
      ["RECEIVER", renderAgentThreadLinks(receiverIds)],
      ["NEW CHILD", renderAgentThreadLinks(childIds)],
      ["MODEL", escapeHtml(model || "—")],
      ["EFFORT", escapeHtml(effort || "—")],
      ["AGENT STATUS", escapeHtml(agentStatus ? agentStatusLabel(agentStatus) : payload.type === "subAgentActivity" ? agentStatusLabel(subagentActivityStatus(payload.kind)) : "—")],
      ["KIND / TOOL", escapeHtml(payload.kind || payload.tool || payload.action || "—")],
      ["AGENT PATH", escapeHtml(payload.agentPath || payload.agent_path || "—")],
    ];
    const stateRows = states.length ? `<div class="agent-event-states"><span>AGENTS STATES</span>${states.map((entry) => `<button type="button" data-open-agent-thread="${escapeHtml(entry.threadId)}" title="打开 ${escapeHtml(entry.threadId)}"><code>${escapeHtml(compactId(entry.threadId))}</code><b>${escapeHtml(agentStatusLabel(entry.status || entry.agentStatus))}</b>${entry.nickname || entry.name || entry.role ? `<em>${escapeHtml(entry.nickname || entry.name || entry.role)}</em>` : ""}${entry.message ? `<small class="agent-state-message">${escapeHtml(entry.message)}</small>` : ""}</button>`).join("")}</div>` : '<div class="agent-event-states empty"><span>AGENTS STATES</span><small>—</small></div>';
    const body = `
      <dl class="agent-event-grid">${fields.map(([label, value]) => `<div><dt>${label}</dt><dd>${value}</dd></div>`).join("")}</dl>
      ${stateRows}
      ${prompt ? `<div class="agent-event-copy"><span>PROMPT</span><pre>${escapeHtml(prompt)}</pre></div>` : ""}
      ${message ? `<div class="agent-event-copy"><span>MESSAGE</span><pre>${escapeHtml(message)}</pre></div>` : ""}
      ${result ? `<div class="agent-event-copy result"><span>RESULT</span><pre>${escapeHtml(result)}</pre></div>` : ""}
    `;
    return executionDetails(`协作代理 · ${payload.tool || payload.action || payload.activityType || payload.kind || "activity"}`, body, { icon: "i-agents", status: isAgentDone(status) ? "" : agentStatusLabel(status), classes: "agent-event-item", id: item.id });
  }

  function renderAgentThreadLinks(ids) {
    if (!ids.length) return "—";
    return ids.map((threadId) => `<button class="agent-thread-link" type="button" data-open-agent-thread="${escapeHtml(threadId)}" title="打开子代理线程 ${escapeHtml(threadId)}">${escapeHtml(compactId(threadId))}<svg><use href="#i-chevron"/></svg></button>`).join("");
  }

  function handleTimelineClick(event) {
    if (openClickedImage(event)) return;
    if (event.target.closest("[data-load-older]")) {
      const container = $("#timeline"), height = container.scrollHeight, top = container.scrollTop;
      state.timelineLimit += 80;
      state.timelineInteractionUntil = Date.now() + 1000;
      renderTimeline(false);
      requestAnimationFrame(() => { container.scrollTop = top + container.scrollHeight - height; });
      return;
    }
    if (event.target.closest("details > summary")) state.timelineInteractionUntil = Date.now() + 5000;
    const open = event.target.closest("[data-open-agent-thread]");
    if (open) {
      navigateToAgentThread(open.dataset.openAgentThread);
      return;
    }
    const path = event.target.closest("[data-copy-path]");
    if (path) {
      if (!openProjectPath(path.dataset.copyPath)) copyText(path.dataset.copyPath, "已复制本地路径");
      return;
    }
    const projectPath = event.target.closest("[data-project-path]");
    if (projectPath) {
      openProjectPath(projectPath.dataset.projectPath);
      return;
    }
    const button = event.target.closest("[data-copy-item]");
    if (!button) return;
    const item = findItem(button.dataset.copyItem);
    const text = item?.type === "agentMessage" ? item.text : (item?.content || []).filter((part) => part.type === "text").map((part) => part.text).join("\n");
    if (!text) return;
    copyText(text, "已复制到剪贴板");
  }

  function copyText(text, message) {
    navigator.clipboard?.writeText(String(text || "")).then(() => toast(message, "success")).catch(() => toast("复制失败", "error"));
  }

  function scrollTimelineToEnd(smooth = false) {
    const timeline = $("#timeline");
    timeline.scrollTo({ top: timeline.scrollHeight, behavior: smooth ? "smooth" : "auto" });
    $("#jumpLatest").hidden = true;
  }

  function updateJumpLatest() {
    const timeline = $("#timeline");
    $("#jumpLatest").hidden = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 180;
  }

  function updateActiveTurnControls() {
    const active = Boolean(state.activeTurnId);
    const sending = state.sendingThreads.has(state.selectedId);
    const pending = readSubmission(state.selectedId);
    $("#interruptMenuButton").hidden = !active;
    $("#sendButton").disabled = sending;
    $("#sendButton").setAttribute("aria-busy", String(sending));
    $("#sendLabel").textContent = sending ? "发送中…" : pending ? "核对发送" : active ? "发送引导" : "发送";
    $("#composerHint").textContent = pending ? "核对上次提交，当前草稿不会被重新发送" : state.composerFiles.length ? "草稿与附件按任务保存" : active ? "追加指令给当前任务 · Ctrl + Enter 发送" : "草稿按任务保存 · Ctrl + Enter 发送";
    $("#composerModel").disabled = active;
    $("#composerEffort").disabled = active;
    $("#composerServiceTier").disabled = active || $("#composerServiceTierField").classList.contains("field-disabled");
    $("#composerPersonality").disabled = active || $("#composerPersonalityField").hidden;
    $(".composer-settings").classList.toggle("locked", active);
    $(".composer-settings").setAttribute("aria-label", active ? "运行中不可修改下一轮设置" : "下一轮运行设置");
    $("#composerSettingsButton").disabled = active;
    if (active) closeComposerSettings();
  }

  function setComposerDelivery(deliveryState, message, threadId = state.selectedId, details = {}) {
    if (!threadId) return;
    if (message) state.composerDeliveries.set(threadId, { ...(state.composerDeliveries.get(threadId) || {}), ...details, state: deliveryState || "", message });
    else state.composerDeliveries.delete(threadId);
    if (threadId === state.selectedId) renderComposerDelivery();
  }

  function renderComposerDelivery() {
    const container = $("#composerDelivery");
    if (!container) return;
    const delivery = state.composerDeliveries.get(state.selectedId);
    container.hidden = !delivery?.message;
    container.dataset.state = delivery?.state || "";
    $("#composerDeliveryText").textContent = delivery?.message || "";
    $("#composerDeliveryClose").hidden = !["accepted", "error"].includes(delivery?.state);
    $("#composerDeliveryEdit").hidden = delivery?.state !== "unknown";
  }

  function dismissComposerDelivery() {
    if (state.selectedId) state.composerDeliveries.delete(state.selectedId);
    renderComposerDelivery();
  }

  function releaseUncertainSubmission() {
    if (!state.selectedId || state.sendingThreads.has(state.selectedId)) return;
    if (!window.confirm("请先检查会话中是否已有这条指令。结束核对只恢复编辑，不会重发；之后再次发送可能重复执行。")) return;
    saveSubmission(state.selectedId, null);
    dismissComposerDelivery();
    updateActiveTurnControls();
  }

  function reconcileComposerDelivery(threadId, thread) {
    const delivery = state.composerDeliveries.get(threadId);
    if (!delivery?.pending || delivery.state === "accepted") return;
    if (composerDeliveryAccepted(delivery, thread)) setComposerDelivery("accepted", "Codex 已接收引导，正在处理", threadId);
  }

  function composerDeliveryAccepted(delivery, thread) {
    const turns = delivery?.turnId ? (thread?.turns || []).filter((turn) => turn.id === delivery.turnId) : thread?.turns || [];
    return Boolean(delivery?.pending) && turns.some((turn) => (turn.items || []).some((item) => item.type === "userMessage" && !item.remotePending && sameUserMessage(delivery.pending, item)));
  }

  async function sendComposer(event) {
    event.preventDefault();
    const threadId = state.selectedId;
    if (!threadId || state.sendingThreads.has(threadId)) return;
    saveComposerDraft();
    const draft = composerDraft(threadId);
    let submission = readSubmission(threadId);
    if (!submission && !draft.text.trim() && !draft.files.length) return;
    state.sendingThreads.add(threadId);
    updateActiveTurnControls();
    try {
      if (!submission) {
        const text = draft.text;
        const files = [...draft.files];
        const turnId = state.activeTurnId;
        const settings = {
          dirty: { ...state.composerDirty }, model: $("#composerModel").value,
          effort: runtimeRangeValue($("#composerEffort")), serviceTier: runtimeRangeValue($("#composerServiceTier")),
          personality: !$("#composerPersonalityField").hidden ? $("#composerPersonality").value : "",
        };
        setComposerDelivery("sending", files.length ? "正在上传附件…" : "正在发送…", threadId);
        const inputs = await turnInputs(text.trim(), files, (loaded, total, name) => {
          if (state.selectedId === threadId) renderUploadProgress("composerFiles", loaded, total, name);
        }, threadId);
        const params = { threadId, input: inputs };
        if (turnId) params.expectedTurnId = turnId;
        else {
          if (settings.dirty.model && settings.model) params.model = settings.model;
          if (settings.dirty.effort) params.effort = settings.effort || null;
          if (settings.dirty.serviceTier) params.serviceTier = settings.serviceTier === "__default__" ? null : settings.serviceTier;
          if (settings.dirty.personality && settings.personality) params.personality = settings.personality;
        }
        submission = {
          id: crypto.randomUUID(), method: turnId ? "turn/steer" : "turn/start", params, text,
          uploadedPaths: files.map((file) => file.uploaded?.path).filter(Boolean),
        };
        saveSubmission(threadId, submission);
      }
      await deliverComposerSubmission(threadId, submission);
    } catch (error) {
      setComposerDelivery("error", `尚未发送：${readableError(error)}`, threadId);
      toast(readableError(error), "error");
    } finally {
      state.sendingThreads.delete(threadId);
      if (state.selectedId === threadId) clearUploadProgress("composerFiles");
      updateActiveTurnControls();
    }
  }

  async function deliverComposerSubmission(threadId, submission) {
    const { params, id, method } = submission;
    const steering = method === "turn/steer";
    const targetThread = state.selectedId === threadId ? state.selectedThread : null;
    const turnId = params.expectedTurnId;
    const targetTurn = targetThread?.turns?.find((turn) => turn.id === turnId);
    const pendingId = `remote-${id}`;
    const pending = { id: pendingId, type: "userMessage", content: params.input, remotePending: true, deliveryState: "sending" };
    if (steering && targetTurn && !findItemInThread(targetThread, pendingId)) {
      targetTurn.items ||= [];
      targetTurn.items.push(pending);
    }
    setComposerDelivery("sending", "正在核对工作站接收结果…", threadId, { pending: null, turnId: null });
    if (state.selectedId === threadId) scheduleTimelineRender(true);
    try {
      const result = await rpc(method, params, id);
      if (steering) {
        setComposerDelivery("queued", "工作站已接收引导，等待 Codex 处理", threadId, { pending, pendingId, turnId });
        const queued = findItemInThread(targetThread, pendingId);
        if (queued) queued.deliveryState = "queued";
      } else {
        const turn = result?.turn || result;
        if (!turn?.id) throw new Error("工作站没有返回轮次标识，请核对发送结果");
        if (state.selectedId === threadId) {
          upsertTurn(turn);
          state.activeTurnId = turn.status === "inProgress" ? turn.id : null;
          state.composerDirty = { model: false, effort: false, serviceTier: false, personality: false };
        }
        setComposerDelivery("accepted", "Codex 已接收消息", threadId);
      }
      saveSubmission(threadId, null);
      if (readSubmission("new")?.thread?.id === threadId) saveSubmission("new", null);
      const draft = composerDraft(threadId);
      if (draft.text === submission.text) draft.text = "";
      const sentFiles = draft.files.filter((file) => submission.uploadedPaths.includes(file.uploaded?.path));
      draft.files = draft.files.filter((file) => !sentFiles.includes(file));
      draft.restored = true;
      persistDraftFiles(threadId);
      releaseSelectedFiles(sentFiles);
      persistComposerDraft(threadId);
      if (state.selectedId === threadId) {
        restoreComposerDraft(threadId);
        closeComposerSettings();
        updateActiveTurnControls();
        scheduleTimelineRender(true);
        await refreshSelectedThread(true);
      }
      return true;
    } catch (error) {
      const uncertain = submissionIsUncertain(error);
      if (!uncertain) {
        saveSubmission(threadId, null);
        const creation = readSubmission("new");
        if (creation?.thread?.id === threadId) {
          creation.turnRequestId = crypto.randomUUID();
          saveSubmission("new", creation);
        }
        if (targetTurn) targetTurn.items = (targetTurn.items || []).filter((item) => item.id !== pendingId);
      }
      setComposerDelivery(uncertain ? "unknown" : "error", uncertain
        ? `发送结果待确认：${readableError(error)}。点击“核对发送”，不会重复执行。`
        : `发送被拒绝：${readableError(error)}，草稿已保留`, threadId);
      if (state.selectedId === threadId) { updateActiveTurnControls(); scheduleTimelineRender(false); }
      return false;
    }
  }

  async function interruptTurn() {
    if (!state.selectedId || !state.activeTurnId) return;
    const turnId = state.activeTurnId;
    const button = $("#interruptMenuButton");
    setBusy(button, true, "中断中…");
    try {
      await rpc("turn/interrupt", { threadId: state.selectedId, turnId });
      toast("已发送中断请求", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  function resizeComposer() {
    const textarea = $("#composerInput");
    textarea.rows = Math.min(7, Math.max(2, textarea.value.split("\n").length));
  }

  async function turnInputs(text, attachments, onProgress = () => {}, draftKey = "") {
    const total = attachments.reduce((sum, attachment) => sum + attachment.size, 0);
    const uploaded = [];
    let completed = 0;
    for (const attachment of attachments) {
      const result = attachment.uploaded || await uploadAttachment(attachment, (loaded) => onProgress(completed + loaded, total, attachment.name));
      attachment.uploaded = result;
      if (draftKey) persistDraftFiles(draftKey);
      uploaded.push({ ...result, type: attachment.type });
      completed += attachment.size;
      onProgress(completed, total, attachment.name);
    }
    const files = uploaded.filter((file) => !IMAGE_TYPES.has(file.type));
    const images = uploaded.filter((file) => IMAGE_TYPES.has(file.type));
    const fileNote = files.length ? `[用户上传文件]\n${files.map((file) => `- ${file.name}（${formatBytes(file.size)}）：${file.path}`).join("\n")}` : "";
    return [textInput([text, fileNote].filter(Boolean).join("\n\n")), ...images.map((file) => localImageInput(file.path))].filter((value) => value.text || value.path);
  }

  function closeComposerSettings() {
    $("#composerForm")?.classList.remove("settings-open");
    $("#composerSettingsButton")?.setAttribute("aria-expanded", "false");
  }

  async function addSelectedFiles(input, stateKey) {
    const files = [...input.files];
    input.value = "";
    try {
      for (const file of files) {
        state[stateKey].push({ name: file.name, size: file.size, type: file.type, file, previewUrl: IMAGE_TYPES.has(file.type) ? URL.createObjectURL(file) : "" });
      }
      renderSelectedFiles(stateKey);
      if (stateKey === "composerFiles") { saveComposerDraft(); persistDraftFiles(state.selectedId); updateActiveTurnControls(); }
      else persistDraftFiles("new");
    } catch (error) {
      renderSelectedFiles(stateKey);
      toast(readableError(error), "error");
    }
  }

  function uploadAttachment(attachment, onProgress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/uploads?name=${encodeURIComponent(attachment.name)}`);
      attachment.xhr = xhr;
      xhr.timeout = 30 * 60 * 1000;
      xhr.onabort = () => reject(new Error("已取消上传，附件已保留，可再次发送"));
      xhr.ontimeout = () => reject(new Error("上传超时，附件已保留"));
      xhr.onloadend = () => { delete attachment.xhr; };
      xhr.responseType = "json";
      xhr.setRequestHeader("Content-Type", attachment.type || "application/octet-stream");
      xhr.upload.onprogress = (event) => onProgress(event.loaded);
      xhr.onload = () => {
        const payload = xhr.response || {};
        if (xhr.status >= 200 && xhr.status < 300 && payload.path) return resolve(payload);
        if (xhr.status === 401 && state.authenticated) showLogin("登录已失效，请重新输入访问密码。");
        reject(new HttpError(String(payload?.error?.message || payload?.message || `上传失败（${xhr.status}）`), xhr.status, payload));
      };
      xhr.onerror = () => reject(new Error(`无法上传 ${attachment.name}`));
      xhr.send(attachment.file);
    });
  }

  function removeSelectedFile(event, stateKey) {
    const button = event.target.closest("[data-remove-file]");
    if (!button) return;
    releaseSelectedFiles(state[stateKey].splice(Number(button.dataset.removeFile), 1));
    renderSelectedFiles(stateKey);
    if (stateKey === "composerFiles") { saveComposerDraft(); persistDraftFiles(state.selectedId); updateActiveTurnControls(); }
    else persistDraftFiles("new");
  }

  function releaseSelectedFiles(files) {
    files.forEach((file) => { if (file.previewUrl) URL.revokeObjectURL(file.previewUrl); });
  }

  function renderUploadProgress(stateKey, loaded, total, name) {
    const container = $(stateKey === "composerFiles" ? "#composerUploadProgress" : "#newThreadUploadProgress");
    const percent = total ? Math.min(100, Math.round(loaded / total * 100)) : 100;
    container.hidden = false;
    $("progress", container).value = percent;
    $("span", container).textContent = `${percent}% · ${name}`;
  }

  function clearUploadProgress(stateKey) {
    const container = $(stateKey === "composerFiles" ? "#composerUploadProgress" : "#newThreadUploadProgress");
    container.hidden = true;
    $("progress", container).value = 0;
  }

  function renderSelectedFiles(stateKey) {
    const container = $(stateKey === "composerFiles" ? "#composerAttachments" : "#newThreadAttachments");
    const files = state[stateKey];
    container.hidden = files.length === 0;
    container.innerHTML = files.map((file, index) => `<figure class="${IMAGE_TYPES.has(file.type) ? "image-attachment" : "file-attachment"}">${IMAGE_TYPES.has(file.type) ? `<img data-preview-file src="${escapeHtml(file.previewUrl)}" alt="${escapeHtml(file.name)}">` : '<svg><use href="#i-file"/></svg>'}<figcaption>${escapeHtml(file.name)} · ${formatBytes(file.size)}</figcaption><button type="button" data-remove-file="${index}" aria-label="移除 ${escapeHtml(file.name)}">×</button></figure>`).join("");
  }

  async function createThread(event) {
    event.preventDefault();
    if (state.creatingThread) return;
    const button = event.submitter || $("#createThreadSubmit");
    let submission = readSubmission("new");
    const files = [...state.newThreadFiles];
    const cwd = $("#newCwd").value.trim();
    const prompt = $("#newPrompt").value;
    if (!submission && (!cwd || (!prompt.trim() && !files.length))) return;
    state.creatingThread = true;
    setBusy(button, true, "正在创建…");
    try {
      if (!submission) {
        const params = { cwd };
        if ($("#newModel").value) params.model = $("#newModel").value;
        const effort = runtimeRangeValue($("#newEffort"));
        const serviceTier = runtimeRangeValue($("#newServiceTier"));
        if (serviceTier) params.serviceTier = serviceTier;
        const inputs = await turnInputs(prompt.trim(), files, (loaded, total, name) => renderUploadProgress("newThreadFiles", loaded, total, name), "new");
        submission = {
          id: crypto.randomUUID(), turnRequestId: crypto.randomUUID(), params, inputs, text: prompt, effort, serviceTier,
          uploadedPaths: files.map((file) => file.uploaded?.path).filter(Boolean),
        };
        saveSubmission("new", submission);
      }
      if (!submission.thread) {
        const result = await rpc("thread/start", submission.params, submission.id);
        submission.thread = result?.thread || result;
        if (!submission.thread?.id) throw new Error("工作站没有返回会话标识，请继续核对创建结果");
        saveSubmission("new", submission);
      }
      const thread = submission.thread;
      const turnParams = { threadId: thread.id, input: submission.inputs };
      if (submission.effort) turnParams.effort = submission.effort;
      if (submission.serviceTier) turnParams.serviceTier = submission.serviceTier;
      const turnSubmission = readSubmission(thread.id) || {
        id: submission.turnRequestId, method: "turn/start", params: turnParams,
        text: submission.text, uploadedPaths: submission.uploadedPaths,
      };
      saveSubmission(thread.id, turnSubmission);
      const draft = composerDraft(thread.id);
      if (!draft.text) draft.text = submission.text;
      for (const file of files) {
        if (submission.uploadedPaths.includes(file.uploaded?.path) && !draft.files.includes(file)) draft.files.push(file);
      }
      persistComposerDraft(thread.id);
      persistDraftFiles(thread.id);
      state.threads = [thread, ...state.threads.filter((entry) => entry.id !== thread.id)];
      state.selectedProject = submission.params.cwd;
      rememberWorkspaceThreads([thread]);
      renderThreadList();
      try { localStorage.setItem("codex-remote:last-cwd", submission.params.cwd); } catch {}
      closeDialog($("#newThreadDialog"));
      if ($("#newPrompt").value === submission.text) $("#newPrompt").value = "";
      state.newThreadFiles = state.newThreadFiles.filter((file) => !draft.files.includes(file));
      persistDraftFiles("new");
      renderSelectedFiles("newThreadFiles");
      state.sendingThreads.add(thread.id);
      try {
        // Start the first turn before reading history: a new thread may not yet
        // have a readable rollout. Failure still opens this same thread's draft.
        await deliverComposerSubmission(thread.id, turnSubmission);
      } finally {
        state.sendingThreads.delete(thread.id);
        await selectThread(thread.id);
      }
      loadThreads(false).catch(() => {});
    } catch (error) {
      if (submission && !submission.thread && !submissionIsUncertain(error)) saveSubmission("new", null);
      $("#newThreadStatus").textContent = readSubmission("new")
        ? `创建结果待确认：${readableError(error)}。再次点击将核对同一次创建。`
        : readableError(error);
      $("#newThreadStatus").hidden = false;
    } finally {
      state.creatingThread = false;
      clearUploadProgress("newThreadFiles");
      setBusy(button, false);
      renderNewThreadRecovery();
    }
  }

  function renderNewThreadRecovery() {
    const submission = readSubmission("new");
    $("#createThreadSubmit").textContent = submission ? (submission.thread ? "继续发送" : "核对创建") : "创建并发送";
    $("#newThreadForm").noValidate = Boolean(submission);
    if (submission) {
      if ($("#newThreadStatus").hidden) $("#newThreadStatus").textContent = `上次创建尚待确认 · ${submission.params.cwd}。核对会继续原提交，当前表单不会重复创建任务。`;
      $("#newThreadStatus").hidden = false;
    }
  }

  function openNewThreadDialog() {
    restoreDraftFiles("new");
    renderWorkspaceDirectories();
    $("#newThreadStatus").hidden = true;
    renderNewThreadRecovery();
    showDialog($("#newThreadDialog"));
    if (!state.workspaceDiscoveryLoaded) syncWorkspaceDirectories(false).catch(() => {});
    loadDirectory("/");
  }

  function loadStoredWorkspaceDirectories() {
    try {
      const stored = JSON.parse(localStorage.getItem("codex-remote:project-directories") || "[]");
      if (Array.isArray(stored)) state.workspaceDirectories = stored.filter((path) => typeof path === "string" && path.startsWith("/"));
    } catch {
      state.workspaceDirectories = [];
    }
  }

  function rememberWorkspaceThreads(threads) {
    let changed = false;
    for (const thread of threads || []) {
      const cwd = pathText(thread?.cwd);
      if (!thread?.id || !cwd || !cwd.startsWith("/")) continue;
      const previous = state.workspaceThreads.get(thread.id) || {};
      state.workspaceThreads.set(thread.id, { ...previous, ...thread, cwd });
      if (!state.workspaceDirectories.includes(cwd)) {
        state.workspaceDirectories.push(cwd);
        changed = true;
      }
    }
    if (changed) {
      state.workspaceDirectories.sort((left, right) => left.localeCompare(right, "zh-CN"));
      try { localStorage.setItem("codex-remote:project-directories", JSON.stringify(state.workspaceDirectories)); } catch {}
    }
    renderWorkspaceDirectories();
  }

  function applyWorkspaceScope() {
    const hint = $("#workspaceDiscoveryHint");
    if (state.workspaceMode !== "restricted" || !state.workspaceRoots.length) {
      if (hint) hint.textContent = "目录访问已放开，并从已有会话自动发现";
      renderWorkspaceDirectories();
      return;
    }
    const withinRoot = (path) => state.workspaceRoots.some((root) => path === root || path.startsWith(`${root.replace(/\/+$/, "")}/`));
    state.workspaceDirectories = state.workspaceDirectories.filter(withinRoot);
    for (const [threadId, thread] of state.workspaceThreads) {
      if (!withinRoot(thread.cwd)) state.workspaceThreads.delete(threadId);
    }
    try { localStorage.setItem("codex-remote:project-directories", JSON.stringify(state.workspaceDirectories)); } catch {}
    if (hint) hint.textContent = `当前服务已显式收窄到 ${state.workspaceRoots.length} 个目录`;
    renderWorkspaceDirectories();
  }

  function renderWorkspaceDirectories() {
    const select = $("#knownWorkspaceSelect");
    if (!select) return;
    select.innerHTML = '<option value="">选择最近使用的项目…</option>' + state.workspaceDirectories.map((path) => `<option value="${escapeHtml(path)}">${escapeHtml(path)}</option>`).join("");
  }

  async function loadDirectory(path) {
    const list = $("#directoryEntries");
    list.innerHTML = '<div class="list-skeleton"><i></i><i></i><i></i></div>';
    try {
      const result = await request(`/api/directories?path=${encodeURIComponent(path || "/")}`);
      state.directoryPath = result.path || "/";
      state.directoryParent = result.parent || "";
      $("#newCwd").value = state.directoryPath;
      $("#directoryCurrentPath").textContent = state.directoryPath;
      $("#directoryUpButton").disabled = !state.directoryParent;
      $("#knownWorkspaceSelect").value = state.workspaceDirectories.includes(state.directoryPath) ? state.directoryPath : "";
      const entries = Array.isArray(result.entries) ? result.entries : [];
      list.innerHTML = entries.length ? entries.map((entry) => `<button type="button" data-directory-path="${escapeHtml(entry.path)}"><svg><use href="#i-folder"/></svg><span>${escapeHtml(entry.name)}</span><svg><use href="#i-chevron"/></svg></button>`).join("") : '<div class="directory-empty">这个目录没有可进入的子目录</div>';
    } catch (error) {
      list.innerHTML = `<div class="directory-empty">${escapeHtml(readableError(error))}</div>`;
      toast(readableError(error), "error");
    }
  }

  async function syncWorkspaceDirectories(showFeedback = true) {
    if (state.workspaceDiscoveryRunning) return;
    state.workspaceDiscoveryRunning = true;
    const button = $("#syncWorkspaceDirectories");
    if (button) setBusy(button, true, "正在读取…");
    try {
      const [activeResult, archivedResult] = await Promise.all([
        rpc("thread/list", { cursor: null, limit: 200, sortKey: "updated_at", sortDirection: "desc", archived: false }),
        rpc("thread/list", { cursor: null, limit: 200, sortKey: "updated_at", sortDirection: "desc", archived: true }),
      ]);
      const threads = [
        ...normalizeArray(activeResult, ["data", "threads"]),
        ...normalizeArray(archivedResult, ["data", "threads"]),
      ];
      rememberWorkspaceThreads(threads);
      state.workspaceDiscoveryLoaded = true;
      if (showFeedback) toast(`已从 ${threads.length} 个最近会话补齐 ${state.workspaceDirectories.length} 个项目目录`, "success");
    } catch (error) {
      if (showFeedback) toast(readableError(error), "error");
      throw error;
    } finally {
      state.workspaceDiscoveryRunning = false;
      if (button) setBusy(button, false);
    }
  }

  function projectFilesUrl(path = "", download = false) {
    const query = new URLSearchParams({ threadId: state.selectedId, path });
    if (download) query.set("download", "1");
    return `/api/files?${query}`;
  }

  function openProjectFiles(path = "") {
    if (!state.selectedId) return;
    showDialog($("#projectFilesDialog"));
    loadProjectDirectory(path);
  }

  async function loadProjectDirectory(path = "") {
    const list = $("#projectFilesList");
    closeProjectFilePreview();
    list.innerHTML = '<div class="list-skeleton"><i></i><i></i><i></i></div>';
    try {
      renderProjectDirectory(await request(projectFilesUrl(path)));
    } catch (error) {
      list.innerHTML = `<div class="directory-empty">${escapeHtml(readableError(error))}</div>`;
      toast(readableError(error), "error");
    }
  }

  function renderProjectDirectory(result) {
    state.projectFilesPath = result.path || "";
    state.projectFilesParent = result.parent || "";
    const cwd = String(state.selectedThread?.cwd || "").replace(/\/+$/, "");
    $("#projectFilesPath").textContent = state.projectFilesPath.startsWith("/") ? state.projectFilesPath : `${cwd}/${state.projectFilesPath}`;
    $("#projectFilesBack").disabled = state.projectFilesPath === "/";
    const entries = [...(result.entries || [])].sort((a, b) => (b.type === "directory") - (a.type === "directory") || a.name.localeCompare(b.name, "zh-CN", { numeric: true }));
    renderFileSelection();
    $("#projectFilesList").innerHTML = entries.length ? entries.map((entry) => `<div class="project-file-row"><input type="checkbox" data-project-select="${escapeHtml(entry.path)}" aria-label="选择 ${escapeHtml(entry.name)}" ${state.projectFileSelection.has(entry.path) ? "checked" : ""}><button type="button" data-project-entry="${escapeHtml(entry.path)}" data-project-entry-type="${escapeHtml(entry.type)}"><svg><use href="#${entry.type === "directory" ? "i-folder" : "i-file"}"/></svg><span><strong>${escapeHtml(entry.name)}</strong><small>${entry.type === "directory" ? "目录" : formatBytes(entry.size)}</small></span><svg><use href="#i-chevron"/></svg></button></div>`).join("") : '<div class="directory-empty">这个目录为空</div>';
    if (result.truncated) $("#projectFilesList").insertAdjacentHTML("beforeend", '<div class="directory-empty">仅显示前 2000 项</div>');
  }

  function handleProjectFileClick(event) {
    const button = event.target.closest("[data-project-entry]");
    if (!button) return;
    if (button.dataset.projectEntryType === "directory") loadProjectDirectory(button.dataset.projectEntry);
    else openProjectFile(button.dataset.projectEntry);
  }

  async function openProjectFile(path) {
    const sequence = ++state.projectPreviewSequence;
    const preview = $("#projectFilePreview");
    state.projectFileSource = "";
    state.projectPreviewPath = path;
    $("#projectPreviewMode").value = "code";
    $("#projectPreviewMode").disabled = true;
    renderProjectPreview();
    const name = String(path || "").split("/").pop() || path;
    preview.hidden = false;
    $("#projectFilesBody").classList.add("previewing");
    $("#projectFileName").textContent = name;
    $("#projectFileMeta").textContent = "正在读取…";
    $("#projectFileCode").textContent = "";
    const download = $("#projectFileDownload");
    download.href = projectFilesUrl(path, true);
    download.download = name;
    try {
      const response = await fetch(projectFilesUrl(path), { credentials: "same-origin", cache: "no-store" });
      const body = await response.text();
      if (sequence !== state.projectPreviewSequence) return;
      if (!response.ok) {
        let message = body;
        try { message = JSON.parse(body)?.error?.message || JSON.parse(body)?.message || body; } catch {}
        throw new Error(message || `请求失败（${response.status}）`);
      }
      if ((response.headers.get("Content-Type") || "").includes("application/json")) {
        renderProjectDirectory(JSON.parse(body));
        return;
      }
      state.projectFileSource = body;
      $("#projectPreviewMode").disabled = !/\.(md|markdown|html?|json|svg)$/i.test(path);
      const language = codeLanguage(path);
      $("#projectFileMeta").textContent = `${language || "text"} · ${formatBytes(new TextEncoder().encode(body).length)} · 只读`;
      $("#projectFileCode").dataset.language = language || "text";
      $("#projectFileCode").innerHTML = highlightSource(body, language);
    } catch (error) {
      $("#projectFileMeta").textContent = "无法在线预览，可尝试下载";
      $("#projectFileCode").textContent = readableError(error);
    }
  }

  function renderFileSelection() {
    const count = state.projectFileSelection.size;
    $("#downloadSelection").disabled = !count;
    $("#downloadSelection").textContent = count ? `打包下载 (${count})` : "打包下载";
    $("#clearFileSelection").hidden = !count;
  }

  function downloadFileSelection() {
    // Native form downloads stream to disk instead of retaining the ZIP in JS memory.
    const form = document.createElement("form");
    form.method = "POST";
    form.action = "/api/files/archive";
    form.target = "_blank";
    form.rel = "noopener";
    for (const [name, value] of [["threadId", state.selectedId], ...[...state.projectFileSelection].map(path => ["path", path])]) {
      const input = document.createElement("input");
      input.type = "hidden"; input.name = name; input.value = value; form.append(input);
    }
    document.body.append(form); form.submit(); form.remove();
  }

  function renderProjectPreview() {
    const formatted = $("#projectPreviewMode").value === "formatted";
    const rendered = $("#projectFileRendered");
    $("#projectFileCode").parentElement.hidden = formatted;
    rendered.hidden = !formatted;
    rendered.replaceChildren();
    if (!formatted) return;
    const source = state.projectFileSource;
    if (/\.(md|markdown)$/i.test(state.projectPreviewPath)) {
      rendered.innerHTML = markdownHtml(source);
    } else if (/\.json$/i.test(state.projectPreviewPath)) {
      const pre = document.createElement("pre");
      try { pre.textContent = JSON.stringify(JSON.parse(source), null, 2); }
      catch { pre.textContent = "JSON 格式无效，请查看源码。"; }
      rendered.append(pre);
    } else {
      const frame = document.createElement("iframe");
      frame.title = "文件格式化预览";
      frame.setAttribute("sandbox", "");
      frame.referrerPolicy = "no-referrer";
      const url = new URL(projectFilesUrl(state.projectPreviewPath), location.href);
      url.searchParams.set("preview", "1");
      frame.src = url.href;
      rendered.append(frame);
    }
  }

  function closeProjectFilePreview() {
    ++state.projectPreviewSequence;
    $("#projectFileRendered").replaceChildren();
    $("#projectFilePreview").hidden = true;
    $("#projectFilesBody").classList.remove("previewing");
  }

  function openProjectPath(value) {
    const cwd = String(state.selectedThread?.cwd || "").replace(/\/+$/, "");
    let path = String(value || "").replace(/:(\d+)(?::\d+)?$/, "").replace(/^\.\//, "");
    if (!path || (!cwd && !path.startsWith("/"))) return false;
    showDialog($("#projectFilesDialog"));
    const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
    loadProjectDirectory(parent).then(() => openProjectFile(path));
    return true;
  }


  function attachmentStore(mode, key, value) {
    if (!("indexedDB" in window)) return Promise.resolve(null);
    state.draftDB ||= new Promise((resolve, reject) => {
      const request = indexedDB.open("codex-remote-drafts", 1);
      request.onupgradeneeded = () => request.result.createObjectStore("files");
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    return state.draftDB.then(db => new Promise((resolve, reject) => {
      const tx = db.transaction("files", mode), store = tx.objectStore("files");
      const request = mode === "readonly" ? store.get(key) : value.length ? store.put(value, key) : store.delete(key);
      tx.oncomplete = () => resolve(request.result);
      tx.onerror = () => reject(tx.error);
      tx.onabort = () => reject(tx.error);
    }));
  }

  function persistDraftFiles(threadId) {
    const files = threadId === "new" ? state.newThreadFiles : composerDraft(threadId).files;
    const saved = files.map(({name, size, type, file, uploaded}) => ({name, size, type, file, uploaded}));
    return attachmentStore("readwrite", threadId, saved).catch(() => toast("浏览器未能保存附件，请保持页面打开直到发送完成", "warning"));
  }

  async function restoreDraftFiles(threadId) {
    const draft = threadId === "new" ? { files: state.newThreadFiles } : composerDraft(threadId);
    if (draft.files.length || draft.restored) return;
    draft.restored = true;
    try {
      const files = await attachmentStore("readonly", threadId);
      if (!files?.length || draft.files.length) return;
      draft.files = files.map(file => ({...file, previewUrl: IMAGE_TYPES.has(file.type) ? URL.createObjectURL(file.file) : ""}));
      if (threadId === "new") { state.newThreadFiles = draft.files; renderSelectedFiles("newThreadFiles"); }
      else if (state.selectedId === threadId) { state.composerFiles = draft.files; renderSelectedFiles("composerFiles"); updateActiveTurnControls(); }
    } catch { toast("附件草稿读取失败，文字草稿仍可使用", "warning"); }
  }

  function previewSelectedFile(event) {
    const image = event.target.closest("[data-preview-file]");
    if (image) openImageViewer(image);
  }

  async function openUploads() {
    showDialog($("#uploadsDialog"));
    const list = $("#uploadsList");
    list.textContent = "正在读取附件…";
    try {
      const result = await request("/api/uploads");
      list.innerHTML = (result.data || []).map(file => `<article class="upload-row"><button type="button" class="text-button" data-open-upload="${escapeHtml(file.path)}">${escapeHtml(file.name)}<small>${formatBytes(file.size)}</small></button><a class="quiet-button" href="${escapeHtml(projectFilesUrl(file.path, true))}" download>下载</a><button type="button" class="quiet-button" data-delete-upload="${escapeHtml(file.path)}" aria-label="删除 ${escapeHtml(file.name)}">删除</button></article>`).join("") || '<p class="list-message">暂无已上传附件</p>';
    } catch (error) { list.textContent = readableError(error); }
  }

  async function handleUploadAction(event) {
    const open = event.target.closest("[data-open-upload]");
    if (open) {
      const path = open.dataset.openUpload;
      if (/\.(png|jpe?g|gif|webp)$/i.test(path)) { const image = document.createElement("img"); image.src = artifactUrl(path); image.alt = open.textContent; openImageViewer(image); }
      else openProjectPath(path);
      return;
    }
    const remove = event.target.closest("[data-delete-upload]");
    if (!remove || !confirm("删除工作站上的这个附件？历史消息中的链接将无法再打开。")) return;
    setBusy(remove, true);
    try {
      await request(`/api/uploads?path=${encodeURIComponent(remove.dataset.deleteUpload)}`, {method:"DELETE"});
      for (const [threadId,draft] of state.composerDrafts) { for (const file of draft.files) if (file.uploaded?.path === remove.dataset.deleteUpload) delete file.uploaded; persistDraftFiles(threadId); }
      for (const file of state.newThreadFiles) if (file.uploaded?.path === remove.dataset.deleteUpload) delete file.uploaded;
      persistDraftFiles("new"); await openUploads();
    }
    catch (error) { toast(readableError(error), "error"); setBusy(remove, false); }
  }

  function renderRequestHistory() {
    const labels = {answered:"已处理", timeout:"已过期", disconnected:"连接中断", resolved:"已在其他客户端处理", thread_revoked:"已失效", unknown:"结果未确认"};
    $("#requestHistory").innerHTML = state.requestHistory.map(entry => `<article class="request-history-row"><strong>${escapeHtml(requestMeta(entry.method).title)} · ${escapeHtml(entry.decision || labels[entry.status] || entry.status)}</strong><small>${escapeHtml(entry.summary || "")} · ${escapeHtml(new Date(entry.at).toLocaleString())}</small>${entry.threadId ? `<button type="button" class="text-button" data-history-thread="${escapeHtml(entry.threadId)}">打开任务</button>` : ""}</article>`).join("") || '<p class="list-message">暂无处理记录</p>';
  }
  function codeLanguage(path) {
    const name = String(path || "").split("/").pop().toLowerCase();
    if (["dockerfile", "makefile"].includes(name)) return name;
    const extension = name.includes(".") ? name.split(".").pop() : "";
    return ({ js: "javascript", mjs: "javascript", cjs: "javascript", jsx: "javascript", ts: "typescript", tsx: "typescript", go: "go", py: "python", rs: "rust", java: "java", c: "c", h: "c", cc: "cpp", cpp: "cpp", hpp: "cpp", cs: "csharp", sh: "shell", bash: "shell", zsh: "shell", json: "json", css: "css", html: "html", htm: "html", xml: "xml", yaml: "yaml", yml: "yaml", toml: "toml", md: "markdown", sql: "sql" })[extension] || extension;
  }

  function highlightSource(value, language = "") {
    const source = String(value ?? "");
    const keywordGroups = {
      javascript: "async await break case catch class const continue debugger default delete do else export extends false finally for from function get if import in instanceof let new null of return set static super switch this throw true try typeof undefined var void while with yield",
      typescript: "abstract any as async await boolean break case catch class const constructor continue declare default delete do else enum export extends false finally for from function get if implements import in infer instanceof interface keyof let namespace never new null number object of private protected public readonly return set static string super switch symbol this throw true try type typeof undefined unknown var void while yield",
      go: "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var true false nil",
      python: "and as assert async await break class continue def del elif else except False finally for from global if import in is lambda None nonlocal not or pass raise return True try while with yield",
      rust: "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
      java: "abstract assert boolean break byte case catch char class const continue default do double else enum extends false final finally float for goto if implements import instanceof int interface long native new null package private protected public return short static strictfp super switch synchronized this throw throws transient true try void volatile while",
      json: "true false null",
      toml: "true false",
      yaml: "true false null",
      shell: "case do done elif else esac export fi for function if in local readonly return set then unset while",
      sql: "and as asc by case create delete desc distinct drop else end false from group having in inner insert into is join left like limit not null on or order outer right select set table true union update values where",
    };
    const keywords = new Set((keywordGroups[language] || "").split(" ").filter(Boolean));
    const plain = (text) => {
      let html = "";
      let last = 0;
      for (const match of text.matchAll(/\b(?:0x[\da-f]+|\d+(?:\.\d+)?|[A-Za-z_$][\w$]*)\b/gi)) {
        html += escapeHtml(text.slice(last, match.index));
        const token = match[0];
        const className = /^\d|^0x/i.test(token) ? "token-number" : keywords.has(token) ? "token-keyword" : "";
        html += className ? `<span class="${className}">${escapeHtml(token)}</span>` : escapeHtml(token);
        last = match.index + token.length;
      }
      return html + escapeHtml(text.slice(last));
    };
    const hashComments = ["python", "shell", "yaml", "toml", "makefile"].includes(language);
    const pattern = hashComments
      ? /(?:'''[\s\S]*?'''|"""[\s\S]*?"""|#[^\n]*|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')/g
      : /(?:\/\*[\s\S]*?\*\/|\/\/[^\n]*|<!--[\s\S]*?-->|`(?:\\.|[^`\\])*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')/g;
    let html = "";
    let last = 0;
    for (const match of source.matchAll(pattern)) {
      html += plain(source.slice(last, match.index));
      const token = match[0];
      const comment = token.startsWith("//") || token.startsWith("/*") || token.startsWith("#") || token.startsWith("<!--");
      html += `<span class="${comment ? "token-comment" : "token-string"}">${escapeHtml(token)}</span>`;
      last = match.index + token.length;
    }
    return html + plain(source.slice(last));
  }

  function toggleThreadMenu() {
    const menu = $("#threadMenu");
    menu.hidden = !menu.hidden;
    $("#threadMenuButton").setAttribute("aria-expanded", String(!menu.hidden));
  }

  function closeThreadMenu() {
    $("#threadMenu").hidden = true;
    $("#threadMenuButton").setAttribute("aria-expanded", "false");
  }

  async function handleThreadAction(action) {
    if (action === "results") { closeThreadMenu(); openResults(); return; }
    closeThreadMenu();
    if (!state.selectedId) return;
    if (action === "files") {
      openProjectFiles();
      return;
    }
    if (action === "agents") {
      toggleSubagentsPanel();
      return;
    }
    if (action === "goal") {
      openGoalDialog();
      return;
    }
    if (action === "interrupt") {
      await interruptTurn();
      return;
    }
    if (action === "requests") {
      openRequestsCenter();
      return;
    }
    if (action === "status") {
      openStatusDialog();
      return;
    }
    if (action === "rename") {
      openRenameDialog(state.selectedId);
      return;
    }
    if (action === "review") {
      openReviewDialog();
      return;
    }
    if (action === "archive") return toggleThreadArchived(state.selectedId, true);
    try {
      if (action === "resume") {
        const result = await rpc("thread/resume", { threadId: state.selectedId });
        state.selectedThread = result?.thread || result;
        state.selectedRuntime = state.selectedThread?.runtime || {};
        state.composerSettingsThreadId = null;
        syncComposerSettings(state.selectedThread);
        state.activeTurnId = findActiveTurn(state.selectedThread)?.id || null;
        renderThreadDetail(false);
        scheduleTimelineRender(true);
        toast("会话已恢复到工作站", "success");
      } else if (action === "fork") {
        const result = await rpc("thread/fork", { threadId: state.selectedId });
        const thread = result?.thread || result;
        if (!thread?.id) throw new Error("工作站没有返回分支会话 ID");
        state.threads.unshift(thread);
        await selectThread(thread.id);
        toast("已创建会话分支", "success");
      } else if (action === "compact") {
        if (!window.confirm("确认压缩当前会话上下文？压缩记录会写入会话时间线。")) return;
        await rpc("thread/compact/start", { threadId: state.selectedId });
        toast("上下文压缩已启动", "success");
      }
    } catch (error) {
      toast(readableError(error), "error");
    }
  }

  function openRenameDialog(threadId) {
    const thread = state.threads.find((entry) => entry.id === threadId) || (state.selectedId === threadId ? state.selectedThread : null);
    if (!thread) return;
    $("#renameDialog").dataset.threadId = threadId;
    $("#renameInput").value = thread.name || threadTitle(thread);
    showDialog($("#renameDialog"));
    setTimeout(() => $("#renameInput").select(), 40);
  }

  async function toggleThreadArchived(threadId, confirmAction = false) {
    if (!threadId) return;
    const archived = state.archived;
    const verb = archived ? "取消归档" : "归档";
    if (confirmAction && !window.confirm(`确认${verb}这个会话？`)) return;
    try {
      await rpc(archived ? "thread/unarchive" : "thread/archive", { threadId });
      if (state.selectedId === threadId) {
        state.selectedId = null;
        state.selectedThread = null;
        $("#threadDetail").hidden = true;
        $("#emptyDetail").hidden = false;
        showThreadRoute();
      }
      await loadThreads(true);
      toast(`${compactId(threadId)} 已${verb}`, "success");
    } catch (error) {
      toast(readableError(error), "error");
    }
  }

  function openReviewDialog() {
    $("#reviewTargetType").value = "uncommittedChanges";
    $("#reviewDelivery").value = "inline";
    $("#reviewTargetValue").value = "";
    renderReviewTargetField();
    showDialog($("#reviewDialog"));
  }

  function renderReviewTargetField() {
    const type = $("#reviewTargetType").value;
    const field = $("#reviewTargetValueField");
    const input = $("#reviewTargetValue");
    const config = {
      baseBranch: ["基础分支", "例如 main 或 origin/main"],
      commit: ["提交 SHA", "输入要审查的提交 SHA"],
      custom: ["审查说明", "描述要 Codex 审查的范围与关注点"],
    }[type];
    field.hidden = !config;
    input.required = Boolean(config);
    if (config) {
      $("#reviewTargetValueLabel").textContent = config[0];
      input.placeholder = config[1];
    }
  }

  async function startReview(event) {
    event.preventDefault();
    if (!state.selectedId) return;
    const type = $("#reviewTargetType").value;
    const value = $("#reviewTargetValue").value.trim();
    let target = { type };
    if (type === "baseBranch") target = { type, branch: value };
    if (type === "commit") target = { type, sha: value };
    if (type === "custom") target = { type, instructions: value };
    if (type !== "uncommittedChanges" && !value) {
      toast("请填写审查目标", "error");
      return;
    }
    const button = event.submitter || $("#reviewSubmit");
    setBusy(button, true, "启动中…");
    try {
      const result = await rpc("review/start", { threadId: state.selectedId, target, delivery: $("#reviewDelivery").value });
      closeDialog($("#reviewDialog"));
      const reviewThreadId = result?.reviewThreadId;
      const turn = result?.turn;
      if (reviewThreadId && reviewThreadId !== state.selectedId) {
        await selectThread(reviewThreadId, { silent: true });
      } else if (turn) {
        upsertTurn(turn);
        if (turn.status === "inProgress") state.activeTurnId = turn.id;
        updateActiveTurnControls();
        scheduleTimelineRender(true);
      }
      toast("代码审查已启动", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  async function renameThread(event) {
    event.preventDefault();
    const name = $("#renameInput").value.trim();
    const threadId = $("#renameDialog").dataset.threadId;
    if (!name || !threadId) return;
    const button = event.submitter;
    setBusy(button, true, "保存中…");
    try {
      await rpc("thread/name/set", { threadId, name });
      updateThreadName(threadId, name);
      closeDialog($("#renameDialog"));
      toast("名称已更新", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  async function openGoalDialog() {
    if (!state.selectedId) return;
    try {
      const result = await rpc("thread/goal/get", { threadId: state.selectedId });
      state.selectedGoal = result?.goal ?? result ?? null;
    } catch {
      // Goal may not exist yet; the form still permits creating it.
    }
    $("#goalInput").value = state.selectedGoal?.objective || "";
    $("#goalStatus").value = state.selectedGoal?.status || "active";
    $("#goalBudget").value = state.selectedGoal?.tokenBudget || "";
    $("#clearGoalButton").hidden = !state.selectedGoal;
    renderGoalUsage();
    showDialog($("#goalDialog"));
  }

  async function saveGoal(event) {
    event.preventDefault();
    if (!state.selectedId) return;
    const objective = $("#goalInput").value.trim();
    const budgetValue = $("#goalBudget").value.trim();
    const params = {
      threadId: state.selectedId,
      objective,
      status: $("#goalStatus").value,
      tokenBudget: budgetValue ? Number(budgetValue) : null,
    };
    const button = event.submitter;
    setBusy(button, true, "保存中…");
    try {
      const result = await rpc("thread/goal/set", params);
      state.selectedGoal = result?.goal || result;
      renderGoalBanner();
      closeDialog($("#goalDialog"));
      toast("目标已保存", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  async function clearGoal() {
    if (!state.selectedId || !state.selectedGoal || !window.confirm("确认清除这个会话的目标？")) return;
    const button = $("#clearGoalButton");
    setBusy(button, true, "清除中…");
    try {
      await rpc("thread/goal/clear", { threadId: state.selectedId });
      state.selectedGoal = null;
      renderGoalBanner();
      closeDialog($("#goalDialog"));
      toast("目标已清除", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  async function loadRequests() {
    try {
      const payload = await request("/api/requests");
      const root = payload && Object.prototype.hasOwnProperty.call(payload, "result") ? payload.result : payload;
      const requests = Array.isArray(root) ? root : normalizeArray(root, ["requests", "data"]);
      state.requestHistory = (root.history || []).filter(entry => entry.status !== "pending");
      state.pendingRequests.clear();
      requests.forEach((entry, index) => {
        const normalized = normalizeRequest(entry, index);
        if (normalized) state.pendingRequests.set(normalized.key, normalized);
      });
      renderRequestCount();
      renderRequestsList();
    } catch (error) {
      if (error.status !== 401) console.info("待处理请求暂不可用", error);
    }
  }

  function normalizeRequest(entry, fallbackIndex = 0) {
    if (!entry || typeof entry !== "object") return null;
    const requestObject = entry.request && typeof entry.request === "object" ? entry.request : entry;
    const keyValue = requestObject.key ?? entry.key ?? requestObject.id ?? entry.id ?? `pending-${fallbackIndex}`;
    return {
      key: String(keyValue),
      method: requestObject.method || entry.method || "unknown",
      params: requestObject.params || entry.params || {},
      fileChanges: requestObject.fileChanges || entry.fileChanges || null,
      canAccept: requestObject.canAccept === true || entry.canAccept === true,
      policyReason: requestObject.policyReason || entry.policyReason || "",
    };
  }

  function renderRequestCount() {
    const count = state.pendingRequests.size;
    for (const element of [$("#requestCount"), $("#threadMenuRequestCount")]) {
      element.textContent = count > 99 ? "99+" : String(count);
      element.hidden = count === 0;
    }
    $("#requestsTitleCount").textContent = String(count);
  }

  function renderRequestsList() {
    const list = $("#requestsList");
    renderRequestHistory();
    if (!state.pendingRequests.size) {
      list.innerHTML = '<div class="requests-empty"><span class="eyebrow">QUEUE CLEAR</span><p>当前没有等待处理的请求</p></div>';
      return;
    }
    list.innerHTML = [...state.pendingRequests.values()].map((entry) => {
      const meta = requestMeta(entry.method);
      const params = entry.params || {};
      const detail = params.command || params.reason || params.message || params.questions?.[0]?.question || params.cwd || entry.method;
      return `<button class="request-card" type="button" data-request-key="${escapeHtml(entry.key)}">
        <span class="request-glyph">${escapeHtml(meta.glyph)}</span>
        <span class="request-card-copy"><strong>${escapeHtml(meta.title)}</strong><small>${escapeHtml(shorten(String(detail), 120))}</small></span>
        <svg><use href="#i-chevron"/></svg>
      </button>`;
    }).join("");
  }

  function openRequestsCenter() {
    loadRequests();
    renderRequestsList();
    showDialog($("#requestsDialog"));
  }

  function openRequest(key) {
    const entry = state.pendingRequests.get(String(key));
    if (!entry) {
      toast("这个请求已经失效或被其他客户端处理", "warning");
      loadRequests();
      return;
    }
    state.currentRequestKey = entry.key;
    const meta = requestMeta(entry.method);
    $("#requestKicker").textContent = meta.kicker;
    $("#requestTitle").textContent = meta.title;
    renderRequest(entry);
    closeDialog($("#requestsDialog"));
    showDialog($("#requestDialog"));
  }

  function renderRequest(entry) {
    const method = entry.method;
    const params = entry.params || {};
    const body = $("#requestBody");
    const actions = $("#requestActions");
    actions.innerHTML = "";
    $("#requestForm").dataset.requestKind = "decision";

    if (method === "mcpServer/elicitation/request") {
      const mode = params.mode || "form";
      const header = `<div class="request-summary elicitation-message"><strong>${escapeHtml(params.serverName || "MCP SERVER")}</strong><p>${escapeHtml(params.message || "MCP 服务需要你的输入。")}</p></div>`;
      if (mode === "url") {
        const url = safeHttpsUrl(params.url);
        $("#requestForm").dataset.requestKind = "mcp-elicitation-url";
        body.innerHTML = `${header}<div class="elicitation-url ${url ? "safe" : "unsafe"}"><span>${url ? "HTTPS LINK" : "BLOCKED LINK"}</span><code>${escapeHtml(params.url || "未提供 URL")}</code><small>${url ? "只在新标签页打开；请核对域名后继续。" : "为避免降级与跨协议风险，只允许打开 HTTPS 地址。"}</small></div>`;
        actions.innerHTML = `${url ? '<button class="session-button" type="button" data-elicitation-open>打开链接<svg><use href="#i-external"/></svg></button>' : ""}<button class="decline-button" type="button" data-elicitation-action="decline">拒绝</button><button class="session-button" type="button" data-elicitation-action="cancel">取消</button>${url ? '<button class="accept-button" type="button" data-elicitation-action="accept">已完成并接受</button>' : ""}`;
        return;
      }
      if (mode === "openai/form") {
        $("#requestForm").dataset.requestKind = "mcp-elicitation-openai-form";
        body.innerHTML = `${header}<div class="request-summary request-policy-warning"><strong>当前客户端不会代填 OpenAI 托管表单</strong><br>该模式没有可安全解释的本地 primitive schema；可拒绝或取消，避免发送不完整内容。</div>`;
        actions.innerHTML = '<button class="decline-button" type="button" data-elicitation-action="decline">拒绝</button><button class="session-button" type="button" data-elicitation-action="cancel">取消</button>';
        return;
      }
      $("#requestForm").dataset.requestKind = "mcp-elicitation-form";
      body.innerHTML = `${header}${renderElicitationForm(params)}`;
      actions.innerHTML = '<button class="decline-button" type="button" data-elicitation-action="decline">拒绝</button><button class="session-button" type="button" data-elicitation-action="cancel">取消</button><button class="accept-button" type="submit">提交并接受</button>';
      return;
    }

    const policyBlock = entry.canAccept
      ? ""
      : `<div class="request-summary request-policy-warning"><strong>仅可拒绝或取消</strong><br>${escapeHtml(entry.policyReason || "授权范围无法由接收端完整验证")}</div>`;

    if (method === "item/commandExecution/requestApproval" || method === "execCommandApproval") {
      const decisions = Array.isArray(params.availableDecisions)
        ? params.availableDecisions.filter((decision) => typeof decision === "string")
        : ["accept", "decline", "cancel"];
      const command = Array.isArray(params.command) ? params.command.join(" ") : (params.command || params.cmd || "未提供命令");
      const scope = {
        commandActions: params.commandActions || params.parsedCmd || null,
        additionalPermissions: params.additionalPermissions || null,
        networkApprovalContext: params.networkApprovalContext || null,
        proposedExecpolicyAmendment: params.proposedExecpolicyAmendment || null,
        proposedNetworkPolicyAmendments: params.proposedNetworkPolicyAmendments || null,
      };
      const hasScope = Object.values(scope).some((value) => value !== null && value !== undefined);
      body.innerHTML = `${policyBlock}${params.reason ? `<div class="request-summary">${escapeHtml(params.reason)}</div>` : ""}
        <dl class="request-detail-grid"><dt>工作目录</dt><dd>${escapeHtml(pathText(params.cwd) || "未提供")}</dd><dt>会话</dt><dd>${escapeHtml(compactId(params.threadId))}</dd>${params.environmentId ? `<dt>环境</dt><dd>${escapeHtml(params.environmentId)}</dd>` : ""}</dl>
        <pre class="request-command">${escapeHtml(command)}</pre>
        ${hasScope ? `<details class="raw-status" open><summary>完整动作与附加权限</summary><pre>${escapeHtml(safeStringify(scope, 2))}</pre></details>` : ""}`;
      actions.innerHTML = `${decisions.includes("decline") ? '<button class="decline-button" type="button" data-request-decision="decline">拒绝</button>' : ""}
        ${decisions.includes("cancel") ? '<button class="session-button" type="button" data-request-decision="cancel">取消</button>' : ""}
        ${entry.canAccept && decisions.includes("accept") ? '<button class="accept-button" type="button" data-request-decision="accept">允许一次</button>' : ""}`;
      return;
    }

    if (method === "item/permissions/requestApproval") {
      body.innerHTML = `${policyBlock}${params.reason ? `<div class="request-summary">${escapeHtml(params.reason)}</div>` : ""}
        <div class="request-scope-note"><strong>仅作用于当前 Turn</strong><span>该决定不会创建永久授权，也不会改变后续 Turn 的审批策略。</span></div>
        <dl class="request-detail-grid"><dt>工作目录</dt><dd>${escapeHtml(pathText(params.cwd) || "未提供")}</dd><dt>会话</dt><dd>${escapeHtml(compactId(params.threadId))}</dd><dt>Turn</dt><dd>${escapeHtml(compactId(params.turnId))}</dd><dt>Item</dt><dd>${escapeHtml(compactId(params.itemId))}</dd>${params.environmentId ? `<dt>环境</dt><dd>${escapeHtml(params.environmentId)}</dd>` : ""}</dl>
        <details class="raw-status" open><summary>请求的权限范围</summary><pre>${escapeHtml(safeStringify(params.permissions || {}, 2))}</pre></details>`;
      actions.innerHTML = '<button class="decline-button" type="button" data-request-decision="decline">拒绝</button><button class="session-button" type="button" data-request-decision="cancel">取消</button>' + (entry.canAccept ? '<button class="accept-button" type="button" data-request-decision="accept">允许当前 Turn</button>' : "");
      return;
    }

    if (method === "item/fileChange/requestApproval" || method === "applyPatchApproval") {
      const changes = entry.fileChanges || params.fileChanges || params.changes || null;
      body.innerHTML = `${policyBlock}${params.reason ? `<div class="request-summary">${escapeHtml(params.reason)}</div>` : '<div class="request-summary">Codex 请求写入或修改工作区文件。</div>'}
        <dl class="request-detail-grid"><dt>会话</dt><dd>${escapeHtml(compactId(params.threadId || params.conversationId))}</dd><dt>Turn / Call</dt><dd>${escapeHtml(compactId(params.turnId || params.callId))}</dd>${params.grantRoot ? `<dt>写入范围</dt><dd>${escapeHtml(params.grantRoot)}</dd>` : ""}</dl>
        ${changes ? `<details class="raw-status" open><summary>已核验的文件路径与类型</summary><pre>${escapeHtml(safeStringify(changes, 2))}</pre></details>` : '<div class="request-summary">协议没有提供可独立核验的文件清单。</div>'}`;
      actions.innerHTML = `<button class="decline-button" type="button" data-request-decision="decline">拒绝</button><button class="session-button" type="button" data-request-decision="cancel">取消</button>${entry.canAccept ? '<button class="accept-button" type="button" data-request-decision="accept">允许一次</button>' : ""}`;
      return;
    }

    if (method === "item/tool/requestUserInput") {
      $("#requestForm").dataset.requestKind = "user-input";
      const questions = Array.isArray(params.questions) ? params.questions : [];
      body.innerHTML = questions.length ? questions.map(renderQuestion).join("") : '<div class="request-summary">工作站没有提供问题内容。</div>';
      actions.innerHTML = '<button class="decline-button" type="button" data-close-request-form>稍后处理</button><button class="accept-button" type="submit">提交回答</button>';
      $("[data-close-request-form]", actions)?.addEventListener("click", () => closeDialog($("#requestDialog")));
      return;
    }

    $("#requestForm").dataset.requestKind = "generic";
    body.innerHTML = `<div class="request-summary">这是当前界面尚未专门适配的服务端请求。核对方法和参数后，可手动填写 JSON 响应。</div>
      <dl class="request-detail-grid"><dt>方法</dt><dd>${escapeHtml(method)}</dd><dt>请求键</dt><dd>${escapeHtml(entry.key)}</dd></dl>
      <details class="raw-status" open><summary>请求参数</summary><pre>${escapeHtml(safeStringify(params, 2))}</pre></details>
      <label class="field"><span>响应 result（JSON）</span><textarea id="genericResponse" class="generic-response-editor" rows="5" spellcheck="false">{}</textarea></label>`;
    actions.innerHTML = '<button class="decline-button" type="button" data-close-request-form>稍后处理</button><button class="accept-button" type="submit">发送响应</button>';
    $("[data-close-request-form]", actions)?.addEventListener("click", () => closeDialog($("#requestDialog")));
  }

  function safeHttpsUrl(value) {
    try {
      const url = new URL(String(value || ""));
      return url.protocol === "https:" ? url.href : "";
    } catch {
      return "";
    }
  }

  function elicitationEnumOptions(schema) {
    const variants = Array.isArray(schema?.oneOf) ? schema.oneOf : Array.isArray(schema?.anyOf) ? schema.anyOf : null;
    if (variants) return variants.filter((entry) => entry && Object.prototype.hasOwnProperty.call(entry, "const")).map((entry) => ({ value: entry.const, label: entry.title || String(entry.const) }));
    if (Array.isArray(schema?.enum)) return schema.enum.map((value, index) => ({ value, label: schema.enumNames?.[index] || String(value) }));
    return [];
  }

  function renderElicitationForm(params) {
    const schema = params.requestedSchema || params.schema || {};
    const properties = schema.properties && typeof schema.properties === "object" ? Object.entries(schema.properties) : [];
    const required = new Set(Array.isArray(schema.required) ? schema.required : []);
    if (!properties.length) return '<div class="request-summary request-policy-warning">表单没有可识别的 primitive 字段；请拒绝或取消。</div>';
    return `<div class="elicitation-form">${properties.map(([name, field], index) => renderElicitationField(name, field || {}, required.has(name), index)).join("")}</div>`;
  }

  function renderElicitationField(name, schema, required, index) {
    const id = `elicitation-${index}`;
    const title = schema.title || name;
    const description = schema.description ? `<small>${escapeHtml(schema.description)}</small>` : "";
    const requiredMark = required ? '<em aria-label="必填">REQUIRED</em>' : '<em>OPTIONAL</em>';
    const common = `id="${id}" data-elicitation-name="${escapeHtml(name)}"${required ? " required" : ""}`;
    let control = "";
    if (schema.type === "array") {
      const options = elicitationEnumOptions(schema.items || {});
      const defaults = new Set(Array.isArray(schema.default) ? schema.default.map(String) : []);
      control = `<div class="elicitation-checks" data-elicitation-name="${escapeHtml(name)}" data-field-kind="array">${options.map((option, optionIndex) => `<label><input type="checkbox" value="${escapeHtml(option.value)}"${defaults.has(String(option.value)) ? " checked" : ""}><span>${escapeHtml(option.label)}</span></label>`).join("") || '<small>没有可选枚举值</small>'}</div>`;
    } else {
      const options = elicitationEnumOptions(schema);
      if (options.length) {
        control = `<select ${common} data-field-kind="enum"><option value="">请选择</option>${options.map((option) => `<option value="${escapeHtml(option.value)}"${String(schema.default) === String(option.value) ? " selected" : ""}>${escapeHtml(option.label)}</option>`).join("")}</select>`;
      } else if (schema.type === "boolean") {
        const defaultValue = schema.default === true ? "true" : schema.default === false ? "false" : "";
        control = `<select ${common} data-field-kind="boolean"><option value="">请选择</option><option value="true"${defaultValue === "true" ? " selected" : ""}>是</option><option value="false"${defaultValue === "false" ? " selected" : ""}>否</option></select>`;
      } else if (schema.type === "number" || schema.type === "integer") {
        control = `<input ${common} data-field-kind="${schema.type}" type="number" inputmode="decimal"${schema.type === "integer" ? ' step="1"' : ' step="any"'}${schema.minimum !== undefined ? ` min="${escapeHtml(schema.minimum)}"` : ""}${schema.maximum !== undefined ? ` max="${escapeHtml(schema.maximum)}"` : ""}${schema.default !== undefined ? ` value="${escapeHtml(schema.default)}"` : ""}>`;
      } else {
        const type = ({ email: "email", uri: "url", date: "date", "date-time": "datetime-local" })[schema.format] || "text";
        control = `<input ${common} data-field-kind="string" type="${type}"${schema.minLength !== undefined ? ` minlength="${escapeHtml(schema.minLength)}"` : ""}${schema.maxLength !== undefined ? ` maxlength="${escapeHtml(schema.maxLength)}"` : ""}${schema.default !== undefined ? ` value="${escapeHtml(schema.default)}"` : ""}>`;
      }
    }
    return `<label class="elicitation-field" data-elicitation-field="${escapeHtml(name)}"><span><strong>${escapeHtml(title)}</strong>${requiredMark}</span>${description}${control}</label>`;
  }

  function collectElicitationContent(entry) {
    const schema = entry.params?.requestedSchema || entry.params?.schema || {};
    const properties = schema.properties && typeof schema.properties === "object" ? schema.properties : {};
    const required = new Set(Array.isArray(schema.required) ? schema.required : []);
    const content = {};
    for (const [name, fieldSchema] of Object.entries(properties)) {
      const root = $$('[data-elicitation-field]', $("#requestBody")).find((element) => element.dataset.elicitationField === name);
      if (!root) continue;
      const schemaEntry = fieldSchema || {};
      let value;
      if (schemaEntry.type === "array") {
        value = $$('input[type="checkbox"]:checked', root).map((input) => input.value);
      } else {
        const control = $("[data-elicitation-name]", root);
        const raw = control?.value ?? "";
        if (raw === "") value = undefined;
        else if (schemaEntry.type === "boolean") value = raw === "true";
        else if (schemaEntry.type === "number" || schemaEntry.type === "integer") {
          value = Number(raw);
          if (!Number.isFinite(value)) throw new Error(`“${schemaEntry.title || name}”必须是数字`);
          if (schemaEntry.type === "integer" && !Number.isInteger(value)) throw new Error(`“${schemaEntry.title || name}”必须是整数`);
        } else value = raw;
      }
      const empty = value === undefined || value === "" || (Array.isArray(value) && value.length === 0);
      if (required.has(name) && empty) throw new Error(`请填写“${schemaEntry.title || name}”`);
      if (empty) continue;
      if (typeof value === "string" && schemaEntry.minLength !== undefined && value.length < schemaEntry.minLength) throw new Error(`“${schemaEntry.title || name}”至少 ${schemaEntry.minLength} 个字符`);
      if (typeof value === "string" && schemaEntry.maxLength !== undefined && value.length > schemaEntry.maxLength) throw new Error(`“${schemaEntry.title || name}”最多 ${schemaEntry.maxLength} 个字符`);
      if (typeof value === "number" && schemaEntry.minimum !== undefined && value < schemaEntry.minimum) throw new Error(`“${schemaEntry.title || name}”不能小于 ${schemaEntry.minimum}`);
      if (typeof value === "number" && schemaEntry.maximum !== undefined && value > schemaEntry.maximum) throw new Error(`“${schemaEntry.title || name}”不能大于 ${schemaEntry.maximum}`);
      if (Array.isArray(value) && schemaEntry.minItems !== undefined && value.length < schemaEntry.minItems) throw new Error(`“${schemaEntry.title || name}”至少选择 ${schemaEntry.minItems} 项`);
      if (Array.isArray(value) && schemaEntry.maxItems !== undefined && value.length > schemaEntry.maxItems) throw new Error(`“${schemaEntry.title || name}”最多选择 ${schemaEntry.maxItems} 项`);
      content[name] = value;
    }
    return content;
  }

  function renderQuestion(question, index) {
    const id = question.id || `question-${index}`;
    const options = Array.isArray(question.options) ? question.options : [];
    let controls = "";
    if (options.length) {
      controls = `<div class="option-list">${options.map((option, optionIndex) => `<label class="option-choice"><input type="radio" name="question-${index}" value="${escapeHtml(option.label)}"${optionIndex === 0 ? " required" : ""}><span><strong>${escapeHtml(option.label)}</strong>${option.description ? `<small>${escapeHtml(option.description)}</small>` : ""}</span></label>`).join("")}
        ${question.isOther ? `<label class="option-choice"><input type="radio" name="question-${index}" value="__other__"><span><strong>其他</strong><small>填写自定义回答</small></span></label><input class="other-answer" data-other-for="${index}" type="${question.isSecret ? "password" : "text"}" autocomplete="off" placeholder="输入其他回答">` : ""}</div>`;
    } else {
      controls = `<textarea class="other-answer${question.isSecret ? " secret-answer" : ""}" data-free-answer="${index}" rows="3" required placeholder="输入回答"></textarea>`;
    }
    return `<fieldset class="question-block" data-question-index="${index}" data-question-id="${escapeHtml(id)}"><legend><span class="question-header">${escapeHtml(question.header || `问题 ${index + 1}`)}</span>${escapeHtml(question.question || "请输入回答")}</legend>${controls}</fieldset>`;
  }

  async function handleRequestAction(event) {
    if (!state.currentRequestKey) return;
    const openButton = event.target.closest("[data-elicitation-open]");
    if (openButton) {
      const entry = state.pendingRequests.get(state.currentRequestKey);
      const url = safeHttpsUrl(entry?.params?.url);
      if (!url) return toast("只允许打开 HTTPS 地址", "error");
      window.open(url, "_blank", "noopener,noreferrer");
      return;
    }
    const elicitationButton = event.target.closest("[data-elicitation-action]");
    if (elicitationButton) {
      await respondToRequest(state.currentRequestKey, { action: elicitationButton.dataset.elicitationAction, content: null, _meta: null }, elicitationButton);
      return;
    }
    const button = event.target.closest("[data-request-decision]");
    if (!button) return;
    await respondToRequest(state.currentRequestKey, { decision: button.dataset.requestDecision }, button);
  }

  async function submitStructuredRequest(event) {
    event.preventDefault();
    const key = state.currentRequestKey;
    const entry = state.pendingRequests.get(key);
    if (!key || !entry) return;
    const kind = event.currentTarget.dataset.requestKind;
    const button = event.submitter;
    let result;
    try {
      if (kind === "user-input") {
        const answers = {};
        const questions = Array.isArray(entry.params?.questions) ? entry.params.questions : [];
        questions.forEach((question, index) => {
          const block = $(`[data-question-index="${index}"]`, $("#requestBody"));
          let value = "";
          if (Array.isArray(question.options) && question.options.length) {
            const selected = $("input[type=radio]:checked", block);
            if (!selected) throw new Error(`请回答“${question.header || question.question}”`);
            value = selected.value === "__other__" ? $(`[data-other-for="${index}"]`, block)?.value.trim() : selected.value;
          } else {
            value = $(`[data-free-answer="${index}"]`, block)?.value.trim();
          }
          if (!value) throw new Error(`请完整回答“${question.header || question.question}”`);
          answers[question.id || `question-${index}`] = { answers: [value] };
        });
        result = { answers };
      } else if (kind === "mcp-elicitation-form") {
        result = { action: "accept", content: collectElicitationContent(entry), _meta: null };
      } else {
        try { result = JSON.parse($("#genericResponse").value); }
        catch { throw new Error("响应必须是有效的 JSON"); }
      }
      await respondToRequest(key, result, button);
    } catch (error) {
      toast(readableError(error), "error");
    }
  }

  async function respondToRequest(key, result, button) {
    setBusy(button, true, "提交中…");
    try {
      await request(`/api/requests/${encodeURIComponent(key)}/respond`, {
        method: "POST",
        body: JSON.stringify({ result }),
      });
      state.pendingRequests.delete(String(key));
      state.currentRequestKey = null;
      renderRequestCount();
      renderRequestsList();
      closeDialog($("#requestDialog"));
      loadRequests();
      toast("响应已送达工作站", "success");
    } catch (error) {
      toast(readableError(error), "error");
    } finally {
      setBusy(button, false);
    }
  }

  async function refreshStatus() {
    try {
      state.backendStatus = await request("/api/status");
      state.workspaceMode = state.backendStatus?.workspaceMode || (state.backendStatus?.allowedRoots?.length ? "restricted" : "unrestricted");
      state.workspaceRoots = Array.isArray(state.backendStatus?.allowedRoots) ? state.backendStatus.allowedRoots : [];
      applyWorkspaceScope();
      renderRawStatus();
      renderDiagnostics();
      updateConnectivity();
    } catch (error) {
      state.backendStatus = { reachable: false, error: readableError(error) };
      renderRawStatus();
      updateConnectivity();
      throw error;
    }
  }

  async function refreshRateLimits() {
    const button = $("#refreshRates");
    if (button) setBusy(button, true, "读取中…");
    try {
      state.rates = await rpc("account/rateLimits/read", {});
      renderRateLimits();
    } catch (error) {
      $("#rateLimits").innerHTML = `<div class="list-message">${escapeHtml(readableError(error))}</div>`;
    } finally {
      if (button) setBusy(button, false);
    }
  }

  function renderRateLimits() {
    const root = state.rates || {};
    const snapshots = root.rateLimitsByLimitId && typeof root.rateLimitsByLimitId === "object"
      ? Object.entries(root.rateLimitsByLimitId)
      : [[root.rateLimits?.limitId || "codex", root.rateLimits || root]];
    const valid = snapshots.filter(([, snapshot]) => snapshot && typeof snapshot === "object" && (snapshot.primary || snapshot.secondary || snapshot.credits));
    if (!valid.length) {
      $("#rateLimits").innerHTML = '<div class="list-message">暂无用量数据</div>';
      return;
    }
    $("#rateLimits").innerHTML = valid.map(([key, snapshot]) => {
      const windows = [["短窗口", snapshot.primary], ["长窗口", snapshot.secondary]].filter(([, value]) => value);
      return `<article class="rate-card"><div class="rate-card-head"><strong>${escapeHtml(snapshot.limitName || key || "Codex")}</strong><small>${escapeHtml(snapshot.planType || "")}</small></div><div class="rate-windows">${windows.map(([label, windowData]) => rateWindowHtml(label, windowData)).join("")}</div>${snapshot.credits ? `<div class="rate-credit"><span>可用额度</span><strong>${escapeHtml(creditText(snapshot.credits))}</strong></div>` : ""}</article>`;
    }).join("");
  }

  function rateWindowHtml(label, windowData) {
    const used = Math.max(0, Math.min(100, Number(windowData.usedPercent || 0)));
    const remaining = 100 - used;
    const level = remaining <= 10 ? "danger" : remaining <= 30 ? "warn" : "";
    const reset = windowData.resetsAt ? ` · ${formatResetTime(windowData.resetsAt)}` : "";
    return `<div class="rate-window"><div class="rate-row"><strong>${escapeHtml(label)}</strong><span><b>剩余 ${remaining.toFixed(0)}%</b>${escapeHtml(reset)}</span></div><progress class="rate-meter ${level}" max="100" value="${remaining}" aria-label="${escapeHtml(label)}剩余 ${remaining.toFixed(0)}%"></progress></div>`;
  }


  const CLIENT_VERSION = "0.7.1";

  function diagnosticText() {
    const backend = state.backendStatus?.backend || state.backendStatus || {};
    return [`客户端 ${CLIENT_VERSION} · 工作站 ${state.backendStatus?.receiverVersion || "未知"}`,
      `网络 ${navigator.onLine ? "在线" : "离线"} · 实时连接 ${state.sseConnected ? "已连接" : "重连中"}`,
      `最近同步 ${state.lastSyncAt ? new Date(state.lastSyncAt).toLocaleString() : "尚未同步"}`,
      `Codex ${backend.transport || "未连接"} · ${backend.connected ? "就绪" : "等待连接"}`,
      `待处理请求 ${state.pendingRequests.size} · ${window.isSecureContext ? "安全连接" : "普通 HTTP"}`,
      navigator.userAgent].join("\n");
  }

  function renderDiagnostics() {
    if (!$("#diagnostics")) return;
    $("#diagnostics").textContent = diagnosticText();
    const version = state.backendStatus?.receiverVersion;
    $("#clientUpdateNotice").hidden = !version || version === CLIENT_VERSION;
  }

  async function openResults() {
    const threadId = state.selectedId;
    showDialog($("#resultsDialog"));
    $("#resultsBody").textContent = "正在汇总本轮结果…";
    await refreshSelectedThread(true);
    if (state.selectedId !== threadId) return;
    const turn = state.selectedThread?.turns?.at(-1);
    const items = turn?.items || [];
    const messages = items.filter(item => item.type === "agentMessage");
    const answer = messages.at(-1)?.text || "本轮尚无最终回复";
    const commands = items.filter(item => item.type === "commandExecution");
    const failed = commands.filter(item => typeof item.exitCode === "number" && item.exitCode !== 0);
    const completed = commands.filter(item => item.exitCode === 0);
    const paths = [...new Set(items.filter(item => item.type === "fileChange").flatMap(item => (item.changes || []).map(change => change.path)).filter(Boolean))];
    const diff = items.filter(item => item.type === "turnDiff").map(item => item.diff || "").join("\n");
    $("#resultsBody").innerHTML = `<p class="result-state">${escapeHtml(turnStatusLabel(turn?.status || "inProgress"))} · ${completed.length} 条命令成功${failed.length ? ` · ${failed.length} 条失败` : ""}</p><div class="markdown-body">${markdownHtml(answer)}</div>${paths.length ? `<h3>变更文件 · ${paths.length}</h3>${paths.map(path => `<button type="button" class="result-path" data-result-path="${escapeHtml(path)}">${escapeHtml(path)}</button>`).join("")}` : ""}${commands.length ? `<details><summary>命令结果 · ${commands.length}</summary>${commands.map(item => `<p>${escapeHtml(item.command || "命令")} · ${item.exitCode == null ? "尚未报告退出码" : `退出码 ${item.exitCode}`}</p>`).join("")}</details>` : ""}${diff ? `<details><summary>本轮差异</summary><pre>${escapeHtml(diff)}</pre></details>` : ""}`;
  }

  function renderGoalUsage() {
    const goal = state.selectedGoal;
    if (!goal) { $("#goalUsage").textContent = "尚未设置目标"; return; }
    const values = [];
    const used = goal.tokensUsed ?? goal.tokenUsage?.totalTokens ?? goal.usage?.totalTokens;
    const elapsed = goal.timeUsedSeconds ?? goal.elapsedSeconds ?? goal.elapsedTimeSeconds;
    if (Number.isFinite(used)) values.push(`已用 ${used.toLocaleString()} tokens`);
    if (Number.isFinite(goal.tokenBudget)) values.push(`预算 ${goal.tokenBudget.toLocaleString()} tokens${Number.isFinite(used) ? ` · 剩余 ${Math.max(0, goal.tokenBudget-used).toLocaleString()}` : ""}`);
    if (Number.isFinite(elapsed)) values.push(`已运行 ${formatDuration(elapsed*1000)}`);
    $("#goalUsage").textContent = values.join(" · ") || "工作站尚未提供目标用量";
  }

  function renderRawStatus() {
    renderDiagnostics();
    $("#rawStatus").textContent = state.backendStatus ? safeStringify(state.backendStatus, 2) : "暂无数据";
  }

  function loadThemePreference() {
    let theme = "system";
    try { theme = localStorage.getItem(THEME_STORAGE_KEY) || "system"; } catch {}
    setThemePreference(theme, false);
  }

  function setThemePreference(theme, persist = true) {
    state.theme = ["light", "dark"].includes(theme) ? theme : "system";
    if (state.theme === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = state.theme;
    document.documentElement.dataset.resolvedTheme = state.theme === "system"
      ? window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"
      : state.theme;
    const select = $("#themeSelect");
    if (select) select.value = state.theme;
    if (!persist) return;
    try {
      if (state.theme === "system") localStorage.removeItem(THEME_STORAGE_KEY);
      else localStorage.setItem(THEME_STORAGE_KEY, state.theme);
    } catch {}
  }

  function loadTextSizePreference() {
    let size = 16;
    try { size = Number(localStorage.getItem(TEXT_SIZE_STORAGE_KEY)) || 16; } catch {}
    setTextSizePreference(size, false);
  }

  function setTextSizePreference(size, persist = true) {
    state.textSize = Math.max(14, Math.min(18, Math.round(Number(size) || 16)));
    document.documentElement.dataset.textSize = String(state.textSize);
    const input = $("#bodyTextSize");
    const output = $("#bodyTextSizeValue");
    if (input) input.value = String(state.textSize);
    if (output) output.textContent = `${state.textSize} px`;
    if (!persist) return;
    try { localStorage.setItem(TEXT_SIZE_STORAGE_KEY, String(state.textSize)); } catch {}
  }

  function renderConfigEditor(id) {
    const input = $("#" + id);
    const highlight = $("#" + id.replace("Source", "Highlight"));
    const language = id === "codexConfigSource" ? "toml" : "json";
    highlight.innerHTML = input.value.split("\n").map(line => `<span class="editor-line">${highlightSource(line, language) || " "}</span>`).join("");
    highlight.scrollTop = input.scrollTop;
    if (!input.parentElement.hidden) $("#configEditorLines").textContent = `${input.value.split("\n").length} 行`;
  }

  function selectConfigTab(tab) {
    const auth = tab === "auth";
    $("#configEditorPanel").hidden = auth;
    $("#authEditorPanel").hidden = !auth;
    for (const button of $$("[data-config-tab]")) {
      const selected = button.dataset.configTab === tab;
      button.setAttribute("aria-selected", String(selected)); button.tabIndex = selected ? 0 : -1;
    }
    $("#configEditorLanguage").textContent = auth ? "JSON" : "TOML";
    $("#configEditorHint").textContent = auth ? "认证内容含密钥或 OAuth 令牌，不存入浏览器。留空并保存可移除文件认证。" : "直接编辑工作站配置。保存前会校验格式并备份旧文件。";
    renderConfigEditor(auth ? "codexAuthSource" : "codexConfigSource");
  }

  async function loadCodexSettings(id = "") {
    const sequence = state.codexSettingsReadSequence = (state.codexSettingsReadSequence || 0) + 1;
    const result = await request(`/api/codex/settings${id ? `?id=${encodeURIComponent(id)}` : ""}`);
    if (!$("#codexSettingsDialog").open || sequence !== state.codexSettingsReadSequence) return;
    state.codexSettingsRevision = result.revision;
    $("#codexSettingsLocation").textContent = result.home;
    $("#codexConfigSource").value = result.config;
    $("#codexAuthSource").value = result.auth;
    renderConfigEditor("codexConfigSource"); renderConfigEditor("codexAuthSource");
    $("#codexProfile").innerHTML = '<option value="">工作站当前文件</option>' + result.profiles.map(profile => `<option value="${escapeHtml(profile.id)}">${escapeHtml(profile.name)}${profile.id === result.active ? " · 当前" : ""}</option>`).join("");
    $("#codexProfile").value = id;
    $("#codexProfileName").value = result.profiles.find(profile => profile.id === id)?.name || "";
    state.codexSettingsDirty = Boolean(id);
  }

  async function codexSettingsAction(action) {
    if (state.codexSettingsBusy) return;
    const id = $("#codexProfile").value;
    if (["apply", "delete"].includes(action) && !id) return toast("请先选择一个已保存配置", "warning");
    if (["apply", "delete"].includes(action) && state.codexSettingsDirty && !confirm("编辑内容尚未保存，继续此操作？")) return;
    if (action === "delete" && !confirm("删除这个已保存的认证配置？")) return;
    if (action === "reload" && !confirm("重载工作站 Codex？共享工作站的其他客户端也会断开重连。请确认任务已结束，未保存的编辑不会生效。")) return;
    if (action === "reload" && state.codexSettingsDirty) return toast("请先保存或重新读取配置", "warning");
    state.codexSettingsBusy = true;
    $$("button", $("#codexSettingsDialog")).forEach(button => button.disabled = true);
    try {
      await request("/api/codex/settings", { method: "POST", body: JSON.stringify({ action, id, revision: state.codexSettingsRevision, name: $("#codexProfileName").value, config: $("#codexConfigSource").value, auth: $("#codexAuthSource").value }) });
      if (action === "profile") {
        // Refresh metadata without discarding a draft of shared workstation settings.
        const config = $("#codexConfigSource").value, auth = $("#codexAuthSource").value;
        await loadCodexSettings(); $("#codexConfigSource").value = config; $("#codexAuthSource").value = auth;
        renderConfigEditor("codexConfigSource"); renderConfigEditor("codexAuthSource");
        state.codexSettingsDirty = true;
      } else await loadCodexSettings();
      $("#codexSettingsMessage").textContent = action === "reload" ? "重载已完成，正在重新连接。" : action === "profile" ? "已保存认证配置。共享设置的编辑请使用“保存到工作站”。" : action === "delete" ? "配置已删除。" : "已保存。下次启动 Codex 生效，当前任务继续运行。";
      if (action === "reload") reconnectNow();
    } catch (error) { $("#codexSettingsMessage").textContent = readableError(error); }
    finally { state.codexSettingsBusy = false; $$("button", $("#codexSettingsDialog")).forEach(button => button.disabled = false); }
  }

  function openStatusDialog() {
    renderRawStatus();
    renderRateLimits();
    renderDiagnostics();
    showDialog($("#statusDialog"));
  }

  function connectEvents() {
    disconnectEvents();
    if (!state.authenticated || !navigator.onLine) {
      updateConnectivity();
      return;
    }
    setConnectionState("connecting");
    const eventQuery = new URLSearchParams();
    if (state.lastEventId) eventQuery.set("after", state.lastEventId);
    if (state.eventInstanceId) eventQuery.set("instance", state.eventInstanceId);
    const eventsUrl = eventQuery.size ? `/api/events?${eventQuery}` : "/api/events";
    const source = new EventSource(eventsUrl, { withCredentials: true });
    state.eventSource = source;
    source.onopen = () => {
      state.sseConnected = true;
      state.reconnectAttempt = 0;
      loadRequests();
      refreshStatus().catch(() => {});
      refreshSelectedThread(true);
    };
    source.onmessage = handleSseMessage;
    source.addEventListener("reset", (event) => {
      state.lastEventId = "";
      let payload = { type: "reset" };
      if (event.data) {
        try { payload = JSON.parse(event.data); }
        catch { payload = { type: "reset" }; }
      }
      handleStreamEvent({ ...payload, type: "reset" });
    });
    source.onerror = () => {
      source.close();
      if (state.eventSource === source) state.eventSource = null;
      state.sseConnected = false;
      updateConnectivity();
      scheduleReconnect();
    };
  }

  function disconnectEvents() {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
    if (state.eventSource) state.eventSource.close();
    state.eventSource = null;
    state.sseConnected = false;
  }

  function scheduleReconnect() {
    if (!state.authenticated || !navigator.onLine || state.reconnectTimer) return;
    const delay = Math.min(15000, 900 * 2 ** state.reconnectAttempt) + Math.round(Math.random() * 450);
    state.reconnectAttempt = Math.min(6, state.reconnectAttempt + 1);
    state.reconnectTimer = setTimeout(() => {
      state.reconnectTimer = null;
      connectEvents();
    }, delay);
    setConnectionState("connecting", `将在 ${Math.ceil(delay / 1000)} 秒后重连`);
  }

  function reconnectNow() {
    state.reconnectAttempt = 0;
    connectEvents();
    refreshStatus().catch(() => {});
  }

  function handleSseMessage(event) {
    if (event.lastEventId) state.lastEventId = event.lastEventId;
    try { handleStreamEvent(JSON.parse(event.data)); }
    catch (error) { console.info("忽略无法解析的 SSE 事件", error); }
  }

  function handleStreamEvent(event) {
    if (!event || typeof event !== "object") return;
    if (event.type === "hello") {
      state.eventInstanceId = String(event.instanceId || "");
      return;
    }
    if (event.type === "notification") {
      handleNotification(event.method, event.params || {});
      return;
    }
    if (event.type === "server_request") {
      const normalized = normalizeRequest(event.request);
      if (!normalized) return;
      state.pendingRequests.set(normalized.key, normalized);
      renderRequestCount();
      renderRequestsList();
      toast(`${requestMeta(normalized.method).title} · 等待处理`, "warning", 7000);
      return;
    }
    if (event.type === "server_request_answered") {
      const key = String(event.key ?? event.request?.key ?? "");
      if (key) state.pendingRequests.delete(key);
      loadRequests();
      if (state.currentRequestKey === key) {
        state.currentRequestKey = null;
        closeDialog($("#requestDialog"));
      }
      renderRequestCount();
      renderRequestsList();
      return;
    }
    if (event.type === "backend_status") {
      const { type, ...status } = event;
      state.backendStatus = state.backendStatus?.backend ? {...state.backendStatus, backend:status} : status;
      renderRawStatus();
      updateConnectivity();
      return;
    }
    if (event.type === "reset") {
      state.lastEventId = "";
      toast("事件链路已重置，正在同步状态", "warning");
      Promise.allSettled([loadThreads(true), loadRequests(), refreshStatus(), refreshRateLimits()]);
      if (state.selectedId) selectThread(state.selectedId, { silent: true, preserveAgentNavigation: state.selectedId !== currentAgentRootId() });
    }
  }

  function handleNotification(method, params) {
    const threadId = params.threadId || params.conversationId || params.thread?.id;
    scheduleAgentPreviewRefresh(threadId);
    const trackedDelivery = state.composerDeliveries.get(threadId);
    if (trackedDelivery?.pending && params.turnId === trackedDelivery.turnId && params.item?.type === "userMessage" && sameUserMessage(trackedDelivery.pending, params.item)) {
      setComposerDelivery("accepted", "Codex 已接收引导，正在处理", threadId);

    }
    const normalizedMethod = String(method || "").replace(/[\/_-]/g, "").toLowerCase();
    if (normalizedMethod.includes("subagentactivity")) {
      const ownerThreadId = params.parentThreadId || params.senderThreadId || threadId || currentAgentRootId();
      ingestSubagentItem(ownerThreadId, { ...params, ...(params.activity || {}), type: "subAgentActivity" });
      return;
    }
    if ((method === "item/started" || method === "item/completed") && isSubagentItem(params.item)) ingestSubagentItem(threadId, params.item);
    if (method === "turn/completed") {
      for (const item of params.turn?.items || []) if (isSubagentItem(item)) ingestSubagentItem(threadId, item);
    }
    if (method === "thread/started" && params.thread) {
      if (isSubagentThread(params.thread)) {
        const parentId = threadParentId(params.thread) || currentAgentRootId();
        const rootId = state.subagents.get(parentId)?.rootId || parentId;
        mergeSubagentThread(params.thread, parentId, rootId);
        renderSubagentPanel();
      } else {
        upsertThreadList(params.thread);
      }
      renderThreadList();
    } else if (method === "thread/status/changed") {
      updateThreadStatus(threadId, params.status);
    } else if (method === "thread/name/updated") {
      updateThreadName(threadId, params.threadName ?? params.name ?? "");
    } else if (method === "thread/settings/updated" && threadId === state.selectedId) {
      const settings = params.settings || params.threadSettings || params.thread_settings || {};
      state.selectedRuntime = { ...(state.selectedRuntime || {}), ...settings };
      if (state.selectedThread) state.selectedThread.runtime = state.selectedRuntime;
      state.composerSettingsThreadId = null;
      syncComposerSettings(state.selectedThread);
      renderThreadDetail(false);
    } else if (["thread/archived", "thread/unarchived", "thread/deleted"].includes(method)) {
      loadThreads(false).catch(() => {});
    } else if (method === "thread/goal/updated" && threadId === state.selectedId) {
      state.selectedGoal = params.goal;
      renderGoalBanner();
    } else if (method === "thread/goal/cleared" && threadId === state.selectedId) {
      state.selectedGoal = null;
      renderGoalBanner();
    } else if (method === "turn/started" && threadId === state.selectedId) {
      upsertTurn(params.turn);
      state.activeTurnId = params.turn?.id || state.activeTurnId;
      updateActiveTurnControls();
      scheduleTimelineRender(true);
    } else if (method === "turn/completed" && threadId === state.selectedId) {
      upsertTurn(params.turn);
      if (state.activeTurnId === params.turn?.id) state.activeTurnId = null;
      updateActiveTurnControls();
      scheduleTimelineRender(true);
      loadThreads(false).catch(() => {});
    } else if (method === "turn/diff/updated" && threadId === state.selectedId) {
      const item = ensureItem(params.turnId, `turn-diff-${params.turnId || "active"}`, "turnDiff");
      item.diff = params.diff || "";
      scheduleTimelineRender(false);
    } else if (method === "item/commandExecution/terminalInteraction" && threadId === state.selectedId) {
      const item = ensureItem(params.turnId, params.itemId, "commandExecution");
      if (!Array.isArray(item.terminalInteractions)) item.terminalInteractions = [];
      item.terminalInteractions.push({ processId: params.processId, stdin: params.stdin || "", receivedAt: Date.now() });
      scheduleTimelineRender(false);
    } else if ((method === "item/started" || method === "item/completed") && threadId === state.selectedId) {
      if (upsertItem(params.turnId, params.item)) setComposerDelivery("accepted", "Codex 已接收引导，正在处理", threadId);
      scheduleTimelineRender(false);
    } else if (threadId === state.selectedId && method === "item/agentMessage/delta") {
      appendItemField(params.turnId, params.itemId, "agentMessage", "text", params.delta);
    } else if (threadId === state.selectedId && method === "item/plan/delta") {
      appendItemField(params.turnId, params.itemId, "plan", "text", params.delta);
    } else if (threadId === state.selectedId && method === "item/commandExecution/outputDelta") {
      appendItemField(params.turnId, params.itemId, "commandExecution", "aggregatedOutput", params.delta);
    } else if (threadId === state.selectedId && method === "item/fileChange/outputDelta") {
      appendItemField(params.turnId, params.itemId, "fileChange", "output", params.delta);
    } else if (threadId === state.selectedId && method === "item/fileChange/patchUpdated") {
      const item = ensureItem(params.turnId, params.itemId, "fileChange");
      item.changes = params.changes || [];
      scheduleTimelineRender(false);
    } else if (threadId === state.selectedId && method === "item/reasoning/summaryTextDelta") {
      appendReasoningDelta(params, "summary", params.summaryIndex);
    } else if (threadId === state.selectedId && method === "item/reasoning/textDelta") {
      appendReasoningDelta(params, "content", params.contentIndex);
    } else if (threadId === state.selectedId && method === "turn/plan/updated") {
      const item = ensureItem(params.turnId, `plan-${params.turnId}`, "plan");
      item.text = [params.explanation, ...(params.plan || []).map((step) => `${planMark(step.status)} ${step.step || step.text || ""}`)].filter(Boolean).join("\n");
      scheduleTimelineRender(false);
    } else if (method === "serverRequest/resolved") {
      const key = String(params.requestId ?? params.key ?? "");
      state.pendingRequests.delete(key);
      if (state.currentRequestKey === key) closeDialog($("#requestDialog"));
      renderRequestCount();
      renderRequestsList();
    } else if (method === "account/rateLimits/updated") {
      state.rates = params;
      renderRateLimits();
    } else if (method === "error") {
      toast(params.error?.message || params.message || "Codex 报告了一个错误", "error", 8000);
    } else if (["warning", "guardianWarning", "deprecationNotice", "configWarning", "model/rerouted", "model/verification"].includes(method)) {
      const detail = params.message || params.warning || params.reason || params.model || method;
      toast(`${method === "model/rerouted" ? "模型已重路由" : method === "model/verification" ? "模型校验" : "Codex 提示"} · ${typeof detail === "string" ? detail : safeStringify(detail)}`, "warning", 7000);
    }
  }

  function appendReasoningDelta(params, field, index = 0) {
    const item = ensureItem(params.turnId, params.itemId, "reasoning");
    if (!Array.isArray(item[field])) item[field] = [];
    item[field][index || 0] = (item[field][index || 0] || "") + (params.delta || "");
    scheduleTimelineRender(false);
  }

  function appendItemField(turnId, itemId, type, field, delta) {
    const item = ensureItem(turnId, itemId, type);
    item[field] = (item[field] || "") + (delta || "");
    scheduleTimelineRender(false);
  }

  function ensureItem(turnId, itemId, type) {
    let turn = findTurn(turnId);
    if (!turn) {
      turn = { id: turnId || `turn-${Date.now()}`, items: [], status: "inProgress", error: null };
      if (!Array.isArray(state.selectedThread.turns)) state.selectedThread.turns = [];
      state.selectedThread.turns.push(turn);
    }
    if (!Array.isArray(turn.items)) turn.items = [];
    let item = turn.items.find((candidate) => candidate.id === itemId);
    if (!item) {
      item = { id: itemId || `item-${Date.now()}`, type };
      if (type === "reasoning") { item.summary = []; item.content = []; }
      if (type === "fileChange") item.changes = [];
      turn.items.push(item);
    }
    return item;
  }

  function upsertItem(turnId, item) {
    if (!item) return false;
    const turn = findTurn(turnId);
    let acceptedPending = false;
    if (turn && item.type === "userMessage") {
      acceptedPending = (turn.items || []).some((candidate) => candidate.remotePending && sameUserMessage(candidate, item));
      turn.items = (turn.items || []).filter((candidate) => !candidate.remotePending || !sameUserMessage(candidate, item));
    }
    const target = ensureItem(turnId, item.id, item.type);
    Object.assign(target, mergeTimelineItem(target, item));
    delete target.remotePending;
    if (acceptedPending) target.deliveryState = "accepted";
    return acceptedPending;
  }

  function upsertTurn(turn) {
    if (!turn || !state.selectedThread) return;
    if (!Array.isArray(state.selectedThread.turns)) state.selectedThread.turns = [];
    const index = state.selectedThread.turns.findIndex((entry) => entry.id === turn.id);
    if (index >= 0) {
      const previous = state.selectedThread.turns[index];
      const next = { ...previous, ...turn };
      if (Array.isArray(turn.items)) {
        // Turn notifications and turn/start responses can contain only a subset of items.
        next.items = [...(previous.items || [])];
        for (const item of turn.items) {
          const itemIndex = next.items.findIndex((candidate) => candidate.id === item.id ||
            (candidate.remotePending && item.type === "userMessage" && sameUserMessage(candidate, item)));
          if (itemIndex < 0) next.items.push(item);
          else next.items[itemIndex] = mergeTimelineItem(next.items[itemIndex], item);
        }
      }
      state.selectedThread.turns[index] = next;
    }
    else state.selectedThread.turns.push(turn);
  }

  function mergeLiveThreadSnapshot(previous, latest) {
    if (!previous || !Array.isArray(latest?.turns)) return latest;
    const next = { ...latest, turns: latest.turns.map((turn) => ({ ...turn, items: [...(turn.items || [])] })) };
    for (const previousTurn of previous.turns || []) {
      const turn = next.turns.find((candidate) => candidate.id === previousTurn.id);
      if (!turn) {
        if (previousTurn.status === "inProgress") next.turns.push(previousTurn);
        continue;
      }
      const active = (turn.status || previousTurn.status) === "inProgress";
      const snapshotItems = [...turn.items];
      const consumed = new Set();
      let insertionIndex = 0;
      // Snapshots define history order; retain live-only items beside their preceding anchor.
      for (const liveItem of previousTurn.items || []) {
        const index = snapshotItems.findIndex((item, itemIndex) => !consumed.has(itemIndex) && sameTimelineItem(item, liveItem));
        if (index < 0) {
          if (snapshotItems.some((item) => sameTimelineItem(item, liveItem))) continue;
          if (active || !["userMessage", "agentMessage"].includes(liveItem.type)) turn.items.splice(insertionIndex++, 0, liveItem);
          continue;
        }
        consumed.add(index);
        insertionIndex = turn.items.indexOf(snapshotItems[index]);
        turn.items[insertionIndex] = mergeTimelineItem(liveItem, snapshotItems[index]);
        insertionIndex++;
      }
    }
    return next;
  }

  function sameTimelineItem(left, right) {
    return Boolean(left?.id && right?.id && left.id === right.id) || sameTimelineMessage(left, right);
  }

  function mergeTimelineItem(liveItem, snapshotItem) {
    const item = { ...liveItem, ...snapshotItem };
    for (const field of ["text", "aggregatedOutput", "output", "diff"]) {
      if (String(liveItem[field] || "").length > String(snapshotItem[field] || "").length) item[field] = liveItem[field];
    }
    if (item.type === "reasoning") {
      for (const field of ["summary", "content"]) {
        if (contentSize(liveItem[field]) > contentSize(snapshotItem[field])) item[field] = liveItem[field];
      }
    }
    if (contentSize(liveItem.terminalInteractions) > contentSize(snapshotItem.terminalInteractions)) item.terminalInteractions = liveItem.terminalInteractions;
    if (liveItem.remotePending && item.type === "userMessage") {
      delete item.remotePending;
      item.deliveryState = "accepted";
    }
    return item;
  }

  function contentSize(value) {
    if (!value) return 0;
    try { return JSON.stringify(value).length; }
    catch { return String(value).length; }
  }

  function sameTimelineMessage(left, right) {
    if (left?.type !== right?.type) return false;
    if (left.type === "userMessage") return sameUserMessage(left, right);
    return left.type === "agentMessage" && Boolean(left.text || right.text) && left.text === right.text;
  }

  function sameUserMessage(left, right) {
    const normalize = (part) => {
      if (["text", "input_text"].includes(part?.type)) return ["text", part.text || ""];
      if (["image", "input_image", "localImage"].includes(part?.type)) return ["image", part.url || part.image_url || part.path || ""];
      return [part?.type || "", ""];
    };
    const leftParts = (left?.content || []).map(normalize);
    const rightParts = (right?.content || []).map(normalize);
    return leftParts.length === rightParts.length && leftParts.every((part, index) => part[0] === rightParts[index][0] && part[1] === rightParts[index][1]);
  }

  function threadProgressKey(thread) {
    const turns = thread?.turns || [];
    const turn = turns.at(-1);
    const tail = (value) => `${String(value || "").length}:${String(value || "").slice(-80)}`;
    const itemKey = (item) => [
      item.id, item.type, item.status,
      tail(item.text), tail(item.aggregatedOutput), tail(item.output), tail(item.diff),
      ...(item.summary || []).map(tail), ...(item.content || []).filter((part) => typeof part === "string").map(tail),
      ...(item.changes || []).map((change) => `${change.kind || ""}:${change.path || ""}:${tail(change.diff)}`),
      item.terminalInteractions?.length || 0,
    ].join("~");
    return [thread?.name, thread?.preview, statusType(thread?.status), JSON.stringify(thread?.runtime || {}), turns.length, turn?.id, turn?.status, turn?.error?.message, ...(turn?.items || []).filter((item) => !item.remotePending).map(itemKey)].join("|");
  }

  function findTurn(turnId) {
    return state.selectedThread?.turns?.find((turn) => turn.id === turnId);
  }

  function findItem(itemId) {
    return findItemInThread(state.selectedThread, itemId);
  }

  function findItemInThread(thread, itemId) {
    for (const turn of thread?.turns || []) {
      const item = turn.items?.find((candidate) => candidate.id === itemId);
      if (item) return item;
    }
    return null;
  }

  function findActiveTurn(thread) {
    return [...(thread?.turns || [])].reverse().find((turn) => turn.status === "inProgress") || null;
  }

  function updateThreadStatus(threadId, status) {
    const thread = state.threads.find((entry) => entry.id === threadId);
    if (thread) thread.status = status;
    const agent = state.subagents.get(threadId);
    if (agent) {
      agent.status = status;
      if (agent.thread) agent.thread.status = status;
      agent.updatedAt = Date.now();
      renderSubagentPanel();
      renderAgentContext();
    }
    if (state.selectedId === threadId && state.selectedThread) {
      state.selectedThread.status = status;
      renderThreadDetail(false);
    }
    renderThreadList();
  }

  function updateThreadName(threadId, name) {
    const thread = state.threads.find((entry) => entry.id === threadId);
    if (thread) thread.name = name;
    const agent = state.subagents.get(threadId);
    if (agent) {
      agent.name = name;
      if (agent.thread) agent.thread.name = name;
      renderSubagentPanel();
      renderAgentContext();
    }
    if (state.selectedId === threadId && state.selectedThread) {
      state.selectedThread.name = name;
      renderThreadDetail(false);
    }
    renderThreadList();
  }

  function upsertThreadList(thread) {
    const index = state.threads.findIndex((entry) => entry.id === thread.id);
    if (index >= 0) state.threads[index] = { ...state.threads[index], ...thread };
    else state.threads.unshift(thread);
  }

  function mergeSelectedIntoList() {
    if (!state.selectedThread) return;
    upsertThreadList(state.selectedThread);
    renderThreadList();
  }

  function updateConnectivity() {
    if (!navigator.onLine) {
      setConnectionState("offline", "设备当前离线");
      return;
    }
    if (!state.sseConnected) {
      setConnectionState("connecting", "实时事件链路正在重连");
      return;
    }
    const backendConnected = deepBoolean(state.backendStatus, ["connected", "online", "ready", "codexConnected", "backend.connected", "codex.connected", "status.connected"]);
    if (backendConnected === false) {
      setConnectionState("offline", state.backendStatus?.message || state.backendStatus?.error || "Codex 后端未连接");
    } else if (state.syncError) {
      setConnectionState("connecting", "会话同步暂时失败，已保留现有内容，正在重试");
    } else {
      const synced = state.lastSyncAt ? ` · 最近同步 ${new Date(state.lastSyncAt).toLocaleTimeString()}` : "";
      setConnectionState("online", `实时事件链路已连接${synced}`);
    }
  }

  function setConnectionState(connectionState, detail = "") {
    const labels = { online: "在线", offline: "离线", connecting: "连接中" };
    $("#statusButton").dataset.state = connectionState;
    $("#connectionText").textContent = labels[connectionState];
    const hero = $(".status-hero");
    hero.dataset.state = connectionState;
    $("#statusHeroTitle").textContent = connectionState === "online" ? "工作站在线" : connectionState === "offline" ? "链路中断" : "正在连接";
    $("#statusHeroDetail").textContent = detail || labels[connectionState];
  }

  function showThreadRoute() {
    $("#appView").classList.remove("mobile-detail");
    closeAllDialogs();
    if (location.hash.startsWith("#thread=")) history.replaceState({}, "", `${location.pathname}${location.search}`);
  }

  function showDialog(dialog) {
    if (!dialog || dialog.open) return;
    try { dialog.showModal(); }
    catch { dialog.setAttribute("open", ""); }
  }

  function closeDialog(dialog) {
    if (!dialog?.open) return;
    if (dialog.id === "codexSettingsDialog" && state.authenticated) {
      if (state.codexSettingsBusy) return;
      if (state.codexSettingsDirty && !confirm("认证配置有未保存的编辑，放弃并关闭？")) return;
    }
    try { dialog.close(); }
    catch { dialog.removeAttribute("open"); }
  }

  function closeAllDialogs() {
    $$("dialog[open]").forEach(closeDialog);
  }

  function setBusy(button, busy, busyText = "处理中…") {
    if (!button) return;
    if (busy) {
      button.disabled = true;
      button.dataset.busyLabel = busyText;
      button.setAttribute("aria-busy", "true");
    } else {
      button.disabled = false;
      delete button.dataset.busyLabel;
      button.removeAttribute("aria-busy");
    }
  }

  function toast(message, type = "info", duration = 4200) {
    const element = document.createElement("div");
    element.className = `toast ${type}`;
    const span = document.createElement("span");
    span.textContent = message;
    element.append(span);
    $("#toastRegion").append(element);
    setTimeout(() => element.remove(), duration);
  }

  function normalizeArray(value, keys) {
    if (Array.isArray(value)) return value;
    for (const key of keys) if (Array.isArray(value?.[key])) return value[key];
    return [];
  }

  function threadTitle(thread) {
    if (!thread) return "未命名会话";
    return thread.name || firstLine(thread.preview) || `会话 ${compactId(thread.id)}`;
  }

  function runtimeModel(thread) {
    return state.selectedRuntime?.model || thread?.model || thread?.settings?.model || thread?.modelProvider || "CODEX";
  }

  function statusType(status) {
    if (!status) return "notLoaded";
    return typeof status === "string" ? status : status.type || "notLoaded";
  }

  function statusLabel(status, rawStatus) {
    const labels = { active: "工作中", idle: "待机", notLoaded: "未加载", systemError: "系统错误" };
    if (status === "active" && rawStatus?.activeFlags?.includes("waitingOnApproval")) return "等待审批";
    if (status === "active" && rawStatus?.activeFlags?.includes("waitingOnUserInput")) return "等待输入";
    return labels[status] || status;
  }

  function turnStatusLabel(status) {
    return ({ completed: "完成", inProgress: "运行中", interrupted: "已中断", failed: "失败" })[status] || status;
  }

  function commandStatusLabel(status) {
    return ({ completed: "完成", inProgress: "运行中", failed: "失败", declined: "已拒绝" })[status] || status || "COMMAND";
  }

  function requestMeta(method) {
    if (method === "mcpServer/elicitation/request") return { glyph: "MCP", kicker: "MCP ELICITATION", title: "MCP 服务需要输入" };
    if (method === "item/commandExecution/requestApproval" || method === "execCommandApproval") return { glyph: ">_", kicker: "COMMAND APPROVAL", title: "命令执行审批" };
    if (method === "item/fileChange/requestApproval" || method === "applyPatchApproval") return { glyph: "±", kicker: "FILE APPROVAL", title: "文件变更审批" };
    if (method === "item/tool/requestUserInput") return { glyph: "?", kicker: "USER INPUT", title: "Codex 需要你的回答" };
    if (method === "item/permissions/requestApproval") return { glyph: "!", kicker: "PERMISSION REQUEST", title: "权限范围请求" };
    return { glyph: "··", kicker: "SERVER REQUEST", title: "服务端请求" };
  }

  function sourceLabel(source) {
    if (!source) return "CODEX";
    if (typeof source === "string") return source;
    return source.type || source.kind || "CODEX";
  }

  function pathText(value) {
    if (!value) return "";
    if (typeof value === "string") return value;
    return value.path || value.value || value.display || safeStringify(value);
  }

  function shortPath(path) {
    if (!path || path.length <= 38) return path;
    const parts = path.split("/").filter(Boolean);
    return `…/${parts.slice(-2).join("/")}`;
  }

  function firstLine(value) {
    return String(value || "").split(/\r?\n/, 1)[0].trim();
  }

  function compactId(value) {
    if (value === null || value === undefined || value === "") return "";
    const text = String(value);
    return text.length > 13 ? `${text.slice(0, 8)}…${text.slice(-4)}` : text;
  }

  function relativeTime(epoch) {
    if (!epoch) return "—";
    const milliseconds = Number(epoch) < 1e12 ? Number(epoch) * 1000 : Number(epoch);
    const difference = Date.now() - milliseconds;
    if (difference < 60_000) return "刚刚";
    if (difference < 3_600_000) return `${Math.floor(difference / 60_000)}m`;
    if (difference < 86_400_000) return `${Math.floor(difference / 3_600_000)}h`;
    if (difference < 7 * 86_400_000) return `${Math.floor(difference / 86_400_000)}d`;
    return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit" }).format(milliseconds);
  }

  function formatResetTime(epoch) {
    const milliseconds = Number(epoch) < 1e12 ? Number(epoch) * 1000 : Number(epoch);
    return `重置 ${new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(milliseconds)}`;
  }

  function formatDuration(milliseconds) {
    const seconds = Math.round(Number(milliseconds) / 1000);
    return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  }

  function formatBytes(bytes) {
    const size = Math.max(0, Number(bytes) || 0);
    return size < 1024 ? `${size} B` : size < 1024 * 1024 ? `${(size / 1024).toFixed(size < 10240 ? 1 : 0)} KiB` : `${(size / 1024 / 1024).toFixed(1)} MiB`;
  }

  function creditText(credits) {
    if (credits.balance !== undefined) return String(credits.balance);
    if (credits.hasCredits === false) return "无可用额度";
    return shorten(safeStringify(credits), 60);
  }

  function readableError(error) {
    if (!error) return "发生未知错误";
    if (/^(Failed to fetch|Load failed|NetworkError)/i.test(error.message || "")) return "网络连接中断";
    return error.message || String(error);
  }

  function safeStringify(value, spacing = 0) {
    try {
      return JSON.stringify(value, (_key, child) => typeof child === "bigint" ? child.toString() : child, spacing);
    } catch {
      return String(value);
    }
  }

  function escapeHtml(value) {
    return String(value ?? "").replace(/[&<>'"]/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[character]);
  }

  function shorten(value, length) {
    const text = String(value || "");
    return text.length > length ? `${text.slice(0, length - 1)}…` : text;
  }

  function findLastIndex(array, predicate) {
    for (let index = array.length - 1; index >= 0; index -= 1) if (predicate(array[index], index)) return index;
    return -1;
  }

  function planMark(status) {
    return status === "completed" ? "✓" : status === "in_progress" || status === "inProgress" ? "→" : "·";
  }

  function changeKindLabel(kind) {
    const type = typeof kind === "string" ? kind : kind?.type;
    return ({ add: "ADD", delete: "DELETE", update: kind?.move_path ? "MOVE" : "UPDATE" })[type] || "EDIT";
  }

  function deepBoolean(object, paths) {
    for (const path of paths) {
      let value = object;
      for (const segment of path.split(".")) value = value?.[segment];
      if (typeof value === "boolean") return value;
    }
    return undefined;
  }
})();
