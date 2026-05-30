"use strict";

// ---------- tiny helpers ----------
const $ = (id) => document.getElementById(id);
const esc = (s) =>
  String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
const splitList = (s) => (s || "").split(",").map((x) => x.trim()).filter(Boolean);
const joinList = (a) => (a || []).join(", ");
const intNum = (v, d = 0) => { const n = parseInt(v, 10); return isNaN(n) ? d : n; };
const fl = (v, d = 0) => { const n = parseFloat(v); return isNaN(n) ? d : n; };

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

let toastTimer = null;
function toast(msg, kind = "") {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast " + kind;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 3400);
}

const TRACK_STATUSES = ["ready", "applied", "interviewing", "offer", "rejected", "skipped"];

// ---------- state ----------
let CFG = null;
let SOURCES = [];
let RECORDS = [];
let modalId = null;
let modalDoc = "resume";
let modalEdit = null;   // working copy of the open application
let modalDirty = false; // unsaved edits in the modal

// ---------- navigation ----------
function switchView(name) {
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.view === name));
  document.querySelectorAll(".view").forEach((v) => v.classList.toggle("active", v.id === "view-" + name));
}

// ---------- config ----------
async function loadConfig() {
  CFG = await api("GET", "/api/config");
  renderConfig();
}

function renderConfig() {
  const c = CFG.candidate || {};
  $("p_name").value = c.name || "";
  $("p_headline").value = c.headline || "";
  $("p_email").value = c.email || "";
  $("p_phone").value = c.phone || "";
  $("p_location").value = c.location || "";
  $("p_linkedin").value = c.linkedin || "";
  $("p_github").value = c.github || "";
  $("p_website").value = c.website || "";
  $("p_skills").value = joinList(c.skills);
  $("p_summary").value = c.summary || "";
  $("p_resume").value = c.baseResume || "";

  const f = CFG.focus || {};
  const loc = f.location || {};
  $("f_interest").value = f.interest || "";
  $("f_city").value = loc.city || "";
  $("f_state").value = loc.state || "";
  $("f_zip").value = loc.zip || "";
  $("f_radius").value = loc.radiusMiles != null ? loc.radiusMiles : 25;
  $("f_remote").checked = !!f.includeRemote;
  $("f_minsalary").value = f.minSalary || 0;
  $("f_exclude").value = joinList(f.excludeKeywords);
  $("f_max").value = f.maxResultsPerSource || 25;
  $("f_minprescreen").value = f.minPrescreenScore != null ? f.minPrescreenScore : 45;
  $("f_minscore").value = f.minMatchScore || 0;
  renderSources();
  renderBrowserSearch();

  // Home (simple mode) mirrors the focus location.
  if ($("home_city")) {
    $("home_city").value = loc.city || "";
    $("home_state").value = loc.state || "";
    $("home_zip").value = loc.zip || "";
    $("home_radius").value = loc.radiusMiles != null ? loc.radiusMiles : 25;
    $("home_remote").checked = f.includeRemote !== false;
  }
  renderHomeParsed();

  const a = CFG.ai || {};
  $("ai_active").value = a.active || "anthropic";
  fillProvider("anthropic", a.anthropic);
  fillProvider("google", a.google);
  fillProvider("deepseek", a.deepseek);
  fillProvider("local", a.local);
  updateProviderDim();

  const s = CFG.sources || {};
  $("adzuna_id").value = (s.adzuna && s.adzuna.appId) || "";
  $("adzuna_key").value = (s.adzuna && s.adzuna.appKey) || "";
  $("usajobs_key").value = (s.usajobs && s.usajobs.apiKey) || "";
  $("usajobs_email").value = (s.usajobs && s.usajobs.email) || "";

  const ap = CFG.apply || {};
  $("ap_channel").value = ap.channel || "review";
  $("ap_auto").checked = !!ap.autoMode;
  $("ap_autoapply").checked = !!ap.autoApply;
  $("ap_interval").value = ap.intervalMinutes || 60;
  $("ap_max").value = ap.maxAppliesPerRun || 5;
  $("ap_exportdir").value = ap.exportDir || "";
  const sm = ap.smtp || {};
  $("smtp_host").value = sm.host || "";
  $("smtp_port").value = sm.port || 587;
  $("smtp_user").value = sm.username || "";
  $("smtp_pass").value = sm.password || "";
  $("smtp_from").value = sm.from || "";
}

function fillProvider(id, p) {
  p = p || {};
  if ($(id + "_key")) $(id + "_key").value = p.apiKey || "";
  if ($(id + "_model")) $(id + "_model").value = p.model || "";
  if ($(id + "_url")) $(id + "_url").value = p.baseUrl || "";
  if ($(id + "_temp")) $(id + "_temp").value = p.temperature != null ? p.temperature : 0.7;
  if ($(id + "_max")) $(id + "_max").value = p.maxTokens || 4096;
}

function readProvider(id) {
  const p = {
    apiKey: $(id + "_key") ? $(id + "_key").value.trim() : "",
    model: $(id + "_model") ? $(id + "_model").value.trim() : "",
    temperature: fl($(id + "_temp") ? $(id + "_temp").value : 0.7, 0.7),
    maxTokens: intNum($(id + "_max") ? $(id + "_max").value : 4096, 4096),
  };
  if ($(id + "_url")) p.baseUrl = $(id + "_url").value.trim();
  return p;
}

function renderSources() {
  const enabled = new Set((CFG.focus && CFG.focus.sources) || []);
  $("sourceList").innerHTML = SOURCES.map(
    (s) => `<label><input type="checkbox" value="${esc(s.id)}" ${enabled.has(s.id) ? "checked" : ""}/> ${esc(s.name)}${s.needsCredentials ? ' <span class="hint">★ needs key</span>' : ""}</label>`
  ).join("");
}

function renderBrowserSearch() {
  const b = (CFG.sources && CFG.sources.browser) || {};
  const boards = new Set(b.boards && b.boards.length ? b.boards : ["indeed", "linkedin"]);
  document.querySelectorAll("#browserBoards input[data-bb]").forEach((i) => { i.checked = boards.has(i.value); });
  if ($("bb_headful")) $("bb_headful").checked = !!b.headful;
  if ($("bb_screens")) $("bb_screens").value = b.maxScreens || 3;
}

function updateProviderDim() {
  const active = $("ai_active").value;
  document.querySelectorAll(".provider-block").forEach((b) =>
    b.classList.toggle("dim", b.dataset.provider !== active)
  );
}

function collectConfig() {
  CFG.candidate = {
    name: $("p_name").value.trim(), headline: $("p_headline").value.trim(),
    email: $("p_email").value.trim(), phone: $("p_phone").value.trim(),
    location: $("p_location").value.trim(), linkedin: $("p_linkedin").value.trim(),
    github: $("p_github").value.trim(), website: $("p_website").value.trim(),
    skills: splitList($("p_skills").value), summary: $("p_summary").value,
    baseResume: $("p_resume").value,
  };
  CFG.focus = {
    interest: $("f_interest").value.trim(),
    location: {
      city: $("f_city").value.trim(), state: $("f_state").value.trim(),
      zip: $("f_zip").value.trim(), radiusMiles: intNum($("f_radius").value, 25),
    },
    includeRemote: $("f_remote").checked,
    minSalary: intNum($("f_minsalary").value, 0),
    excludeKeywords: splitList($("f_exclude").value),
    sources: Array.from(document.querySelectorAll("#sourceList input:checked")).map((i) => i.value),
    maxResultsPerSource: intNum($("f_max").value, 25),
    minPrescreenScore: intNum($("f_minprescreen").value, 0),
    minMatchScore: intNum($("f_minscore").value, 0),
  };
  CFG.ai = {
    active: $("ai_active").value,
    anthropic: readProvider("anthropic"), google: readProvider("google"),
    deepseek: readProvider("deepseek"), local: readProvider("local"),
  };
  // Preserve any fields we don't edit here (notably captured `accounts`).
  CFG.sources = Object.assign({}, CFG.sources, {
    adzuna: { appId: $("adzuna_id").value.trim(), appKey: $("adzuna_key").value.trim() },
    usajobs: { apiKey: $("usajobs_key").value.trim(), email: $("usajobs_email").value.trim() },
    browser: {
      boards: Array.from(document.querySelectorAll("#browserBoards input[data-bb]:checked")).map((i) => i.value),
      headful: $("bb_headful") ? $("bb_headful").checked : false,
      maxScreens: intNum($("bb_screens") ? $("bb_screens").value : 3, 3),
    },
  });
  CFG.apply = {
    channel: $("ap_channel").value, autoMode: $("ap_auto").checked, autoApply: $("ap_autoapply").checked,
    intervalMinutes: intNum($("ap_interval").value, 60), maxAppliesPerRun: intNum($("ap_max").value, 5),
    exportDir: $("ap_exportdir").value.trim(),
    smtp: {
      host: $("smtp_host").value.trim(), port: intNum($("smtp_port").value, 587),
      username: $("smtp_user").value.trim(), password: $("smtp_pass").value, from: $("smtp_from").value.trim(),
    },
  };
}

async function saveConfig() {
  collectConfig();
  try { await api("PUT", "/api/config", CFG); toast("Saved", "ok"); loadStatus(); }
  catch (e) { toast("Save failed: " + e.message, "err"); }
}

async function loadSources() {
  try { SOURCES = await api("GET", "/api/sources"); } catch { SOURCES = []; }
  renderSources();
}

// ---------- AI test ----------
async function testProvider(provider) {
  const el = $("test_" + provider);
  el.textContent = "testing…"; el.className = "test-result";
  collectConfig();
  try {
    await api("PUT", "/api/config", CFG);
    const r = await api("POST", "/api/ai/test", { provider });
    if (r.ok) { el.textContent = "✓ " + (r.name || "ok") + (r.sample ? ` — “${r.sample.slice(0, 40)}”` : ""); el.className = "test-result ok"; }
    else { el.textContent = "✗ " + (r.error || "failed"); el.className = "test-result err"; }
  } catch (e) { el.textContent = "✗ " + e.message; el.className = "test-result err"; }
}

// ---------- jobs ----------
async function loadJobs() {
  try { RECORDS = await api("GET", "/api/jobs"); } catch { RECORDS = []; }
  renderJobs();
  if (modalId) refreshModalFromData();
}

function scoreClass(n) { return n >= 75 ? "score-high" : n >= 50 ? "score-mid" : "score-low"; }

function renderJobs() {
  const filter = $("jobFilter").value;
  const rows = RECORDS.filter((r) => {
    const st = r.application ? r.application.status : "discovered";
    return !filter || st === filter;
  });
  $("navJobCount").textContent = RECORDS.length ? RECORDS.length : "";
  $("jobsEmpty").hidden = RECORDS.length > 0;

  $("jobList").innerHTML = rows.map((r) => {
    const app = r.application;
    const st = app ? app.status : "discovered";
    const hasScore = app && app.resume;
    const score = hasScore
      ? `<div class="job-score ${scoreClass(app.matchScore)}">${app.matchScore}<small>match</small></div>`
      : (app && app.prescreenScore
          ? `<div class="job-score ${scoreClass(app.prescreenScore)}">${app.prescreenScore}<small>fit</small></div>`
          : `<div class="job-score score-low">—</div>`);
    const salary = r.job.salary ? `<span>· 💵 ${esc(r.job.salary)}</span>` : "";
    const remote = r.job.remote ? `<span>· 🏠 remote</span>` : "";
    return `<div class="job-row" data-id="${esc(r.job.id)}">
      <div class="job-main">
        <div class="job-title">${esc(r.job.title)}</div>
        <div class="job-meta">
          <span>${esc(r.job.company || "—")}</span>
          <span>· ${esc(r.job.location || "—")}</span>
          ${remote}${salary}
          <span class="src">· ${esc(r.job.source)}</span>
        </div>
      </div>
      ${score}
      <span class="badge ${st}">${st}</span>
    </div>`;
  }).join("");

  document.querySelectorAll(".job-row").forEach((row) =>
    row.addEventListener("click", () => openModal(row.dataset.id))
  );
}

// ---------- modal ----------
function recordById(id) { return RECORDS.find((r) => r.job.id === id); }

function openModal(id) {
  modalId = id;
  modalDoc = "resume";
  const r = recordById(id);
  const app = (r && r.application) || {};
  modalEdit = { status: app.status || "", notes: app.notes || "", resume: app.resume || "", coverLetter: app.coverLetter || "" };
  modalDirty = false;
  renderModal();
  $("modal").hidden = false;
}

function closeModal() { modalId = null; modalEdit = null; $("modal").hidden = true; }

// Re-sync the modal from fresh data unless the user has unsaved edits.
function refreshModalFromData() {
  if (modalDirty) { renderModal(); return; }
  const r = recordById(modalId);
  if (!r) { closeModal(); return; }
  const app = r.application || {};
  modalEdit = { status: app.status || "", notes: app.notes || "", resume: app.resume || "", coverLetter: app.coverLetter || "" };
  renderModal();
}

function renderModal() {
  const r = recordById(modalId);
  if (!r) { closeModal(); return; }
  const app = r.application || {};
  const st = app.status || "discovered";
  const gen = st === "generating";
  const hasDocs = !!(modalEdit.resume || modalEdit.coverLetter);
  const official = (r.job.applyUrl || "").trim();
  const channelNote = { review: "mark applied (you submit manually)", export: "save files to your export folder", email: "email it to the posting" }[(CFG.apply && CFG.apply.channel) || "review"];

  let boxes = "";
  if (official) boxes += `<div class="match-box"><b>🏢 Official application:</b> <a href="${esc(official)}" target="_blank" rel="noopener">${esc(official)}</a></div>`;
  if (!hasDocs && (app.prescreenScore || app.prescreenReason)) boxes += `<div class="match-box"><b>🧪 Relevance ${app.prescreenScore || 0}/100.</b> ${esc(app.prescreenReason || "")}</div>`;
  if (hasDocs) boxes += `<div class="match-box"><b>Match ${app.matchScore || 0}/100.</b> ${esc(app.matchReason || "")}</div>`;
  if (app.error) boxes += `<div class="match-box err"><b>Error:</b> ${esc(app.error)}</div>`;
  if (app.strengths && app.strengths.length)
    boxes += `<div class="insight ok"><b>✓ Strengths</b><ul>${app.strengths.map((x) => `<li>${esc(x)}</li>`).join("")}</ul></div>`;
  if (app.gaps && app.gaps.length)
    boxes += `<div class="insight warn"><b>⚠ Gaps to address</b><ul>${app.gaps.map((x) => `<li>${esc(x)}</li>`).join("")}</ul></div>`;

  const salary = r.job.salary ? ` · 💵 ${esc(r.job.salary)}` : "";
  const docArea = modalDoc === "desc"
    ? `<div class="doc-content">${esc(r.job.description)}</div>`
    : `<textarea class="doc-edit" id="docEdit">${esc(modalDoc === "cover" ? modalEdit.coverLetter : modalEdit.resume)}</textarea>
       <div class="doc-tools"><button class="btn tiny" id="m_copy">📋 Copy</button><span class="hint">edits are saved with the button below</span></div>`;

  const statusOpts = TRACK_STATUSES.map((s) => `<option value="${s}" ${modalEdit.status === s ? "selected" : ""}>${s}</option>`).join("");

  $("modalBody").innerHTML = `
    <h2>${esc(r.job.title)}</h2>
    <div class="sub">${esc(r.job.company || "")} · ${esc(r.job.location || "")}${r.job.remote ? " · 🏠 remote" : ""}${salary} · via ${esc(r.job.source)} · <span class="badge ${st}">${st}</span></div>

    <div class="modal-actions">
      <button class="btn" id="m_open">↗ Open posting</button>
      <button class="btn primary" id="m_gen" ${gen ? "disabled" : ""}>${gen ? "⏳ Generating…" : (hasDocs ? "↻ Regenerate" : "✨ Generate application")}</button>
      ${hasDocs ? `<button class="btn" id="m_apply">📨 Apply (${esc(channelNote)})</button>` : ""}
      ${official ? `<button class="btn" id="m_official">🏢 Open official application</button>` : ""}
      <button class="btn" id="m_findofficial">🔎 Find official apply page</button>
    </div>

    ${boxes}

    ${hasDocs ? `<div class="refine">
      <input type="text" id="refineInput" placeholder="Refine: e.g. ‘make it more concise’, ‘emphasize leadership’…" />
      <button class="btn" id="m_refine">↻ Refine</button>
    </div>` : ""}

    <div class="doc-tabs">
      <button class="doc-tab ${modalDoc === "resume" ? "active" : ""}" data-doc="resume">Résumé</button>
      <button class="doc-tab ${modalDoc === "cover" ? "active" : ""}" data-doc="cover">Cover letter</button>
      <button class="doc-tab ${modalDoc === "desc" ? "active" : ""}" data-doc="desc">Job description</button>
    </div>
    ${docArea}

    <div class="track">
      <label>Status <select id="trackStatus" class="select">${statusOpts}</select></label>
      <label class="grow">Notes <input type="text" id="trackNotes" value="${esc(modalEdit.notes)}" placeholder="e.g. phone screen on Friday" /></label>
      <button class="btn primary" id="m_save">💾 Save</button>
    </div>
  `;

  $("m_open").addEventListener("click", () => window.open(r.job.url, "_blank"));
  $("m_gen").addEventListener("click", () => generate(""));
  if ($("m_apply")) $("m_apply").addEventListener("click", applyCurrent);
  if ($("m_official")) $("m_official").addEventListener("click", () => window.open(official, "_blank"));
  $("m_findofficial").addEventListener("click", findOfficial);
  if ($("m_refine")) $("m_refine").addEventListener("click", () => generate($("refineInput").value.trim()));
  if ($("m_copy")) $("m_copy").addEventListener("click", () => {
    const text = modalDoc === "cover" ? modalEdit.coverLetter : modalEdit.resume;
    navigator.clipboard.writeText(text).then(() => toast("Copied", "ok"), () => toast("Copy failed", "err"));
  });
  if ($("docEdit")) $("docEdit").addEventListener("input", (e) => {
    modalDirty = true;
    if (modalDoc === "cover") modalEdit.coverLetter = e.target.value; else modalEdit.resume = e.target.value;
  });
  $("trackStatus").addEventListener("change", (e) => { modalEdit.status = e.target.value; modalDirty = true; });
  $("trackNotes").addEventListener("input", (e) => { modalEdit.notes = e.target.value; modalDirty = true; });
  $("m_save").addEventListener("click", saveApplication);
  document.querySelectorAll(".doc-tab").forEach((t) =>
    t.addEventListener("click", () => { modalDoc = t.dataset.doc; renderModal(); })
  );
}

async function generate(instructions) {
  try {
    await api("POST", `/api/jobs/${modalId}/generate`, instructions ? { instructions } : {});
    toast(instructions ? "Refining…" : "Generating application…");
  } catch (e) { toast(e.message, "err"); }
}

async function applyCurrent() {
  try { await api("POST", `/api/jobs/${modalId}/apply`); toast("Applying…"); }
  catch (e) { toast(e.message, "err"); }
}

// findOfficial resolves the company's own application URL for the open job
// (the AI + a site crawl find the link on the company's site/ATS), records it,
// then re-renders the modal so the new "official application" link shows.
async function findOfficial() {
  const btn = $("m_findofficial");
  if (btn) { btn.disabled = true; btn.textContent = "🔎 Searching…"; }
  toast("Finding the official application page…");
  try {
    const res = await api("POST", `/api/jobs/${modalId}/resolve-apply`);
    await loadJobs();
    refreshModalFromData();
    if (res && res.ApplyURL) toast("Official application found", "ok");
    else toast("No official application URL found for this job", "err");
  } catch (e) {
    toast(e.message, "err");
    if (btn) { btn.disabled = false; btn.textContent = "🔎 Find official apply page"; }
  }
}

async function saveApplication() {
  try {
    await api("PUT", `/api/jobs/${modalId}/application`, {
      status: modalEdit.status, notes: modalEdit.notes,
      resume: modalEdit.resume, coverLetter: modalEdit.coverLetter,
    });
    modalDirty = false;
    toast("Saved", "ok");
  } catch (e) { toast(e.message, "err"); }
}

// ---------- status & stats ----------
async function loadStatus() {
  let st;
  try { st = await api("GET", "/api/engine/status"); } catch { return; }

  const pill = $("enginePill");
  pill.classList.toggle("running", st.running && !st.busy);
  pill.classList.toggle("busy", st.busy);
  $("enginePillText").textContent = st.busy ? "working…" : st.running ? "automation on" : "idle";
  $("providerLine").textContent = st.providerError ? "⚠ " + st.providerError : (st.activeProvider || "no AI configured");
  if ($("home_ai_hint")) $("home_ai_hint").hidden = !st.providerError && !!st.activeProvider;

  const s = st.stats || { byStatus: {} };
  const by = s.byStatus || {};
  $("statDiscovered").textContent = by.discovered || 0;
  $("statMatched").textContent = by.matched || 0;
  $("statReady").textContent = by.ready || 0;
  $("statApplied").textContent = (by.applied || 0) + (by.interviewing || 0) + (by.offer || 0);
  $("statSkipped").textContent = by.skipped || 0;
  $("statError").textContent = by.error || 0;
  // pipeline stage metrics
  $("pmTotal").textContent = s.totalJobs || 0;
  $("pmMatched").textContent = by.matched || 0;
  $("pmReady").textContent = by.ready || 0;

  $("btnToggleAuto").textContent = st.running ? "⏹ Stop automation" : "▶ Start automation";
  $("btnToggleAuto").classList.toggle("primary", !st.running);
  $("btnToggleAuto").classList.toggle("danger", st.running);
  $("setupHint").hidden = (s.totalJobs || 0) > 0;
}

// ---------- log ----------
function appendLog(ev) {
  const log = $("log");
  const empty = log.querySelector(".log-empty");
  if (empty) empty.remove();
  const t = new Date(ev.time || Date.now());
  const line = document.createElement("div");
  line.className = "log-line " + (ev.level || "info");
  line.innerHTML = `<span class="log-time">${esc(t.toLocaleTimeString())}</span><span class="log-msg">${esc(ev.message || "")}</span>`;
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  log.appendChild(line);
  while (log.childNodes.length > 400) log.removeChild(log.firstChild);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

async function loadLogs() {
  try {
    const hist = await api("GET", "/api/logs");
    $("log").innerHTML = "";
    if (!hist || !hist.length) { $("log").innerHTML = `<div class="log-empty">No activity yet.</div>`; return; }
    hist.forEach(appendLog);
  } catch { /* ignore */ }
}

// ---------- live events ----------
function connectSSE() {
  const es = new EventSource("/api/events");
  window.__es = es; // exposed for debugging/automation
  es.onmessage = (e) => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    if (ev.type === "log") appendLog(ev);
    else if (ev.type === "refresh") { loadJobs(); loadStatus(); loadAccounts(); }
  };
  es.onerror = () => { /* EventSource auto-reconnects */ };
}

// ---------- engine controls ----------
async function doSearch() { try { await api("POST", "/api/search"); toast("Searching job boards…"); } catch (e) { toast(e.message, "err"); } }
async function doFilter() { try { await api("POST", "/api/filter"); toast("Filtering jobs for relevance with AI…"); } catch (e) { toast(e.message, "err"); } }
async function doTailor() { try { await api("POST", "/api/tailor"); toast("Tailoring matched jobs with AI…"); } catch (e) { toast(e.message, "err"); } }
async function doRunOnce() { try { await api("POST", "/api/engine/run"); toast("Running the full pipeline…"); } catch (e) { toast(e.message, "err"); } }

// ---------- provider models ----------
async function fetchModels(provider) {
  const el = $("test_" + provider);
  if (el) { el.textContent = "loading models…"; el.className = "test-result"; }
  collectConfig();
  try {
    await api("PUT", "/api/config", CFG);
    const r = await api("GET", "/api/ai/models?provider=" + encodeURIComponent(provider));
    if (!r.ok) { if (el) { el.textContent = "✗ " + (r.error || "could not list models"); el.className = "test-result err"; } return; }
    const dl = $("models_" + provider);
    const models = r.models || [];
    if (dl) dl.innerHTML = models.map((m) => `<option value="${esc(m)}"></option>`).join("");
    if (el) { el.textContent = "✓ " + models.length + " models — open the Model field"; el.className = "test-result ok"; }
  } catch (e) { if (el) { el.textContent = "✗ " + e.message; el.className = "test-result err"; } }
}

// ---------- connected accounts ----------
let ACCOUNTS = [];
async function loadAccounts() {
  try { ACCOUNTS = await api("GET", "/api/accounts"); } catch { ACCOUNTS = []; }
  renderAccounts();
}
function renderAccounts() {
  const el = $("accountList");
  if (!el) return;
  if (!ACCOUNTS.length) { el.innerHTML = `<div class="hint">No connectable sources are enabled. Enable e.g. LinkedIn or ZipRecruiter in Job Focus.</div>`; return; }
  el.innerHTML = ACCOUNTS.map((a) => {
    const when = a.capturedAt ? new Date(a.capturedAt).toLocaleString() : "";
    const status = a.connected
      ? `<span class="ac-status on">✓ connected${when ? " · " + esc(when) : ""}</span>`
      : `<span class="ac-status">not connected</span>`;
    const btns = a.connected
      ? `<button class="btn tiny" data-connect="${esc(a.id)}">Reconnect</button> <button class="btn tiny danger" data-disconnect="${esc(a.id)}">Disconnect</button>`
      : `<button class="btn tiny" data-connect="${esc(a.id)}">Connect</button>`;
    return `<div class="account-row"><div class="ac-main"><div class="ac-name">${esc(a.name)}</div><div class="ac-hint">${esc(a.hint || "")}</div></div>${status}${btns}</div>`;
  }).join("");
  el.querySelectorAll("[data-connect]").forEach((b) => b.addEventListener("click", () => connectAccount(b.dataset.connect)));
  el.querySelectorAll("[data-disconnect]").forEach((b) => b.addEventListener("click", () => disconnectAccount(b.dataset.disconnect)));
}
async function connectAccount(source) {
  try { await api("POST", `/api/accounts/${source}/connect`); toast("A browser window is opening — sign in / clear the check, then it captures your session", "ok"); }
  catch (e) { toast(e.message, "err"); }
}
async function disconnectAccount(source) {
  try { await api("DELETE", `/api/accounts/${source}`); toast("Disconnected", "ok"); loadAccounts(); }
  catch (e) { toast(e.message, "err"); }
}
async function openFolder() { try { await api("POST", "/api/open-folder"); } catch (e) { toast(e.message, "err"); } }
async function toggleAuto() {
  const running = $("btnToggleAuto").textContent.includes("Stop");
  try { await api("POST", running ? "/api/engine/stop" : "/api/engine/start"); loadStatus(); } catch (e) { toast(e.message, "err"); }
}

// ---------- simple / advanced mode ----------
function applyMode(mode) {
  const adv = mode === "advanced";
  document.querySelectorAll(".mode-btn").forEach((b) => b.classList.toggle("active", b.dataset.mode === mode));
  document.querySelectorAll(".nav-item[data-adv]").forEach((b) => { b.hidden = !adv; });
  // Leaving advanced while on an advanced-only view? Jump Home.
  if (!adv) {
    const active = document.querySelector(".nav-item.active");
    if (active && active.hasAttribute("data-adv")) switchView("home");
  }
  try { localStorage.setItem("mode", mode); } catch {}
}

// ---------- home (guided flow) ----------
function rolesFromInterest(interest) {
  return (interest || "").split(/[\/\n;|]+/).map((s) => s.trim()).filter(Boolean);
}

function renderHomeParsed() {
  const el = $("home_parsed");
  if (!el || !CFG) return;
  const c = CFG.candidate || {}, f = CFG.focus || {};
  const roles = rolesFromInterest(f.interest);
  if (c.headline || roles.length) {
    el.hidden = false;
    el.innerHTML =
      (c.headline ? `<b>${esc(c.headline)}</b><br/>` : "") +
      (roles.length ? `AI will search for: ${roles.map(esc).join(", ")}` : "");
  } else {
    el.hidden = true;
  }
}

async function uploadResumeFile(file) {
  const fd = new FormData();
  fd.append("resume", file);
  const res = await fetch("/api/resume/parse", { method: "POST", body: fd });
  const data = await res.json().catch(() => null);
  if (!res.ok) throw new Error((data && data.error) || "résumé upload failed");
  return data;
}

async function homeFindJobs() {
  const btn = $("btnHomeGo");
  const fileInput = $("home_resume_file");
  const pasted = $("home_resume_text").value.trim();
  const hasFile = fileInput.files && fileInput.files.length;
  const haveResume = hasFile || pasted || (CFG && CFG.candidate && CFG.candidate.baseResume);
  if (!haveResume) { toast("Add your résumé first (upload or paste) so the AI knows what to search for.", "err"); return; }

  const orig = btn.textContent;
  btn.disabled = true; btn.textContent = "Reading your résumé…";
  try {
    // 1. Import the résumé so the AI can derive profile + roles.
    if (hasFile) await uploadResumeFile(fileInput.files[0]);
    else if (pasted) await api("POST", "/api/resume/parse", { text: pasted, filename: "resume.txt" });

    // 2. Reload config (now carries the parsed profile + roles).
    CFG = await api("GET", "/api/config");

    // 3. Apply the chosen location + remote.
    CFG.focus = CFG.focus || {};
    CFG.focus.location = {
      city: $("home_city").value.trim(), state: $("home_state").value.trim(),
      zip: $("home_zip").value.trim(), radiusMiles: intNum($("home_radius").value, 25),
    };
    CFG.focus.includeRemote = $("home_remote").checked;
    await api("PUT", "/api/config", CFG);
    renderConfig();

    // 4. Run the whole pipeline (search → filter → tailor).
    await api("POST", "/api/engine/run");
    const st = $("home_status");
    if (st) { st.hidden = false; st.textContent = "Working… searching, filtering and tailoring. New jobs appear in the Jobs tab as they're found."; }
    toast("On it — opening Jobs…", "ok");
    setTimeout(() => switchView("jobs"), 900);
  } catch (e) {
    toast(e.message, "err");
  } finally {
    btn.disabled = false; btn.textContent = orig;
  }
}

// ---------- wire up ----------
function init() {
  document.querySelectorAll(".nav-item").forEach((b) => b.addEventListener("click", () => switchView(b.dataset.view)));
  document.querySelectorAll("[data-goto]").forEach((a) => a.addEventListener("click", (e) => { e.preventDefault(); switchView(a.dataset.goto); }));
  document.querySelectorAll("[data-save]").forEach((b) => b.addEventListener("click", saveConfig));
  document.querySelectorAll("[data-test]").forEach((b) => b.addEventListener("click", () => testProvider(b.dataset.test)));
  document.querySelectorAll("[data-models]").forEach((b) => b.addEventListener("click", () => fetchModels(b.dataset.models)));

  $("ai_active").addEventListener("change", updateProviderDim);
  $("jobFilter").addEventListener("change", renderJobs);
  $("btnSearch").addEventListener("click", doSearch);
  $("btnSearch2").addEventListener("click", doSearch);
  $("btnFilter").addEventListener("click", doFilter);
  $("btnTailor").addEventListener("click", doTailor);
  $("btnRunOnce").addEventListener("click", doRunOnce);
  $("btnOpenFolder").addEventListener("click", openFolder);
  $("btnToggleAuto").addEventListener("click", toggleAuto);
  $("btnHomeGo").addEventListener("click", homeFindJobs);
  document.querySelectorAll(".mode-btn").forEach((b) => b.addEventListener("click", () => applyMode(b.dataset.mode)));
  $("btnClearLog").addEventListener("click", () => { $("log").innerHTML = `<div class="log-empty">cleared</div>`; });

  $("modalClose").addEventListener("click", closeModal);
  $("modal").addEventListener("click", (e) => { if (e.target.id === "modal") closeModal(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });

  let savedMode = "simple";
  try { savedMode = localStorage.getItem("mode") || "simple"; } catch {}
  applyMode(savedMode);

  loadConfig().then(loadSources).then(loadAccounts);
  loadJobs();
  loadStatus();
  loadLogs();
  connectSSE();
  setInterval(loadStatus, 5000);
}

document.addEventListener("DOMContentLoaded", init);
