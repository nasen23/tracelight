// tracelight web client: the timeline of a trace parsed on the server,
// drawn from tiles of level-of-detail blocks fetched as the view needs them.
"use strict";

// ---------------------------------------------------------------- constants

const M = THEME.metrics;
const BOX = 8; // values per drawn box in S.boxes (see draw)
// LABEL_W, the label gutter's width, follows its edge as it is dragged
// (kept in the browser; double-click: back to the default).
const clampLabelW = (w) => Math.min(Math.max(w, M.labelMinGutter), M.labelMaxGutter);
let LABEL_W = clampLabelW(+localStorage.getItem("nsa-label-w") || M.labelW);
const onGutterEdge = (x) => x >= LABEL_W - 4 && x <= LABEL_W + 3;
const RULER_H = M.rulerH, LANE_H = M.laneH, TRACK_PAD = M.trackPad, GROUP_H = M.groupH, TOGGLE_H = M.toggleH;
const MIN_LEVEL = -8, MAX_LEVEL = 50;
const ZOOM_PER_NOTCH = M.zoomPerNotch;
const FIELDS = 7; // start, end, name, kind, count, index, busy
const KIND = THEME.kindIds;
const KIND_NAME = THEME.kinds;
const MAX_TILES = 800;
// The active palette (applyTheme): THEME.palettes.dark or .light.
let PAL = THEME.palettes.dark, C = PAL.colors;
const FONT = 'system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif';

// ---------------------------------------------------------------- state

// S is the page's state. A session shows one or more traces (S.traces) on
// one time axis: each trace has its own metadata, tiles, names and the
// state of its rows (see newTrace); the view is shared.
const S = {
  token: sessionStorage.getItem("nsa-token") || "",
  info: null,
  inputs: [],   // the paths the session was opened with
  traces: [],
  focus: null,  // the trace the sidebar and the summaries are about
  align: "collectives", // "collectives": on their collective kernels; "abs": their clocks; "start": first events together
  rows: [], contentH: 0,
  view: { start: 0, scale: 1 }, target: { start: 0, scale: 1 }, anchorX: 0, animating: false, lastT: 0,
  scrollY: 0,
  inflight: 0, queue: [], generation: 0, frameNo: 0,
  mouse: null, dragging: null, range: null,
  selected: null, highlight: null,
  panelTab: 0,
  perf: { show: false, times: [], tileMs: [], slots: { exact: 0, coarse: 0, blank: 0 } },
  dirty: false, // a frame is requested
  pickerDir: "",
  // picked are the traces ticked in the file browser, to open side by side.
  picked: [],
  navSpeed: navSpeedOf(localStorage.getItem("nsa-nav-speed")), // multiplies the W/A/S/D rates
  // The density: "auto" (compact with more than M.compactAuto traces), "on"
  // or "off"; and the summary lanes the rank overview shows (see
  // layoutCompact). Kept in the browser.
  compactMode: ["on", "off"].includes(localStorage.getItem("nsa-compact")) ? localStorage.getItem("nsa-compact") : "auto",
  overlaid: localStorage.getItem("nsa-compact-overlaid") === "1",
  compactLanes: (() => { try { const l = JSON.parse(localStorage.getItem("nsa-compact-lanes")); return Array.isArray(l) && l.length ? l : null; } catch { return null; } })(),
  ranks: [], // the traces' blocks of rows in the rank overview
};
// The tabs (see "tabs" below): DOC is the one shown, whose state is in S;
// BACKGROUND is true while S holds another's (withDoc), when nothing on the
// page is drawn from it.
let DOC = null, BACKGROUND = false;
// navSpeedOf is a saved navigation speed: one of THEME.navSpeeds', else 1.
function navSpeedOf(v) { return THEME.navSpeeds.some((s) => s.times === +v) ? +v : 1; }
// The trace in focus, its metadata and its path: what a single-trace page
// called the trace.
const focusTrace = () => (S.focus && !S.focus.off ? S.focus : null) || shownTraces().find((t) => t.meta) || shownTraces()[0] || S.traces[0] || null;
// shownTraces are the session's traces not left out (setTraceShown).
const shownTraces = () => S.traces.filter((t) => !t.off);
Object.defineProperty(S, "meta", { get: () => { const f = focusTrace(); return f && f.meta; } });
Object.defineProperty(S, "path", { get: () => { const f = focusTrace(); return f ? f.path : ""; } });
const loadedTraces = () => S.traces.filter((t) => t.meta);

const TRACE_H = 30, PENDING_H = 28, UNFOLDED = 8;

// newTrace is a trace of a session, from what /api/session says of it.
function newTrace(info, i, n) {
  return {
    i, path: info.path, name: info.name, host: info.host || "", rank: info.rank ?? -1, size: info.size || 0,
    // progress: how far the server's load of it is, while it loads
    // (/api/progress: {phase, fraction}).
    progress: null,
    // key names it in links (#only=, #omit=); off: left out (setTraceShown).
    key: traceBase(info.path), off: false,
    sessionStart: info.sessionStart || 0, sessionStartNs: info.sessionStartNs || "", sessionMono: info.sessionMono || 0, clock: info.clock || "", note: info.note || "",
    offset: 0, basis: "start",
    meta: null, loading: false, error: "",
    // Many traces: the first open, the others on demand; the rank overview
    // shows them all.
    folded: n > UNFOLDED && i >= UNFOLDED && S.compactMode !== "on" && !(S.compactMode === "auto" && n > M.compactAuto),
    tiles: new Map(), names: new Map(), colorCache: new Map(),
    collapsed: new Set(), hidden: new Set(), showIdle: false, summaries: {},
  };
}

// offsetOf is where on the shared axis a trace's own time 0 (its first
// event) is: at the moment it happened, by its capture's clocks, the
// session's earliest at 0, or at 0 when aligning starts (alignTraces in
// shared.js, as the desktop app). Times shown (the ruler, the tooltip, the
// details) are on this shared axis.
function offsetOf(T) {
  if (S.traces.length < 2) return 0; // one trace: its own time, as ever
  return T.offset || 0;
}
// alignSummary says what the traces' places rest on: the ones lined up on
// collectives in a line, the others each in their own.
function alignSummary() {
  const coll = S.traces.filter((T) => T.basis === "collectives" && !T.aligned.ref);
  let h = "";
  if (coll.length) {
    const ref = S.traces.find((T) => T.aligned && T.aligned.ref);
    const m = coll.map((T) => T.aligned.matches), sp = coll.map((T) => T.aligned.spread).sort((x, y) => x - y);
    h += `<div class="note">${coll.length + 1} traces lined up on their collective kernels: ${Math.min(...m)}–${Math.max(...m)} matched each, ends within ±${esc(fmtDur(sp[sp.length >> 1]))} (worst ±${esc(fmtDur(sp[sp.length - 1]))}), against ${esc(rankName(ref))}</div>`;
  }
  const rest = S.traces.filter((T) => T.basis !== "collectives");
  const groups = new Map();
  for (const T of rest) { const n = alignNote(T); groups.set(n, [...(groups.get(n) || []), T]); }
  for (const [n, ts] of groups) {
    const warn = (ts[0].basis === "monotonic" && !(ts[0].clock && !ts[0].host)) || ts[0].basis === "start";
    const who = ts.length > 3 ? `${ts.length} traces` : ts.map((T) => esc(rankName(T))).join(", ");
    h += `<div class="note ${warn ? "warn" : ""}">${who}: ${esc(n)}</div>`;
  }
  return h;
}
// alignNote says what a trace's place on the axis rests on.
function alignNote(T) {
  const a = T.aligned || {};
  switch (T.basis) {
    case "collectives": return a.ref ? "reference for the collectives" : `${a.matches} collectives matched, ±${fmtDur(a.spread)}`;
    case "wall": return "by its wall clock";
    case "host": return "by its host's clock, against a trace of the same host";
    case "monotonic": return T.clock && !T.host ? "by the clock of the file it was merged into, with the other ranks of that file"
      : "by its host's clock only, among the host's traces: not placed against the others";
  }
  return "no clock: at its start";
}
// computeOrigin aligns the traces (their offsets and what each rests on);
// again as their metadata (their first event's time) comes.
function computeOrigin() {
  // Wall clocks relative to the earliest, exactly (ns since 1970 are not
  // exact as numbers: BigInt, from the server's decimal strings), as the
  // desktop app does; +1 so that 0 says "none".
  const big = S.traces.map((T) => T.sessionStartNs ? BigInt(T.sessionStartNs) : 0n);
  let ref = 0n;
  for (const b of big) if (b > 0n && (ref === 0n || b < ref)) ref = b;
  const a = alignSession(S.traces.map((T, i) => ({ utc: big[i] > 0n ? Number(big[i] - ref) + 1 : 0, mono: T.sessionMono, host: T.host, clock: T.clock,
    start: T.meta ? T.meta.start : 0, coll: T.meta && T.meta.collectives || {} })), S.align);
  S.traces.forEach((T, i) => { T.offset = a[i].offset; T.basis = a[i].basis; T.aligned = a[i]; });
}
// extent is the shared time the loaded traces cover.
function extent() {
  let lo = Infinity, hi = -Infinity;
  for (const T of loadedTraces()) { if (T.off) continue; const o = offsetOf(T); lo = Math.min(lo, o); hi = Math.max(hi, o + T.meta.duration); }
  return lo <= hi ? [lo, hi] : [0, 1000];
}

const $ = (id) => document.getElementById(id);
const canvas = $("timeline");
const ctx = canvas.getContext("2d");

// Formatting and the colors of events are in shared.js, the palette and
// metrics in theme.js (generated from the desktop app's): see DESIGN.md.

let BG_RGB = hexRGB(C.bg);
function clearColors() { for (const T of S.traces) T.colorCache.clear(); }
// rgba is a palette color, translucent.
function rgba(hex, a) { const [r, g, b] = hexRGB(hex); return `rgba(${r},${g},${b},${a})`; }
function colorOf(T, nameIdx, kind) {
  const key = nameIdx * 8 + kind;
  let c = T.colorCache.get(key);
  if (c) return c;
  const rgb = eventRGB(T.names.get(nameIdx) || "", kind, PAL);
  const css = (r, g, b, a = 1) => `rgba(${r},${g},${b},${a})`;
  const mix = (t) => rgb.map((v, i) => Math.round(v * (1 - t) + BG_RGB[i] * t));
  c = { rgb, solid: css(...rgb), dim: css(...mix(0.75)), merged: (a) => css(...mix(0.3), a), line: (a) => css(...rgb, a),
    dimRGB: mix(0.75), mergedRGB: mix(0.3) };
  if (T.names.has(nameIdx)) T.colorCache.set(key, c);
  return c;
}

// ---------------------------------------------------------------- API

class AuthError extends Error {}

async function api(path, params = {}, opts = {}) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) for (const x of [].concat(v)) q.append(k, x);
  const headers = {};
  if (S.token) headers.Authorization = "Bearer " + S.token;
  const res = await fetch("api/" + path + (q.size ? "?" + q.toString() : ""), { headers, signal: opts.signal });
  if (res.status === 401) {
    showTokenDialog(S.token ? "That token was not accepted." : "");
    throw new AuthError("a token is required");
  }
  const body = await res.json().catch(() => ({ error: res.statusText }));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function showTokenDialog(err) {
  $("token-err").textContent = err;
  $("token-dialog").hidden = false;
  $("token-input").focus();
}
$("token-form").addEventListener("submit", (e) => {
  e.preventDefault();
  S.token = $("token-input").value.trim();
  sessionStorage.setItem("nsa-token", S.token);
  $("token-dialog").hidden = true;
  start();
});

let toastTimer = 0;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 5000);
}

// ---------------------------------------------------------------- file picker

const ICON_DIR = '<svg class="ic" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>';
const ICON_TRACE = '<svg class="ic" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9h6M9 12h8M7 15h5"/></svg>';

async function openPicker(dir = S.pickerDir) {
  $("picker").hidden = false;
  S.pickerDir = dir;
  const files = $("files");
  files.innerHTML = '<div class="file"><span></span><span></span><span class="n muted">Loading…</span></div>';
  let res;
  try {
    res = await api("files", { dir });
  } catch (e) {
    if (!(e instanceof AuthError)) files.innerHTML = `<div class="file"><span></span><span></span><span class="n warn">${esc(e.message)}</span></div>`;
    return;
  }
  const crumbs = $("crumbs");
  const parts = res.dir ? res.dir.split("/") : [];
  let html = '<a data-dir="">root</a>';
  parts.forEach((p, i) => { html += ' / <a data-dir="' + esc(parts.slice(0, i + 1).join("/")) + '">' + esc(p) + "</a>"; });
  crumbs.innerHTML = html;
  crumbs.querySelectorAll("a").forEach((a) => a.addEventListener("click", () => openPicker(a.dataset.dir)));
  if (!res.entries.length) {
    files.innerHTML = '<div class="file"><span></span><span></span><span class="n muted">No traces or folders here.</span></div>';
  } else {
    files.innerHTML = res.entries.map((e) => {
      const tag = e.note ? `<span class="tag ${e.ready ? "ok" : ""}">${esc((e.type === "report" ? "nsys-rep · " : "") + e.note)}</span>` : "";
      const date = new Date(e.mtime).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
      const pick = e.type === "dir" ? '<span class="pick"></span>' :
        `<input class="pick" type="checkbox" data-pick="${esc(e.path)}" ${S.picked.includes(e.path) ? "checked" : ""} title="Pick to open side by side" aria-label="Pick ${esc(e.name)}">`;
      return `<div class="file ${e.type === "dir" ? "dir" : ""}" tabindex="0" data-type="${e.type}" data-path="${esc(e.path)}">
        ${pick}${e.type === "dir" ? ICON_DIR : ICON_TRACE}<span class="n">${esc(e.name)}${tag}</span>
        <span class="s">${e.size ? fmtBytes(e.size) : ""}</span><span class="d">${e.type === "dir" ? "" : esc(date)}</span></div>`;
    }).join("");
  }
  files.querySelectorAll("input[data-pick]").forEach((el) => el.addEventListener("click", (e) => {
    e.stopPropagation();
    const p = el.dataset.pick;
    S.picked = el.checked ? [...S.picked.filter((x) => x !== p), p] : S.picked.filter((x) => x !== p);
    pickedButton();
  }));
  files.querySelectorAll(".file[data-path]").forEach((el) => {
    const go = (e) => el.dataset.type === "dir" ? openPicker(el.dataset.path) : openTrace(el.dataset.path, { newTab: e.metaKey || e.ctrlKey });
    el.addEventListener("click", go);
    el.addEventListener("keydown", (e) => { if (e.key === "Enter") go(e); });
  });
  $("picker-note").textContent = S.info && S.info.notes ? S.info.notes.join(" ") : "";
  // A folder of several traces (one per rank) opens as a session.
  const n = res.entries.filter((e) => e.type !== "dir").length, all = $("open-folder");
  all.hidden = n < 2;
  all.textContent = `Open all ${n} side by side`;
  all.onclick = (e) => openPaths([S.info.root.replace(/\/$/, "") + (res.dir ? "/" + res.dir : "")], { newTab: e.metaKey || e.ctrlKey });
  pickedButton();
}
// pickedButton offers to open the traces picked in the browser (from any
// folder) side by side, in place of opening the whole folder.
function pickedButton() {
  const b = $("open-picked"), n = S.picked.length;
  b.hidden = n === 0;
  $("open-folder").style.display = n ? "none" : "";
  $("picked-clear").hidden = n === 0;
  b.textContent = n === 1 ? "Open the picked trace" : `Open ${n} picked side by side`;
}
$("open-picked").addEventListener("click", (e) => {
  const picked = S.picked;
  S.picked = [];
  $("picker").hidden = true;
  openPaths(picked, { newTab: e.metaKey || e.ctrlKey });
});
$("picked-clear").addEventListener("click", () => { S.picked = []; openPicker(S.pickerDir); });
$("picker-close").addEventListener("click", () => { $("picker").hidden = true; });
$("open").addEventListener("click", () => openPicker());
// The desktop app's views in the browser (app.html), on the same traces,
// view and token.
$("app-ui").addEventListener("click", (e) => { e.currentTarget.href = "app.html" + location.search + location.hash; });

// ---------------------------------------------------------------- opening traces

// splitPaths reads the path input: one path, a directory or a glob, or
// several absolute ones separated by spaces.
function splitPaths(v) {
  return v.trim().split(/\s+(?=\/)/).map((x) => x.trim()).filter(Boolean);
}

// openTrace opens one path (a trace, a directory of traces or a glob).
function openTrace(path, opts) { return openPaths([path], opts); }

// openPaths opens a session of the traces the paths name (absolute, or
// relative to the server's root) in the tab shown (a new one with newTab):
// the server lists them, and each loads as it shows. link carries the view,
// the selection and the alignment to apply (a deep link's fragment, or a
// tab restored). What comes back lands in the tab it was opened in, shown
// or not (withDoc).
async function openPaths(inputs, { link = null, newTab = false, all = false } = {}) {
  if (newTab && !openTab()) return;
  $("picker").hidden = true;
  pathError("");
  const one = inputs.length === 1 ? inputs[0].split("/").pop() : inputs.length + " paths";
  // While the server lists what the paths name, a line a path (a merged
  // file's shows its read); then a line a trace (loadItems).
  S.listing = inputs.map((p) => ({ path: p, name: p.replace(/\/+$/, "").split("/").pop() || p, progress: null }));
  showScreen({ kind: "loading", text: `Opening ${one}…` });
  S.pathText = inputs.join(" ");
  const doc = DOC, gen = S.generation = ++GEN;
  renderDocTabs();
  let res;
  try {
    res = await api("session", { path: inputs });
  } catch (e) {
    if (e instanceof AuthError) return;
    withDoc(doc, () => {
      if (gen !== S.generation) return;
      S.listing = null;
      // Say why next to the path, and keep showing what was open.
      pathError(e.message);
      S.pathText = inputs.join(" ");
      syncChrome();
      focusPath();
      showScreen(S.meta ? null : { kind: "empty" });
    });
    return;
  }
  const h = link || new URLSearchParams("");
  // Too many traces asks which (chooseHTML), but not for those chosen, a
  // link that says #all=1, nor a tab restored that showed them.
  if (!all && h.get("all") !== "1" && !h.get("v") && shouldChoose(res.traces.length, uniqueBytes(res.traces), inputs.length)) {
    withDoc(doc, () => {
      if (gen !== S.generation) return;
      S.listing = null;
      S.pathText = inputs.join(" ");
      syncChrome();
      showScreen({ kind: "choose", inputs, traces: res.traces, ticked: res.traces.map((_, i) => i < M.choosePick) });
    });
    return;
  }
  const loads = withDoc(doc, () => {
    if (gen !== S.generation) return null;
    const n = res.traces.length;
    S.listing = null;
    clearTraces(S.traces);
    Object.assign(S, {
      inputs, traces: res.traces.map((t, i) => newTrace(t, i, n)), focus: null,
      scrollY: 0, selected: null, highlight: null, range: null, pathText: inputs.join(" "),
    });
    computeOrigin();
    for (const w of res.warnings) toast(w);
    addRecent(inputs.join(" "));
    S.align = ["start", "abs"].includes(h.get("align")) ? h.get("align") : "collectives";
    // The traces a link leaves out (#only=, #omit=: names, by commas).
    const names = (k) => h.getAll(k).flatMap((v) => v.split(",")).map((x) => x.trim()).filter(Boolean);
    filterOff(S.traces.map((T) => T.key), names("only"), names("omit")).forEach((off, i) => { S.traces[i].off = off; });
    showScreen({ kind: "loading", text: `Loading ${n === 1 ? res.traces[0].name : n + " traces"}…` });
    syncChrome();
    layoutRows();
    renderSidebar();
    renderPanel();
    // The open traces load at once, in parallel; folded ones when unfolded.
    // Those left out load too (not their tiles): the session's time axis
    // and alignment are of every trace, as the desktop app's, so a link's
    // view means the same whichever show.
    return Promise.all(S.traces.filter((t) => !t.folded).map(loadMeta));
  });
  if (!loads) return;
  await loads;
  withDoc(doc, () => {
    if (gen !== S.generation) return;
    if (!loadedTraces().length) {
      const err = S.traces.find((t) => t.error);
      pathError(err ? `${err.name}: ${err.error}` : "Nothing could be opened.");
      showScreen({ kind: "empty" });
      return;
    }
    showScreen(null);
    fit(false);
    if (DOC === doc && !BACKGROUND) canvas.focus();
    // A link can carry the view and the selection (see saveHash).
    const v = (h.get("v") || "").split(",").map(Number);
    if (v.length === 2 && v.every(Number.isFinite) && v[1] > v[0]) {
      S.target = clampView({ start: v[0], scale: (v[1] - v[0]) / dataW() });
      S.view = { ...S.target };
    }
    const tab = +h.get("tab");
    if (tab > 0 && tab < TABS.length) { S.panelTab = tab; renderPanel(); }
    const sel = (h.get("sel") || "").split(":").map(Number);
    if (sel.length === 3) sel.unshift(0); // a single-trace link: row, lane, index
    if (sel.length === 4 && sel.every(Number.isInteger) && S.traces[sel[0]]?.meta) select(S.traces[sel[0]], sel[1], sel[2], sel[3], true);
    saveHash();
    saveTabs();
    invalidate();
  });
}

// loadMeta loads a trace's metadata (its rows), which parses it on the
// server, once.
function loadMeta(T) {
  if (T.meta) return Promise.resolve();
  if (T.pending) return T.pending;
  T.loading = true;
  T.error = "";
  const doc = DOC, gen = S.generation;
  T.pending = api("trace", { path: T.path }).then((meta) => withDoc(doc, () => {
    if (gen !== S.generation) return;
    T.meta = meta;
    if (!T.sessionStart && meta.sessionStart) { T.sessionStart = meta.sessionStart; T.sessionStartNs = meta.sessionStartNs || ""; }
    if (!T.sessionMono && meta.sessionMono) T.sessionMono = meta.sessionMono;
    if (!T.clock && meta.clock) T.clock = meta.clock;
    computeOrigin(); // its first event's time is known now
    if (!T.host && meta.host) T.host = meta.host;
  })).catch((e) => {
    if (!(e instanceof AuthError)) T.error = e.message;
  }).finally(() => {
    T.loading = false;
    T.pending = null;
    T.progress = null;
    withDoc(doc, () => {
      if (gen !== S.generation) return;
      if (S.screen && S.screen.kind === "loading") renderScreen(); // one more done
      layoutRows();
      renderSidebar();
      renderPanel(); // its tabs depend on what the trace holds
      invalidate();
    });
  });
  layoutRows();
  return T.pending;
}

// setAlign aligns the traces on their capture's clock ("abs") or on their
// first events ("start"), keeping the view's middle where it was on the
// trace in focus.
function setAlign(mode) {
  if (mode === S.align) return;
  const F = focusTrace(), before = F ? offsetOf(F) : 0;
  S.align = mode;
  computeOrigin();
  const d = F ? offsetOf(F) - before : 0;
  S.view.start += d; S.target.start += d;
  S.view = clampView(S.view); S.target = clampView(S.target);
  renderSidebar();
  saveHash();
  invalidate();
}

// saveHash keeps the traces, the view and the selection in the address, so
// that a link opens the same place.
let hashTimer = 0;
function saveHash() {
  clearTimeout(hashTimer);
  hashTimer = setTimeout(() => {
    if (!S.inputs.length) { // an empty tab
      history.replaceState(null, "", location.pathname + (S.tokenInURL && S.token ? "#token=" + encodeURIComponent(S.token) : ""));
      return;
    }
    if (!S.meta) return;
    // The paths in the query (?path=/abs/path, repeated); the token, which
    // must not reach the server's logs, and the view in the fragment.
    const h = new URLSearchParams();
    if (S.tokenInURL && S.token) h.set("token", S.token);
    h.set("v", Math.round(S.target.start) + "," + Math.round(S.target.start + S.target.scale * dataW()));
    if (S.selected) {
      const sel = [S.selected.row, S.selected.lane, S.selected.index];
      h.set("sel", (S.traces.length > 1 ? [S.selected.ti, ...sel] : sel).join(":"));
    }
    if (S.panelTab) h.set("tab", S.panelTab);
    if (S.align !== "collectives") h.set("align", S.align);
    const [fk, fv] = traceFilter(S.traces.map((T) => T.key), S.traces.map((T) => T.off));
    if (fk) h.set(fk, fv.join(","));
    const q = S.inputs.map((p) => "path=" + encodeURIComponent(p).replace(/%2F/g, "/")).join("&");
    history.replaceState(null, "", "?" + q + "#" + h.toString());
    saveTabs();
  }, 300);
}

// ---------------------------------------------------------------- layout

function idleName(T, id) {
  const n = T.meta.devices[id];
  return "GPU " + id + (n ? " · " + n : "");
}

// layoutRows lays out every trace: with several, a heading for each, then
// (unless folded) its rows, as the desktop app lays out one.
function layoutRows() {
  if (compactOn()) { layoutCompact(); return; }
  S.ranks = [];
  const rows = [];
  let y = 0;
  const multi = S.traces.length > 1;
  for (const T of S.traces) {
    if (T.off) continue; // left out (setTraceShown)
    if (multi) {
      rows.push({ kind: "trace", t: T, y, h: TRACE_H }); y += TRACE_H;
      if (T.folded) continue;
    }
    if (!T.meta) { rows.push({ kind: "pending", t: T, y, h: PENDING_H }); y += PENDING_H; continue; }
    const m = T.meta;
    if (m.idle.length) { rows.push({ kind: "toggle", t: T, y, h: TOGGLE_H }); y += TOGGLE_H; }
    let idleDone = !m.idle.length || !T.showIdle;
    const idleRows = () => {
      idleDone = true;
      for (const id of m.idle) {
        const g = idleName(T, id);
        rows.push({ kind: "group", t: T, group: g, idle: true, y, h: GROUP_H }); y += GROUP_H;
        if (!T.collapsed.has(g)) { rows.push({ kind: "idle", t: T, group: g, y, h: LANE_H + 2 * TRACK_PAD }); y += LANE_H + 2 * TRACK_PAD; }
      }
    };
    let group = null, gi = -1;
    m.rows.forEach((r, i) => {
      if (!idleDone && r.kind !== "stream") idleRows();
      if (r.group !== group) {
        group = r.group;
        rows.push({ kind: "group", t: T, group, y, h: GROUP_H, tracks: 0, events: 0 });
        gi = rows.length - 1;
        y += GROUP_H;
      }
      rows[gi].tracks++;
      rows[gi].events += r.count;
      if (T.collapsed.has(group) || T.hidden.has(i)) return;
      const h = trackHeight(r.kind, r.lanes), [lh, pad] = trackLane(r.kind);
      rows.push({ kind: "track", t: T, group, track: i, y, h, lh, pad });
      y += h;
    });
    if (!idleDone) idleRows();
  }
  S.rows = rows;
  S.contentH = y;
  clampScroll();
}

// ---------------------------------------------------------------- the rank overview
// Many traces (the ranks of a job) side by side: each a block of a busy/idle
// strip and a summary lane per kind of row (GPU, CUDA API, NVTX), every row
// of the kind drawn into it, on the shared axis; the trace's name stays in
// the gutter as its block scrolls. The same layout as the desktop app's
// (compactLayout and busyColumns in shared.js). C toggles it.

function compactOn() {
  if (S.compactMode === "on") return true;
  if (S.compactMode === "off") return false;
  return shownTraces().length > M.compactAuto;
}
const lanesOn = () => S.compactLanes || THEME.compactDefault;
function setCompact(mode) {
  S.compactMode = mode;
  if (mode === "auto") localStorage.removeItem("nsa-compact"); else localStorage.setItem("nsa-compact", mode);
  layoutRows(); renderSidebar(); invalidate();
}
function toggleCompact() { setCompact(compactOn() ? "off" : "on"); }
function setLane(key, show) {
  const on = lanesOn();
  let next = THEME.compactLanes.map((l) => l.key).filter((k) => k === key ? show : on.includes(k));
  if (!next.length) next = ["gpu"];
  S.compactLanes = next;
  localStorage.setItem("nsa-compact-lanes", JSON.stringify(next));
  layoutRows(); renderSidebar(); invalidate();
}
// setOverlaid draws the rows of a kind over each other (true) or stacks
// what runs in parallel (false).
function setOverlaid(on) {
  S.overlaid = on;
  if (on) localStorage.setItem("nsa-compact-overlaid", "1"); else localStorage.removeItem("nsa-compact-overlaid");
  layoutRows(); renderSidebar(); invalidate();
}
// slotOf is the sub-lane of its summary lane that lane li of a row draws
// into (laneSlot), -1 for none.
const slotOf = (r, li) => laneSlot(r.kind, r.lanes, r.overview || null, li, !S.overlaid);
const rankName = (T) => traceBase(T.name);

function layoutCompact() {
  // Per trace and kind, its rows shown and the sub-lanes in use, in order.
  const shown = shownTraces();
  const info = shown.map((T) => {
    const by = {}, slots = {}, tops = {}; // tops: where a thread's band of nested ranges starts
    if (!T.meta) return { by: { stream: [] }, slots, tops, subs: { stream: 1 } }; // a lane to show it loading in
    T.meta.rows.forEach((r, i) => {
      if (T.hidden.has(i)) return;
      (by[r.kind] ||= []).push(i);
      const sl = slots[r.kind] ||= [], tp = tops[r.kind] ||= [];
      for (let li = 0; li < r.lanes; li++) {
        const s = slotOf(r, li);
        if (s >= 0 && !sl.includes(s)) sl.push(s);
        if (s >= 0 && li === 0 && r.kind !== "stream") tp.push(s);
      }
    });
    const subs = {};
    for (const k in by) { (slots[k] ||= []).sort((a, b) => a - b); subs[k] = Math.max(slots[k].length, Math.min(by[k].length, 1)); }
    return { by, slots, tops, subs };
  });
  const { rows, height } = compactLayout(info.map((x) => x.subs), lanesOn());
  S.rows = []; S.ranks = [];
  let band = 0, prev = "";
  for (const cr of rows) {
    const T = shown[cr.trace], inf = info[cr.trace];
    const kind = cr.key === "" ? "stream" : THEME.compactLanes.find((l) => l.key === cr.key).kind;
    const members = T.meta ? inf.by[kind] || [] : [];
    if (cr.key === "") { S.ranks.push({ t: T, y0: cr.y, y1: cr.y + cr.h, idle: -1 }); band = -1; prev = ""; }
    else if (cr.key !== prev) { band++; prev = cr.key; }
    const rank = S.ranks.length - 1, sl = inf.slots[kind] || [], slot = cr.sub < sl.length ? sl[cr.sub] : 0;
    S.ranks[rank].y1 = cr.y + cr.h;
    S.rows.push({ kind: cr.key === "" ? "strip" : T.meta ? "summary" : "pending", t: T, key: cr.key, y: cr.y, h: cr.h, members, rank,
      sub: cr.sub, slot, top: cr.sub > 0 && (inf.tops[kind] || []).includes(slot), band });
  }
  S.contentH = height;
  clampScroll();
}

const stripScratch = { flat: [], busy: [] };

// stripStyles are the strip's colors by level (0 idle, 1..8 busier),
// made once per palette.
let stripCache = null;
function stripStyles() {
  if (!stripCache || stripCache.c !== C) {
    stripCache = { c: C, s: [rgba(C.warn, 0.85), ...Array.from({ length: 8 }, (_, i) => rgba(C.accent, 0.12 + 0.4 * (i + 1) / 8))] };
  }
  return stripCache.s;
}

// batchRect queues a rectangle to fill with style; flushRects fills those
// queued, one path a style: compact lanes draw thousands of small blocks.
const rectBatch = new Map();
function batchRect(style, x, y, w, h) {
  let a = rectBatch.get(style);
  if (!a) rectBatch.set(style, a = []);
  a.push(x, y, w, h);
}
function flushRects() {
  for (const [style, a] of rectBatch) {
    if (!a.length) continue;
    ctx.fillStyle = style; ctx.beginPath();
    for (let i = 0; i < a.length; i += 4) ctx.rect(a[i], a[i + 1], a[i + 2], a[i + 3]);
    ctx.fill();
    a.length = 0;
  }
  if (rectBatch.size > 512) rectBatch.clear();
}
// tileRow is a tile's blocks of row r (indexed once per tile).
function tileRow(tile, r) {
  const d = tile.data;
  if (!d.byRow) { d.byRow = new Map(); for (const x of d.rows) d.byRow.set(x.r, x); }
  return d.byRow.get(r);
}

function compactGutter(y, h) {
  ctx.fillStyle = C.panel; ctx.fillRect(0, y, LABEL_W, h);
  ctx.fillStyle = C.border; ctx.fillRect(LABEL_W - 1, y, 1, h);
}

// drawSummary draws a sub-lane of a summary lane: the lanes of the rows of
// its kind that draw into it (slotOf), overlaid.
function drawSummary(row, ri, y, W) {
  const T = row.t, off = offsetOf(T), tiles = tilesOf(T), v = S.view;
  if (row.band % 2 === 0) { ctx.fillStyle = C.rowAlt; ctx.fillRect(LABEL_W, y, W - LABEL_W, row.h); }
  if (row.sub > 0) { ctx.fillStyle = row.top ? C.border : rgba(C.border, 0.5); ctx.fillRect(LABEL_W, y, W - LABEL_W, 1); } // top: another thread's band
  resetColumns(W);
  for (const m of row.members) {
    const meta = T.meta.rows[m];
    for (let li = 0; li < meta.lanes; li++) {
      if (slotOf(meta, li) !== row.slot) continue;
      for (const { tile, clip } of tiles) {
        const tr = tileRow(tile, m);
        if (!tr || !tr.l[li]) continue;
        if (clip) {
          flushColumns(y + 1, LANE_H - 3); flushRects(); drawWide(); // what came before, unclipped
          ctx.save(); ctx.beginPath(); ctx.rect(xAt(off + clip[0]), y, (clip[1] - clip[0]) / v.scale, row.h); ctx.clip();
        }
        drawBlocks(tr.l[li], T, off, m, li, y, W, true);
        if (clip) { flushColumns(y + 1, LANE_H - 3); flushRects(); drawWide(); ctx.restore(); }
      }
    }
  }
  flushColumns(y + 1, LANE_H - 3);
  flushRects();
  drawWide();
  compactGutter(y, row.h);
  if (row.sub > 0) return;
  const l = THEME.compactLanes.find((x) => x.key === row.key).label, f = `11px ${FONT}`;
  text(l, LABEL_W - 10 - textW(l, f), y + row.h / 2, f, C.muted);
}

// RANK_LOADING is a rank's idle share while its tiles of the view have not
// come: it says "loading…", not that its GPUs idle (as the desktop app).
const RANK_LOADING = -2;

// drawLoadingBar draws, along the top of the timeline under the ruler, a
// segment going back and forth while the view's tiles stream: the view is
// not complete yet (as the desktop app's).
function drawLoadingBar(W) {
  const x0 = LABEL_W, w = W - LABEL_W;
  if (w <= 0) return;
  ctx.fillStyle = rgba(C.accent, 0.15); ctx.fillRect(x0, RULER_H, w, 3);
  const t = (performance.now() % 1400) / 1400, seg = w / 4;
  ctx.save(); ctx.beginPath(); ctx.rect(x0, RULER_H, w, 3); ctx.clip();
  ctx.fillStyle = C.accent; ctx.fillRect(x0 - seg + t * (w + seg), RULER_H, seg, 3);
  ctx.restore();
  invalidate(); // it moves while it shows
}

// drawStrip draws a trace's busy/idle strip: the share of each pixel column
// its GPUs are busy, idle columns inside the trace tinted.
function drawStrip(row, y, W) {
  const T = row.t, rk = S.ranks[row.rank];
  // The separator from the trace above, in the middle of the gap.
  if (row.rank > 0) { ctx.fillStyle = C.border; ctx.fillRect(0, y - M.compactGap / 2, W, 1); }
  compactGutter(y, row.h);
  if (!T.meta) return;
  const off = offsetOf(T), v = S.view, n = Math.floor(dataW()), flat = stripScratch.flat;
  flat.length = 0;
  const tiles = tilesOf(T);
  if (!tiles.length) {
    // Its tiles of the view not come yet: nothing says its GPUs idle.
    const c0 = Math.max(Math.ceil((off - v.start) / v.scale), 0), c1 = Math.min(Math.floor((off + T.meta.duration - v.start) / v.scale), n - 1);
    if (c1 >= c0) { ctx.fillStyle = rgba(C.faint, 0.18); ctx.fillRect(LABEL_W + c0, y, c1 - c0 + 1, row.h - 1); }
    rk.idle = RANK_LOADING;
    return;
  }
  for (const { tile, clip } of tiles) {
    for (const m of row.members) {
      const tr = tileRow(tile, m);
      if (!tr) continue;
      const lo = Math.max(v.start - off, clip ? clip[0] : -Infinity), hi = Math.min(v.start - off + n * v.scale, clip ? clip[1] : Infinity);
      for (const lane of tr.l) {
        if (!lane) continue;
        for (let k = firstBlock(lane, lo); k < lane.length; k += FIELDS) {
          let s = lane[k], e = lane[k + 1];
          if (s >= hi) break;
          if (clip) { if (e < clip[0] || s >= clip[1]) continue; s = Math.max(s, clip[0]); e = Math.min(e, clip[1]); }
          flat.push(off + s, off + e, Math.min(lane[k + 6], e - s));
        }
      }
    }
  }
  const busy = stripScratch.busy = busyColumns(flat, v.start, v.scale, n, stripScratch.busy);
  const c0 = Math.max(Math.ceil((off - v.start) / v.scale), 0), c1 = Math.min(Math.floor((off + T.meta.duration - v.start) / v.scale), n - 1);
  const levelOf = (b) => b >= M.compactIdle ? 1 + Math.floor(b * 3.99) * 2 : 0;
  let idle = 0;
  for (let c = c0; c <= c1;) {
    const level = levelOf(busy[c]);
    let e = c + 1;
    while (e <= c1 && levelOf(busy[e]) === level) e++;
    batchRect(stripStyles()[level], LABEL_W + c, y, e - c, row.h - 1);
    if (level === 0) idle += e - c;
    c = e;
  }
  flushRects();
  rk.idle = c1 >= c0 ? idle / (c1 - c0 + 1) : -1;
}

// drawRankLabels writes each trace's name in its block's gutter, kept in
// view while any of the block is.
function drawRankLabels(W, H) {
  for (const rk of S.ranks) {
    const y0 = RULER_H + rk.y0 - S.scrollY, y1 = RULER_H + rk.y1 - S.scrollY;
    if (y1 < RULER_H || y0 > H) continue;
    // The name as track names are (12 px), then the host and how idle its
    // GPUs are in the view, while the block has room.
    const top = Math.max(y0, RULER_H), ly = Math.min(Math.max(y0 + M.compactStripH + 10, RULER_H + 10), y1 - 8);
    ctx.save(); ctx.beginPath(); ctx.rect(8, top, LABEL_W - 84, y1 - top); ctx.clip();
    text(rankName(rk.t), 12, ly, `600 12px ${FONT}`, rk.t === focusTrace() && S.traces.length > 1 ? C.accentHi : C.text);
    if (y1 - ly >= 22) {
      const host = rk.t.host || "", f = `10.5px ${FONT}`;
      let x = 12;
      if (host) { text(host, x, ly + 15, f, C.faint); x += textW(host, f); }
      if (rk.idle === RANK_LOADING) text((host ? " · " : "") + "loading…", x, ly + 15, f, C.faint);
      if (rk.idle >= 0) {
        const idle = (host ? " · " : "") + `GPU idle ${Math.round(100 * rk.idle)}%`;
        text(idle, x, ly + 15, f, rk.idle > 0.2 ? C.warn : C.faint);
      }
    }
    ctx.restore();
  }
}

// densityCard chooses the density and the rank overview's lanes.
function densityCard() {
  const on = lanesOn(), seg = [["auto", "Auto"], ["on", "Overview"], ["off", "Full"]];
  let h = `<div class="section-title">DENSITY</div><div class="card">
    <span class="segmented wide" id="density" role="group" aria-label="Density">${seg.map(([m, l]) =>
      `<button data-density="${m}" class="${S.compactMode === m ? "on" : ""}">${l}</button>`).join("")}</span>
    <div class="note">Rank overview with more than ${M.compactAuto} traces · C toggles</div>`;
  if (compactOn()) {
    h += `<div class="lanes">${THEME.compactLanes.map((l) =>
      `<label class="lane"><input type="checkbox" data-lane="${l.key}" ${on.includes(l.key) ? "checked" : ""}>${esc(l.label)}</label>`).join("")}</div>
    <span class="segmented wide" id="parallel" role="group" aria-label="Parallel work">${[[false, "Stacked"], [true, "Overlaid"]].map(([m, l]) =>
      `<button data-overlaid="${m ? 1 : 0}" class="${S.overlaid === m ? "on" : ""}">${l}</button>`).join("")}</span>
    <div class="note">Parallel work: ${S.overlaid ? "each kind in one lane, over each other" : "kernels of several streams, nested NVTX ranges side by side"}</div>`;
  }
  return h + "</div>";
}
function bindDensity(sb) {
  sb.querySelectorAll("#density button").forEach((el) => el.addEventListener("click", () => setCompact(el.dataset.density)));
  sb.querySelectorAll("input[data-lane]").forEach((el) => el.addEventListener("change", () => setLane(el.dataset.lane, el.checked)));
  sb.querySelectorAll("#parallel button").forEach((el) => el.addEventListener("click", () => setOverlaid(el.dataset.overlaid === "1")));
}

// ---------------------------------------------------------------- the view

const size = { w: 800, h: 400 };
const dataW = () => Math.max(size.w - LABEL_W, 50);

function clampView(v) {
  const [lo, hi] = extent();
  return clampViewport(v, lo, hi, dataW());
}
function zoomTo(a, b, animate = true) {
  const span = Math.max(b - a, 100), margin = span * 0.02;
  S.target = clampView({ start: a - margin, scale: (span + 2 * margin) / dataW() });
  S.anchorX = dataW() / 2;
  if (animate) S.animating = true; else { S.view = { ...S.target }; S.animating = false; }
  invalidate();
}
function fit(animate = true) { if (S.meta) { const [lo, hi] = extent(); zoomTo(lo, hi, animate); } }
function zoomAt(factor, x) {
  const [lo, hi] = extent();
  S.target = zoomViewport(S.target, factor, x, lo, hi, dataW());
  S.anchorX = x;
  S.animating = true;
  invalidate();
}
// pinchAt zooms by factor around x at once, as a pinch follows the
// fingers, taking over from a zoom animation where it had got to.
function pinchAt(factor, x) {
  const [lo, hi] = extent();
  S.target = zoomViewport(S.target, factor, x, lo, hi, dataW());
  S.view = { ...S.target };
  S.anchorX = x;
  S.animating = false;
  invalidate();
}
function panBy(dx) {
  S.view.start += dx * S.view.scale; S.view = clampView(S.view);
  S.target.start += dx * S.target.scale; S.target = clampView(S.target);
  invalidate();
}
function panAnimated(dx) {
  S.target.start += dx * S.target.scale; S.target = clampView(S.target);
  S.anchorX = dataW() / 2; S.animating = true; invalidate();
}
function clampScroll() {
  const max = Math.max(S.contentH - (size.h - RULER_H) + LANE_H, 0);
  S.scrollY = Math.min(Math.max(S.scrollY, 0), max);
}
function step(now) {
  const dt0 = (now - S.lastT) / 1000;
  S.lastT = now;
  if (!S.animating) return;
  const dt = dt0 <= 0 || dt0 > 0.1 ? 1 / 60 : dt0;
  const k = 1 - Math.exp(-dt / 0.065), x = S.anchorX, v = S.view, t = S.target;
  const scale = Math.exp(Math.log(v.scale) + (Math.log(t.scale) - Math.log(v.scale)) * k);
  const ac = v.start + x * v.scale, at = t.start + x * t.scale, a = ac + (at - ac) * k;
  S.view = { start: a - x * scale, scale };
  if (Math.abs(Math.log(S.view.scale / t.scale)) < 0.002 && Math.abs(S.view.start - t.start) < 0.3 * t.scale) {
    S.view = { ...t };
    S.animating = false;
  }
}
const timeAt = (x) => S.view.start + (x - LABEL_W) * S.view.scale;
const xAt = (t) => LABEL_W + (t - S.view.start) / S.view.scale;

// ---------------------------------------------------------------- tiles
// Each trace has its tiles (on its own time), fetched only while some of its
// rows show.

const levelFor = (scale) => Math.min(MAX_LEVEL, Math.max(MIN_LEVEL, Math.floor(Math.log2(scale))));
const tileSpan = (level) => 512 * Math.pow(2, level);
// Tiles are asked for in batches (/api/tiles: the tiles of every trace a
// frame misses, up to BATCH a request, MAX_BATCHES requests at once (small batches,
// several at once: the first tiles come back sooner)), from
// what each frame wants (S.queue, made anew every frame: a view left is not
// fetched for), nearest first: a coarse tile over a slot with nothing to
// show, then the view's tiles from the middle out, then the neighbors and
// the coarser level for pans and zooms out. In the rank overview they hold
// only the rows and lanes it draws (and the GPU rows, for the busy strips);
// a tile of every row stands in for those.
const BATCH = 16, MAX_BATCHES = 8;
const tileLanesKey = () => compactOn() ? [...new Set([...lanesOn(), "gpu"])].sort().join(",") : "";
const tileKey = (level, index, lanes) => level + ":" + index + ":" + lanes;

function getTile(T, level, index) {
  const lanes = tileLanesKey();
  let t = T.tiles.get(tileKey(level, index, lanes));
  if ((!t || !t.data) && lanes) t = T.tiles.get(tileKey(level, index, ""));
  if (!t || !t.data) return null;
  t.used = performance.now();
  return t;
}
function requestTile(T, level, index, priority) {
  if (index < 0 || level > MAX_LEVEL || index * tileSpan(level) > T.meta.duration) return;
  const lanes = tileLanesKey(), key = tileKey(level, index, lanes);
  if (T.tiles.has(key) || (lanes && T.tiles.get(tileKey(level, index, ""))?.data)) return;
  S.queue.push({ T, level, index, key, lanes, priority });
}
function pump() {
  S.queue.sort((a, b) => a.priority - b.priority);
  const seen = new Set();
  while (S.inflight < MAX_BATCHES && S.queue.length) {
    const batch = [];
    while (batch.length < BATCH && S.queue.length) {
      const j = S.queue.shift(), id = j.T.i + "|" + j.key;
      if (seen.has(id) || j.T.tiles.has(j.key)) continue;
      seen.add(id);
      batch.push(j);
    }
    if (batch.length) fetchTiles(batch);
  }
  updateProgress();
}
function fetchTiles(batch) {
  const traces = [...new Set(batch.map((j) => j.T))], lanes = batch[0].lanes;
  for (const j of batch) j.T.tiles.set(j.key, { level: j.level, index: j.index, data: null, used: performance.now() });
  const t0 = performance.now();
  S.inflight++; // the page's: tiles of every tab share the batches
  const q = { path: traces.map((T) => T.path), t: batch.map((j) => traces.indexOf(j.T) + ":" + j.level + ":" + j.index) };
  if (lanes) q.lanes = lanes;
  api("tiles", q).then((res) => {
    res.tiles.forEach((x, k) => {
      const j = batch[k], T = traces[x.p];
      for (const [n, v] of Object.entries(x.tile.names)) T.names.set(+n, v);
      const e = T.tiles.get(j.key);
      if (e) e.data = x.tile;
    });
    S.perf.tileMs.push(performance.now() - t0);
    if (S.perf.tileMs.length > 100) S.perf.tileMs.shift();
  }).catch((e) => {
    for (const j of batch) { const e2 = j.T.tiles.get(j.key); if (e2 && !e2.data) j.T.tiles.delete(j.key); }
    if (!(e instanceof AuthError)) toast("Tiles: " + e.message);
  }).finally(() => {
    S.inflight--;
    invalidate(); // the next frame asks for what is still missing
    updateProgress();
  });
}
// forgetTiles drops each trace's least recently used tiles beyond a share
// of MAX_TILES.
// With hidden (a tab left), all but M.hiddenTabTiles in all.
function forgetTiles(hidden = false) {
  const n = Math.max(1, loadedTraces().length);
  const per = hidden ? Math.max(8, Math.floor(M.hiddenTabTiles / n)) : Math.max(120, Math.floor(MAX_TILES / n));
  for (const T of S.traces) {
    if (T.tiles.size <= per) continue;
    const old = [...T.tiles.entries()].filter(([, t]) => t.data).sort((a, b) => a[1].used - b[1].used);
    for (const [k] of old.slice(0, T.tiles.size - per)) T.tiles.delete(k);
  }
}
function updateProgress() {
  const p = $("progress"), busy = S.inflight + S.queue.length;
  p.style.opacity = busy ? 1 : 0;
  p.style.width = busy ? Math.max(8, 100 - busy * 12) + "%" : "100%";
}

// tilesOf returns, for the view, the tiles of trace T to draw (once a
// frame): for each tile of the view's level, that tile if loaded, else what
// is loaded of coarser or finer levels over its window. Its times are the
// trace's own: the shared time less its offset.
function tilesOf(T) {
  if (T.frame === S.frameNo) return T.frameTiles;
  T.frame = S.frameNo;
  const off = offsetOf(T);
  const level = levelFor(S.view.scale), span = tileSpan(level);
  const t0 = timeAt(LABEL_W) - off, t1 = timeAt(size.w) - off;
  const first = Math.max(0, Math.floor(t0 / span)), last = Math.floor(t1 / span);
  const center = (t0 + t1) / 2;
  const out = [];
  for (let i = first; i <= last; i++) {
    const prio = Math.abs((i + 0.5) * span - center) / span;
    const tile = getTile(T, level, i);
    if (tile) { out.push({ tile, clip: null }); S.perf.slots.exact++; continue; }
    requestTile(T, level, i, prio);
    const lo = i * span, hi = (i + 1) * span;
    let found = false;
    for (let up = 1; up <= 8 && !found; up++) {
      const l = level + up;
      if (l > MAX_LEVEL) break;
      const t = getTile(T, l, Math.floor(lo / tileSpan(l)));
      if (t) { out.push({ tile: t, clip: [lo, hi] }); found = true; }
    }
    // Finer ones, from a zoom out: one level, then two.
    for (let down = 1; down <= 2 && !found && level - down >= MIN_LEVEL; down++) {
      const n = 1 << down;
      for (let j = n * i; j < n * (i + 1); j++) {
        const t = getTile(T, level - down, j);
        if (t) { out.push({ tile: t, clip: [lo, hi] }); found = true; }
      }
    }
    // Nothing to show here: a coarse tile over it first, fast to come.
    if (!found) requestTile(T, level + 3, Math.floor(lo / tileSpan(level + 3)), -1);
    if (lo <= T.meta.duration) S.perf.slots[found ? "coarse" : "blank"]++;
  }
  // Prefetch a tile each side, and the coarser level, for pans and zooms out.
  requestTile(T, level, first - 1, 50);
  requestTile(T, level, last + 1, 50);
  if (level + 1 <= MAX_LEVEL) {
    const s2 = tileSpan(level + 1);
    for (let i = Math.floor(t0 / s2); i <= Math.floor(t1 / s2); i++) requestTile(T, level + 1, i, 60);
  }
  T.frameTiles = out;
  return out;
}

// ---------------------------------------------------------------- drawing

function invalidate() {
  if (!S.dirty) { S.dirty = true; requestAnimationFrame(frame); }
}
function frame(now) {
  S.dirty = false;
  S.frameNo++;
  // How the frame's tiles stood, slot by slot (a tile of the view's level
  // of a trace drawn): there, a coarser or finer one standing in, or none.
  S.perf.slots = { exact: 0, coarse: 0, blank: 0 };
  S.queue = []; // what this frame wants (pump)
  const moving = stepNav((now - S.lastT) / 1000);
  step(now);
  if (S.meta && (S.target.start !== S.savedStart || S.target.scale !== S.savedScale)) {
    S.savedStart = S.target.start; S.savedScale = S.target.scale;
    saveHash();
  }
  const t0 = performance.now();
  draw();
  S.perf.times.push(performance.now() - t0);
  if (S.perf.times.length > 120) S.perf.times.shift();
  if (S.animating || moving) invalidate();
}

function resize() {
  const r = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  size.w = r.width; size.h = r.height;
  canvas.width = Math.round(r.width * dpr);
  canvas.height = Math.round(r.height * dpr);
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  if (S.meta) { S.view = clampView(S.view); S.target = clampView(S.target); clampScroll(); }
  invalidate();
}
new ResizeObserver(resize).observe($("timeline-wrap"));

// textW is the width of s in font, cached by font, then by text (no key
// strings made a call: labels ask every frame).
const widths = new Map();
function textW(s, font) {
  let byText = widths.get(font);
  if (!byText) widths.set(font, byText = new Map());
  let w = byText.get(s);
  if (w === undefined) {
    ctx.font = font;
    w = ctx.measureText(s).width;
    if (byText.size > 20000) byText.clear();
    byText.set(s, w);
  }
  return w;
}
function text(s, x, y, font, color) {
  ctx.font = font; ctx.fillStyle = color; ctx.fillText(s, x, y);
}
function rrect(x, y, w, h, r) {
  ctx.beginPath();
  if (r > 0 && ctx.roundRect) ctx.roundRect(x, y, w, h, r); else ctx.rect(x, y, w, h);
  ctx.fill();
}

function firstVisibleRow() {
  let lo = 0, hi = S.rows.length;
  while (lo < hi) { const m = (lo + hi) >> 1; if (S.rows[m].y + S.rows[m].h > S.scrollY) hi = m; else lo = m + 1; }
  return lo;
}

function draw() {
  const W = size.w, H = size.h;
  ctx.fillStyle = C.bg;
  ctx.fillRect(0, 0, W, H);
  if (!S.meta) return;
  ctx.textBaseline = "middle";
  const top = RULER_H, step = tickStep(S.view.scale, 110);

  ctx.save();
  ctx.beginPath(); ctx.rect(0, top, W, H - top); ctx.clip();
  // grid
  ctx.fillStyle = C.grid;
  for (let t = Math.floor(S.view.start / step) * step; ; t += step) {
    const x = xAt(t);
    if (x > W) break;
    if (x >= LABEL_W) ctx.fillRect(x, top, 1, H - top);
  }
  // What was drawn where, for hit testing: trace, row, lane, offset, x0,
  // x1, y, height per box (BOX values), and the lane values it came from.
  // (Reused frame to frame: what a frame allocates, the collector pays.)
  if (!S.boxes) { S.boxes = []; S.boxFlats = []; }
  S.boxes.length = 0; S.boxFlats.length = 0;
  for (let ri = firstVisibleRow(); ri < S.rows.length; ri++) {
    const row = S.rows[ri], y = top + row.y - S.scrollY;
    if (y > H) break;
    if (row.kind === "group") drawGroup(row, y, W);
    else if (row.kind === "track") drawTrack(row, ri, y, W, H);
    else if (row.kind === "trace") drawTraceHeading(row, y, W);
    else if (row.kind === "pending") drawPending(row, y, W);
    else if (row.kind === "idle") {
      ctx.fillStyle = C.panel; ctx.fillRect(0, y, LABEL_W, row.h);
      ctx.fillStyle = C.border; ctx.fillRect(LABEL_W - 1, y, 1, row.h);
      text("No activity", 22, y + row.h / 2, `12px ${FONT}`, C.faint);
      text("no kernels, memcpy or memset were recorded on this GPU", LABEL_W + 12, y + row.h / 2, `11.5px ${FONT}`, C.faint);
    } else if (row.kind === "toggle") drawToggle(row, y, W);
    else if (row.kind === "strip") drawStrip(row, y, W);
    else if (row.kind === "summary") drawSummary(row, ri, y, W);
  }
  drawRankLabels(W, H);
  drawOverlays(W, H);
  ctx.restore();
  drawRuler(W, step);
  pump();
  if (S.inflight + S.queue.length > 0) drawLoadingBar(W);
  if (S.perf.show) drawPerf(W, H);
  forgetTiles();
}

// traceLabel names a trace: its file, and the host and rank it was on.
function traceLabel(T) {
  const extra = [T.host, T.rank >= 0 ? "rank " + T.rank : ""].filter(Boolean).join(" · ");
  return { name: T.name, extra };
}

// drawTraceHeading draws the heading of a trace of a session: what it is,
// whether it is loaded, and how it is aligned.
function drawTraceHeading(row, y, W) {
  const T = row.t, cy = y + TRACE_H / 2;
  ctx.fillStyle = C.panel; ctx.fillRect(0, y, W, TRACE_H);
  ctx.fillStyle = rgba(C.accent, T === focusTrace() && S.traces.length > 1 ? 0.10 : 0.04); ctx.fillRect(0, y, W, TRACE_H);
  ctx.fillStyle = C.border; ctx.fillRect(0, y + TRACE_H - 1, W, 1); ctx.fillRect(0, y, W, 1);
  ctx.fillStyle = C.accent; ctx.fillRect(0, y + 1, 3, TRACE_H - 2);
  const cx = 16;
  ctx.strokeStyle = C.text; ctx.lineWidth = 1.8; ctx.beginPath();
  if (T.folded) { ctx.moveTo(cx - 2, cy - 5); ctx.lineTo(cx + 3, cy); ctx.lineTo(cx - 2, cy + 5); }
  else { ctx.moveTo(cx - 5, cy - 2); ctx.lineTo(cx, cy + 3); ctx.lineTo(cx + 5, cy - 2); }
  ctx.stroke();
  const { name, extra } = traceLabel(T);
  const f = `700 12.5px ${FONT}`;
  text(name, 30, cy, f, C.text);
  let x = 30 + textW(name, f) + 10;
  if (extra) { text(extra, x, cy, `12px ${FONT}`, C.muted); x += textW(extra, `12px ${FONT}`) + 14; }
  const st = T.error ? T.error : T.loading ? loadingText(T) : !T.meta ? (T.folded ? "not loaded · click to open" : "") :
    `${fmtCount(T.meta.events)} events · ${fmtDur(T.meta.duration)}` +
    (S.traces.length > 1 && S.align !== "start" ? " · " + alignNote(T) : "");
  text(st, x, cy, `11.5px ${FONT}`, T.error ? C.warn : C.faint);
}

function drawPending(row, y, W) {
  const T = row.t;
  ctx.fillStyle = C.panel; ctx.fillRect(0, y, LABEL_W, row.h);
  ctx.fillStyle = C.border; ctx.fillRect(LABEL_W - 1, y, 1, row.h);
  const msg = T.error ? "Could not load: " + T.error : "Loading " + T.name + "… " + (T.progress ? Math.round(T.progress.fraction * 100) + "%" : "");
  text(msg, 22, y + row.h / 2, `12px ${FONT}`, T.error ? C.warn : C.faint);
  if (!T.error && !T.loading) queueMicrotask(() => loadMeta(T)); // shown: load it
}

function drawGroup(row, y, W) {
  ctx.fillStyle = C.panelAlt; ctx.fillRect(0, y, W, GROUP_H);
  ctx.fillStyle = C.border; ctx.fillRect(0, y + GROUP_H - 1, W, 1);
  const collapsed = row.t.collapsed.has(row.group), cx = 14, cy = y + GROUP_H / 2;
  ctx.strokeStyle = C.muted; ctx.lineWidth = 1.5; ctx.beginPath();
  if (collapsed) { ctx.moveTo(cx - 2, cy - 4); ctx.lineTo(cx + 2, cy); ctx.lineTo(cx - 2, cy + 4); }
  else { ctx.moveTo(cx - 4, cy - 2); ctx.lineTo(cx, cy + 2); ctx.lineTo(cx + 4, cy - 2); }
  ctx.stroke();
  const f = `600 12px ${FONT}`;
  text(row.group, 26, cy, f, row.idle ? C.muted : C.text);
  const extra = row.idle ? "   idle" : `   ${fmtCount(row.tracks)} rows · ${fmtCount(row.events)} events`;
  text(extra, 26 + textW(row.group, f), cy, `11px ${FONT}`, C.faint);
}

function drawToggle(row, y, W) {
  ctx.fillStyle = C.ruler; ctx.fillRect(0, y, W, row.h);
  ctx.fillStyle = C.border; ctx.fillRect(0, y + row.h - 1, W, 1);
  const T = row.t, n = T.meta.idle.length;
  const msg = `${n} ${n === 1 ? "GPU" : "GPUs"} with no activity ${T.showIdle ? "shown" : "hidden"}  ·  `;
  const f = `11.5px ${FONT}`;
  text(msg, 26, y + row.h / 2, f, C.faint);
  const hover = S.mouse && rowAt(S.mouse.y) >= 0 && S.rows[rowAt(S.mouse.y)] === row;
  text(T.showIdle ? "Hide" : "Show", 26 + textW(msg, f), y + row.h / 2, `600 ${f}`, hover ? C.accentHi : C.accent);
}

function drawTrack(row, ri, y, W, H) {
  const T = row.t, meta = T.meta.rows[row.track], off = offsetOf(T);
  const tiles = tilesOf(T);
  if (ri % 2 === 0) { ctx.fillStyle = C.rowAlt; ctx.fillRect(LABEL_W, y, W - LABEL_W, row.h); }
  ctx.fillStyle = C.grid; ctx.fillRect(0, y + row.h - 1, W, 1);
  const v = S.view;
  for (let li = 0; li < meta.lanes; li++) {
    const ly = y + row.pad + li * row.lh;
    if (ly > H || ly + row.lh < RULER_H) continue;
    for (const { tile, clip } of tiles) {
      const tr = tileRow(tile, row.track);
      if (!tr || !tr.l[li]) continue;
      if (clip) {
        ctx.save(); ctx.beginPath();
        ctx.rect(xAt(off + clip[0]), ly, (clip[1] - clip[0]) / v.scale, row.lh); ctx.clip();
      }
      drawBlocks(tr.l[li], T, off, row.track, li, ly, W, false, row.lh);
      if (clip) ctx.restore();
    }
  }
  // the gutter
  ctx.fillStyle = C.panel; ctx.fillRect(0, y, LABEL_W, row.h);
  ctx.fillStyle = C.border; ctx.fillRect(LABEL_W - 1, y, 1, row.h);
  ctx.fillStyle = C.grid; ctx.fillRect(0, y + row.h - 1, LABEL_W, 1);
  ctx.fillStyle = { api: C.swatchRuntime, nvtx: C.swatchNVTX, cpu: C.swatchCPU, python: C.swatchPython }[meta.kind] || C.swatchStream;
  // The name and swatch centered on the first lane.
  const ny = y + row.pad + (row.lh - LANE_H) / 2;
  rrect(12, ny + 5, 3, LANE_H - 10, 1.5);
  ctx.save(); ctx.beginPath(); ctx.rect(20, y, LABEL_W - 28, row.h); ctx.clip();
  text(meta.name, 22, ny + LANE_H / 2, `12px ${FONT}`, C.text);
  if (row.h >= 2 * row.lh) {
    let info = fmtCount(meta.count) + " events";
    if (meta.kind === "stream" && T.meta.duration > 0) info += " · " + trim(100 * meta.busy / T.meta.duration, 1) + "% busy";
    if (meta.lanes > 1) info += " · depth " + meta.lanes;
    text(info, 22, y + row.pad + row.lh * 1.5, `10.5px ${FONT}`, C.faint);
  }
  ctx.restore();
}

// drawBlocks draws a lane's blocks of a tile. batched (the overview's
// summary lanes, which draw every trace at once): the merged and the narrow
// blocks are queued by color for flushRects, the wide ones, with their
// names, for drawWide, on top; the look is the same. lh is the lane's
// height.
function drawBlocks(flat, T, off, row, lane, ly, W, batched, lh = LANE_H) {
  const v = S.view, x0v = LABEL_W - 4, x1v = W + 4, by = ly + 1, bh = lh - 3, base = v.start - off;
  const hl = S.highlight;
  // Only the blocks in the view: a lane's blocks are apart and in order.
  const tEnd = base + (x1v - LABEL_W) * v.scale;
  for (let k = firstBlock(flat, base + (x0v - LABEL_W) * v.scale); k < flat.length; k += FIELDS) {
    const s = flat[k], e = flat[k + 1];
    if (s > tEnd) break;
    let x0 = LABEL_W + (s - base) / v.scale, x1 = LABEL_W + (e - base) / v.scale;
    if (x1 < x0v || x0 > x1v) continue;
    x0 = Math.max(x0, x0v); x1 = Math.min(x1, x1v);
    const w = Math.max(x1 - x0, 1);
    const name = flat[k + 2], kind = flat[k + 3], count = flat[k + 4];
    const col = colorOf(T, name, kind);
    const dim = hl !== null && (count > 1 || T.names.get(name) !== hl);
    if (count > 1) {
      const dens = e > s ? Math.min(flat[k + 6] / (e - s), 1) : 1;
      let a = 0.3 + 0.6 * dens;
      if (dim) a *= 0.3;
      if (batched) { columnBlock(col, a, x0, w, false, T.i, row, lane, k, flat); continue; }
      else {
        ctx.fillStyle = col.merged(a); ctx.fillRect(x0, by, w, bh);
        ctx.fillStyle = col.line(a); ctx.fillRect(x0, by, w, 1);
      }
    } else if (kind === KIND.mark) {
      if (batched) queueWide(col, null, x0, ly, 0, 0, -1);
      else drawMark(col, x0, ly, lh);
    } else if (batched && w < 3) {
      columnBlock(col, dim ? 0.3 : 1, x0, w, dim, T.i, row, lane, k, flat);
      continue;
    } else if (batched) {
      queueWide(dim ? col.dim : col.solid, w >= M.labelMinW ? T.names.get(name) || "" : null, x0, by, w, bh, e - s);
    } else {
      ctx.fillStyle = dim ? col.dim : col.solid;
      // A seam between events back to back (as the desktop app's).
      const bw = w >= 4 ? w - 1 : w;
      rrect(x0, by, bw, bh, w > 6 ? 3 : 0);
      if (w >= M.labelMinW) label(T.names.get(name) || "", e - s, x0, by, bw, bh);
    }
    S.boxes.push(T.i, row, lane, k, x0, x0 + w, by, bh);
    S.boxFlats.push(flat);
  }
}
function drawMark(col, x0, ly, lh = LANE_H) {
  ctx.fillStyle = col.solid; ctx.beginPath();
  const cy = ly + lh / 2;
  ctx.moveTo(x0, cy - 6); ctx.lineTo(x0 + 5, cy); ctx.lineTo(x0, cy + 6); ctx.lineTo(x0 - 5, cy); ctx.fill();
}
// A summary lane draws the blocks of many rows over each other (every
// stream of a GPU): its merged and narrow ones go into a buffer of pixel
// columns, each keeping the strongest (the most opaque, then the first),
// drawn as runs by flushColumns: a column's worth, whatever the rows.
// Each column also keeps where its block came from (trace, row, lane,
// offset in the tile's lane, the lane), for hit testing: a box a run, not
// one a block.
const cols = { alpha: new Float32Array(0), col: [], dim: [], hit: [] };
function columnBlock(col, a, x0, w, dim, ti, row, lane, k, flat) {
  const c0 = Math.max(0, Math.floor(x0 - LABEL_W)), c1 = Math.min(cols.alpha.length, Math.ceil(x0 + w - LABEL_W));
  for (let c = c0; c < c1; c++) {
    if (a > cols.alpha[c]) {
      cols.alpha[c] = a; cols.col[c] = col; cols.dim[c] = dim;
      const h = cols.hit[c] || (cols.hit[c] = [0, 0, 0, 0, null]);
      h[0] = ti; h[1] = row; h[2] = lane; h[3] = k; h[4] = flat;
    }
  }
}
function resetColumns(W) {
  const n = Math.max(0, Math.ceil(W - LABEL_W) + 1);
  if (cols.alpha.length !== n) { cols.alpha = new Float32Array(n); cols.col = new Array(n); cols.dim = new Array(n); cols.hit = new Array(n); }
  else cols.alpha.fill(0);
}
// flushColumns draws the columns for a lane at by, bh high: as a picture
// one pixel high, a column a pixel, stretched to the lane (one drawing,
// where runs of rectangles would be thousands at a zoom out), and the
// boxes hit testing finds, a run of one block each.
const colImg = { canvas: null, ctx: null, img: null, n: 0 };
function flushColumns(by, bh) {
  const n = cols.alpha.length;
  if (!n) return;
  if (colImg.n !== n) {
    colImg.canvas = document.createElement("canvas");
    colImg.canvas.width = n; colImg.canvas.height = 1;
    colImg.ctx = colImg.canvas.getContext("2d");
    colImg.img = colImg.ctx.createImageData(n, 1);
    colImg.n = n;
  }
  const d = colImg.img.data;
  d.fill(0);
  let any = false;
  for (let c = 0; c < n; c++) {
    const a = cols.alpha[c];
    if (!a) continue;
    const col = cols.col[c], rgb = a >= 1 ? (cols.dim[c] ? col.dimRGB : col.rgb) : col.mergedRGB, o = 4 * c;
    d[o] = rgb[0]; d[o + 1] = rgb[1]; d[o + 2] = rgb[2]; d[o + 3] = Math.round(Math.min(a, 1) * 255);
    any = true;
  }
  if (any) {
    colImg.ctx.putImageData(colImg.img, 0, 0);
    const smooth = ctx.imageSmoothingEnabled;
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(colImg.canvas, 0, 0, n, 1, LABEL_W, by, n, bh);
    ctx.imageSmoothingEnabled = smooth;
  }
  // Hit testing: runs of one block.
  for (let c = 0; c < n;) {
    if (!cols.alpha[c]) { c++; continue; }
    const h = cols.hit[c];
    let e = c + 1;
    while (e < n && cols.alpha[e] && cols.hit[e][3] === h[3] && cols.hit[e][4] === h[4]) e++;
    S.boxes.push(h[0], h[1], h[2], h[3], LABEL_W + c, LABEL_W + e, by, bh);
    S.boxFlats.push(h[4]);
    c = e;
  }
  cols.alpha.fill(0);
}

// firstBlock is the offset in a tile's lane of its first block ending at or
// after t (the blocks are apart and in order).
function firstBlock(flat, t) {
  let lo = 0, hi = flat.length / FIELDS;
  while (lo < hi) { const m = (lo + hi) >> 1; if (flat[m * FIELDS + 1] < t) lo = m + 1; else hi = m; }
  return lo * FIELDS;
}
// The batched lanes' wide blocks (and marks: dur -1), for drawWide, in
// flat arrays reused frame to frame.
const wide = { n: 0, num: [], ref: [] };
function queueWide(style, name, x, y, w, h, dur) {
  const i = wide.n++;
  wide.num[5 * i] = x; wide.num[5 * i + 1] = y; wide.num[5 * i + 2] = w; wide.num[5 * i + 3] = h; wide.num[5 * i + 4] = dur;
  wide.ref[2 * i] = style; wide.ref[2 * i + 1] = name;
}
function drawWide() {
  const a = wide.num, r = wide.ref;
  for (let i = 0; i < wide.n; i++) {
    const x = a[5 * i], y = a[5 * i + 1], w = a[5 * i + 2], h = a[5 * i + 3], dur = a[5 * i + 4];
    if (dur < 0) { drawMark(r[2 * i], x, y); continue; }
    ctx.fillStyle = r[2 * i];
    const bw = w >= 4 ? w - 1 : w; // a seam, as drawBlocks'
    rrect(x, y, bw, h, 3);
    if (r[2 * i + 1] !== null) label(r[2 * i + 1], dur, x, y, bw, h);
  }
  wide.n = 0;
  r.fill(null);
}

function label(name, dur, x, y, w, h) {
  if (name.length > 200) name = name.slice(0, 200);
  const f = `500 11px ${FONT}`, nw = textW(name, f), pad = w < 40 ? 3 : 5; // narrow: a letter or two more
  const cy = y + h / 2;
  if (nw + 2 * pad > w) {
    ctx.save(); ctx.beginPath(); ctx.rect(x + 2, y, w - 4, h); ctx.clip();
    text(name, x + pad, cy, f, C.onEvent);
    ctx.restore();
    return;
  }
  text(name, x + pad, cy, f, C.onEvent);
  const d = "  " + fmtDur(dur), df = `10px ${FONT}`;
  if (w > nw + 2 * pad + 60 && nw + textW(d, df) + 2 * pad <= w) text(d, x + pad + nw, cy, df, rgba(C.onEvent, 0.65));
}

// trackY is where lane of a track is drawn: its top and height (in the
// compact view, its summary lane's), or null.
function trackY(T, track, lane) {
  for (const r of S.rows) {
    if (r.kind === "track" && r.t === T && r.track === track) return { y: RULER_H + r.y - S.scrollY + r.pad + lane * r.lh, h: r.lh };
    if (r.kind === "summary" && r.t === T && r.members.includes(track) && slotOf(T.meta.rows[track], lane) === r.slot) return { y: RULER_H + r.y - S.scrollY, h: r.h };
  }
  return null;
}
// eventBox is where an event (as /api/event gives it, with its trace's
// index ti) is drawn, if its row shows.
function eventBox(ev) {
  const T = S.traces[ev.ti];
  if (!T) return null;
  const ty = trackY(T, ev.row, ev.lane);
  if (ty === null) return null;
  const off = offsetOf(T);
  const x0 = Math.max(xAt(off + ev.start), LABEL_W - 4), x1 = Math.min(xAt(off + ev.end), size.w + 4);
  return { x: x0, y: ty.y + 1, w: Math.max(x1 - x0, 1), h: ty.h - 3 };
}
function strokeBox(b, color, width, grow) {
  ctx.strokeStyle = color; ctx.lineWidth = width; ctx.beginPath();
  if (ctx.roundRect) ctx.roundRect(b.x - grow, b.y - grow, b.w + 2 * grow, b.h + 2 * grow, 3);
  else ctx.rect(b.x - grow, b.y - grow, b.w + 2 * grow, b.h + 2 * grow);
  ctx.stroke();
}

// linkChip draws a chip naming the linked event where the flow line
// leaves the view, at x, y (kept inside it); a click goes to it.
function linkChip(x, y, arrow, name, W, H) {
  const f = `600 11.5px ${FONT}`, t = `${arrow} ${name.length > 48 ? name.slice(0, 47) + "…" : name}  ·  Go to`, w = textW(t, f) + 16, h = 22;
  const bx = Math.min(Math.max(x - w / 2, LABEL_W + 4), W - w - 4), by = Math.min(Math.max(y - h / 2, RULER_H + 4), H - h - 4);
  const m = S.mouse, hot = m && m.x >= bx && m.x <= bx + w && m.y >= by && m.y <= by + h;
  ctx.shadowColor = C.shadow; ctx.shadowBlur = 6;
  ctx.fillStyle = hot ? C.accent : C.accentHi;
  ctx.beginPath(); if (ctx.roundRect) ctx.roundRect(bx, by, w, h, 6); else ctx.rect(bx, by, w, h); ctx.fill();
  ctx.shadowBlur = 0;
  text(t, bx + 8, by + h / 2, f, C.accentText);
  S.linkChip = { x: bx, y: by, w, h };
}

function drawOverlays(W, H) {
  ctx.save(); ctx.beginPath(); ctx.rect(LABEL_W, RULER_H, W - LABEL_W, H - RULER_H); ctx.clip();
  if (S.range) {
    const a = xAt(Math.min(S.range.a, S.range.b)), b = xAt(Math.max(S.range.a, S.range.b));
    ctx.fillStyle = rgba(C.range, 0.10); ctx.fillRect(a, RULER_H, Math.max(b - a, 1), H);
    ctx.fillStyle = rgba(C.range, 0.6); ctx.fillRect(a, RULER_H, 1, H); ctx.fillRect(b, RULER_H, 1, H);
  }
  const sel = S.selected;
  S.linkChip = null;
  if (sel) {
    const b = eventBox(sel);
    if (sel.linked) {
      const o = eventBox(sel.linked);
      // The other end out of the view (its row scrolled away, or before or
      // after the times shown): the line goes to the view's edge, where a
      // chip names it (as the desktop app's).
      let x1 = o ? o.x + Math.min(o.w / 2, 6) : 0, y1 = o ? o.y + o.h / 2 : 0;
      const out = o && (x1 < LABEL_W || x1 > W || y1 < RULER_H || y1 > H);
      if (o && b) {
        const arrow = y1 < RULER_H ? "↑" : y1 > H ? "↓" : x1 < LABEL_W ? "←" : "→";
        if (out) { x1 = Math.min(Math.max(x1, LABEL_W + 10), W - 10); y1 = Math.min(Math.max(y1, RULER_H + 10), H - 10); }
        const x0 = b.x + Math.min(b.w / 2, 6), y0 = b.y + b.h / 2, my = (y0 + y1) / 2;
        ctx.strokeStyle = rgba(C.accentHi, 0.85); ctx.lineWidth = 1.5;
        ctx.beginPath(); ctx.moveTo(x0, y0); ctx.bezierCurveTo(x0, my, x1, my, x1, y1); ctx.stroke();
        if (out) linkChip(x1, y1, arrow, sel.linked.name, W, H);
      }
      if (o && !out) strokeBox(o, C.accentHi, 1.5, 1.5);
      if (b && !o) linkChip(b.x + b.w / 2, b.y - 14, "↗", sel.linked.name, W, H); // not laid out (a lane off, a row hidden)
    }
    if (b) {
      ctx.shadowColor = rgba(C.selection, 0.35); ctx.shadowBlur = 8;
      strokeBox(b, C.selection, 2, 1.5);
      ctx.shadowBlur = 0;
    }
  }
  const m = S.mouse;
  if (m && !S.dragging && m.x > LABEL_W && m.y > RULER_H) {
    ctx.fillStyle = rgba(C.text, 0.25); ctx.fillRect(m.x, RULER_H, 1, H);
    const h = hitAt(m.x, m.y);
    if (h && h.count === 1) strokeBox({ x: h.x0, y: h.y, w: h.x1 - h.x0, h: h.h }, rgba(C.text, 0.8), 1, 1);
  }
  ctx.restore();
}

function drawRuler(W, step) {
  ctx.fillStyle = C.ruler; ctx.fillRect(0, 0, W, RULER_H);
  ctx.fillStyle = C.border; ctx.fillRect(0, RULER_H - 1, W, 1);
  ctx.save(); ctx.beginPath(); ctx.rect(LABEL_W, 0, W - LABEL_W, RULER_H); ctx.clip();
  const minor = step / 5;
  for (let t = Math.floor(S.view.start / minor) * minor; ; t += minor) {
    const x = xAt(t);
    if (x > W + 2) break;
    const major = Math.abs(t / step - Math.round(t / step)) < 0.1;
    if (major) {
      ctx.fillStyle = C.muted; ctx.fillRect(x, RULER_H - 10, 1, 10);
      if (x >= LABEL_W) text(fmtTick(t, step), x + 4, 13, `10.5px ${FONT}`, C.muted);
    } else { ctx.fillStyle = C.faint; ctx.fillRect(x, RULER_H - 5, 1, 5); }
  }
  if (S.range) {
    const a = xAt(Math.min(S.range.a, S.range.b)), b = xAt(Math.max(S.range.a, S.range.b));
    ctx.fillStyle = rgba(C.range, 0.25); rrect(a, 2, Math.max(b - a, 1), RULER_H - 4, 3);
    // The span, and an × that clears the range.
    const l = fmtDur(Math.abs(S.range.b - S.range.a)), f = `600 11px ${FONT}`, lw = textW(l, f);
    const lx = Math.max(a + (b - a - lw - 18) / 2, a + 4);
    text(l, lx, RULER_H / 2, f, C.text);
    const cx = lx + lw + 11, cy = RULER_H / 2;
    const hot = S.mouse && S.mouse.y < RULER_H && Math.abs(S.mouse.x - cx) <= 8;
    ctx.fillStyle = rgba(C.range, hot ? 0.55 : 0.3); ctx.beginPath(); ctx.arc(cx, cy, 7, 0, 2 * Math.PI); ctx.fill();
    ctx.strokeStyle = C.text; ctx.lineWidth = 1.4; ctx.beginPath();
    ctx.moveTo(cx - 3, cy - 3); ctx.lineTo(cx + 3, cy + 3); ctx.moveTo(cx + 3, cy - 3); ctx.lineTo(cx - 3, cy + 3); ctx.stroke();
    S.rangeClose = { x: cx, y: cy };
  } else S.rangeClose = null;
  const m = S.mouse;
  if (m && m.x > LABEL_W) {
    const l = fmtTime(timeAt(m.x), S.view.scale), f = `600 10.5px ${FONT}`, w = textW(l, f);
    const bx = Math.min(m.x + 1, W - w - 10);
    ctx.fillStyle = C.inverse; rrect(bx, 5, w + 10, 18, 4);
    text(l, bx + 5, 14, f, C.inverseText);
  }
  ctx.restore();
  ctx.fillStyle = C.panel; ctx.fillRect(0, 0, LABEL_W, RULER_H - 1);
  ctx.fillStyle = C.border; ctx.fillRect(LABEL_W - 1, 0, 1, RULER_H);
  if (S.inflight + S.queue.length > 0) text("Loading the view…", 12, RULER_H / 2, `600 11px ${FONT}`, C.accent); // its tiles still coming
  else {
    text("View ", 12, RULER_H / 2, `11px ${FONT}`, C.faint);
    text(fmtDur(S.view.scale * dataW()), 12 + textW("View ", `11px ${FONT}`), RULER_H / 2, `600 11px ${FONT}`, C.muted);
  }
}

function pct(a, p) { const s = [...a].sort((x, y) => x - y); return s.length ? s[Math.min(s.length - 1, Math.floor(p * s.length))] : 0; }
function drawPerf(W, H) {
  const t = S.perf.times, tm = S.perf.tileMs;
  const lines = [
    `draw ${t.length ? t[t.length - 1].toFixed(2) : "–"} ms · p50 ${pct(t, .5).toFixed(2)} · p95 ${pct(t, .95).toFixed(2)}`,
    `tiles: ${S.traces.reduce((n, T) => n + [...T.tiles.values()].filter((x) => x.data).length, 0)} cached in ${loadedTraces().length} traces, ${S.inflight + S.queue.length} loading`,
    `tile round trip p50 ${pct(tm, .5).toFixed(0)} ms · p95 ${pct(tm, .95).toFixed(0)} ms`,
    `level ${levelFor(S.view.scale)} · ${fmtCount(S.boxes.length / BOX)} blocks drawn · P hides`,
  ];
  const f = `11px ${FONT}`, w = Math.max(...lines.map((l) => textW(l, f))) + 20, h = lines.length * 16 + 12;
  const x = W - w - 14, y = H - h - 14;
  ctx.fillStyle = rgba(C.tooltip, 0.94); rrect(x, y, w, h, 8);
  lines.forEach((l, i) => text(l, x + 10, y + 14 + i * 16, f, i ? C.muted : C.text));
}

// ---------------------------------------------------------------- hit testing

function rowAt(y) {
  const cy = y - RULER_H + S.scrollY;
  let lo = 0, hi = S.rows.length;
  while (lo < hi) { const m = (lo + hi) >> 1; if (S.rows[m].y + S.rows[m].h > cy) hi = m; else lo = m + 1; }
  return lo < S.rows.length && S.rows[lo].y <= cy ? lo : -1;
}

// hitAt finds the block drawn under x, y (or within 3 pixels of x).
function hitAt(x, y) {
  if (!S.boxes || y < RULER_H || x < LABEL_W) return null;
  let best = null, bestD = 4;
  const b = S.boxes;
  // From the last drawn: where blocks overlap (a compact lane's streams),
  // the one on top.
  for (let i = b.length - BOX; i >= 0; i -= BOX) {
    const by = b[i + 6];
    if (y < by - 1 || y > by + b[i + 7] + 1) continue;
    const d = x < b[i + 4] ? b[i + 4] - x : x > b[i + 5] ? x - b[i + 5] : 0;
    if (d < bestD) { bestD = d; best = i; if (d === 0) break; }
  }
  if (best === null) return null;
  const T = S.traces[b[best]], row = b[best + 1], lane = b[best + 2], k = b[best + 3], flat = S.boxFlats[best / BOX];
  return {
    t: T, off: offsetOf(T), row, lane, x0: b[best + 4], x1: b[best + 5], y: b[best + 6], h: b[best + 7],
    start: flat[k], end: flat[k + 1], name: flat[k + 2], kind: flat[k + 3], count: flat[k + 4], index: flat[k + 5], busy: flat[k + 6],
  };
}

// ---------------------------------------------------------------- tooltip

function updateTooltip() {
  const tip = $("tooltip"), m = S.mouse;
  const h = m && !S.dragging ? hitAt(m.x, m.y) : null;
  if (!h) { tip.hidden = true; return; }
  const name = h.t.names.get(h.name) || "";
  const track = h.t.meta.rows[h.row];
  const col = colorOf(h.t, h.name, h.kind);
  const of = S.traces.length > 1 ? `<div class="f">${esc(h.t.name)}${h.t.host ? " · " + esc(h.t.host) : ""}</div>` : "";
  if (h.count > 1) {
    tip.innerHTML = `<div class="t">${fmtCount(h.count)} events</div>
      <div>busy ${trim(100 * h.busy / Math.max(h.end - h.start, 1), 1)}% of ${fmtDur(h.end - h.start)}</div>
      <div><span class="f">first: </span>${esc(name)}</div><div class="f">zoom in, or click, to tell them apart</div>${of}`;
  } else {
    tip.innerHTML = `<div class="t">${esc(name)}</div>
      <div><span class="kind">${KIND_NAME[h.kind] || "Event"}</span><span class="f">  ·  ${esc(track.name)}</span></div>
      <div><span class="f">Duration </span><b>${fmtDur(h.end - h.start)}</b></div>
      <div><span class="f">Start </span>${fmtTime(h.off + h.start, 0.1)}</div>${of}`;
  }
  // (Colors through the CSSOM: the page's CSP forbids inline style attributes.)
  tip.style.setProperty("--swatch", col.solid);
  const kind = tip.querySelector(".kind");
  if (kind) kind.style.color = col.solid;
  tip.hidden = false;
  const r = tip.getBoundingClientRect();
  let x = m.x + 16, y = m.y + 18;
  if (x + r.width > size.w - 4) x = m.x - r.width - 12;
  if (y + r.height > size.h - 4) y = m.y - r.height - 10;
  tip.style.left = x + "px"; tip.style.top = y + "px";
}

// ---------------------------------------------------------------- input

function local(e) { const r = canvas.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; }

canvas.addEventListener("wheel", (e) => {
  if (!S.meta) return;
  e.preventDefault();
  const scale = e.deltaMode === 1 ? 33 : e.deltaMode === 2 ? size.h : 1;
  const dx = e.deltaX * scale, dy = e.deltaY * scale, p = local(e);
  if (e.ctrlKey || e.metaKey || e.altKey) {
    const x = Math.max(p.x - LABEL_W, 0);
    // A mouse wheel's notches come as multiples of 120 in wheelDeltaY;
    // anything else is a touchpad's, precise.
    const notch = e.deltaMode !== 0 || (e.wheelDeltaY && e.wheelDeltaY % 120 === 0);
    if (notch) zoomAt(Math.pow(ZOOM_PER_NOTCH, dy / 100), x); // smoothly, a notch a step
    else if (e.ctrlKey && !e.metaKey && !e.altKey) {
      // A touchpad's pinch, which Chrome and Firefox send as a wheel with
      // ctrlKey, deltaY 100 times the natural log of the scale: the
      // content follows the fingers at once.
      pinchAt(Math.exp(dy / 100), x);
    } else pinchAt(Math.pow(ZOOM_PER_NOTCH, dy / 100), x); // ⌘/Alt + touchpad scrolling
  } else if (e.shiftKey && dx === 0) {
    panBy(dy);
  } else {
    if (dx) panBy(dx);
    if (dy) { S.scrollY += dy; clampScroll(); invalidate(); }
  }
}, { passive: false });

// Safari sends a touchpad's pinch as gesture events, whose scale is from 1
// as the pinch began; without preventDefault they zoom the whole page.
let gestureScale = 1;
const gesture = (e) => {
  e.preventDefault();
  if (!S.meta) return;
  if (e.type === "gesturestart") gestureScale = 1;
  if (e.scale > 0 && gestureScale > 0 && e.scale !== gestureScale) {
    pinchAt(gestureScale / e.scale, Math.max(local(e).x - LABEL_W, 0));
  }
  gestureScale = e.scale;
};
for (const type of ["gesturestart", "gesturechange", "gestureend"]) canvas.addEventListener(type, gesture, { passive: false });

canvas.addEventListener("pointerdown", (e) => {
  if (!S.meta || e.button !== 0) return;
  canvas.setPointerCapture(e.pointerId);
  const p = local(e);
  const rc = S.rangeClose;
  if (rc && Math.abs(p.x - rc.x) <= 9 && Math.abs(p.y - rc.y) <= 9) { clearRange(); return; }
  const lc = S.linkChip;
  if (lc && p.x >= lc.x && p.x <= lc.x + lc.w && p.y >= lc.y && p.y <= lc.y + lc.h) { goToLinked(); return; }
  if (onGutterEdge(p.x)) { S.gutterDrag = true; return; }
  S.dragging = { x: p.x, y: p.y, start: S.view.start, scroll: S.scrollY, moved: false, range: e.shiftKey && p.x > LABEL_W };
  if (S.dragging.range) S.range = { a: timeAt(p.x), b: timeAt(p.x) };
});
canvas.addEventListener("pointermove", (e) => {
  const p = local(e);
  S.mouse = p;
  if (S.gutterDrag) { LABEL_W = clampLabelW(p.x); invalidate(); return; }
  canvas.style.cursor = !S.dragging && onGutterEdge(p.x) ? "col-resize" : S.dragging?.moved && !S.dragging.range ? "grabbing" : "";
  const d = S.dragging;
  if (d) {
    const dx = p.x - d.x, dy = p.y - d.y;
    if (Math.abs(dx) > 3 || Math.abs(dy) > 3) d.moved = true;
    if (d.range) S.range.b = timeAt(p.x);
    else if (d.moved) {
      S.view.start = d.start - dx * S.view.scale;
      S.view = clampView(S.view); S.target = { ...S.view }; S.animating = false;
      S.scrollY = d.scroll - dy; clampScroll();
      canvas.style.cursor = "grabbing";
    }
  }
  invalidate();
  requestAnimationFrame(updateTooltip);
});
canvas.addEventListener("pointerup", (e) => {
  if (S.gutterDrag) { S.gutterDrag = false; localStorage.setItem("nsa-label-w", String(Math.round(LABEL_W))); return; }
  const d = S.dragging;
  S.dragging = null;
  canvas.style.cursor = "";
  if (!d) return;
  const p = local(e);
  if (d.range) { if (d.moved) rangeChanged(); else clearRange(); }
  if (!d.moved && !d.range) click(p.x, p.y);
  invalidate();
});
canvas.addEventListener("pointerleave", () => { S.mouse = null; $("tooltip").hidden = true; invalidate(); });
canvas.addEventListener("dblclick", (e) => {
  const p = local(e);
  if (onGutterEdge(p.x)) { LABEL_W = M.labelW; localStorage.removeItem("nsa-label-w"); invalidate(); return; }
  const h = hitAt(p.x, p.y);
  if (h) zoomTo(h.off + h.start, h.off + h.end);
});

function click(x, y) {
  const ri = rowAt(y);
  if (ri >= 0) {
    const row = S.rows[ri];
    if (row.kind === "trace") { toggleFold(row.t); return; }
    if (row.kind === "toggle") { setFocus(row.t); row.t.showIdle = !row.t.showIdle; layoutRows(); renderSidebar(); invalidate(); return; }
    if (row.kind === "group") { toggleGroup(row.t, row.group); return; }
  }
  const h = hitAt(x, y);
  if (!h) { if (x > LABEL_W && y > RULER_H) { S.selected = null; clearRange(); renderPanel(); saveHash(); } return; }
  if (h.count > 1) { zoomTo(h.off + h.start, h.off + h.end); return; }
  select(h.t, h.row, h.lane, h.index);
}

// select selects an event of trace T; it is then the trace in focus.
async function select(T, row, lane, index, quiet = false) {
  const doc = DOC, gen = S.generation;
  try {
    const ev = await api("event", { path: T.path, row, lane, index });
    withDoc(doc, () => {
      if (gen !== S.generation) return;
      S.selected = { ...ev, ti: T.i };
      if (ev.linked) ev.linked.ti = T.i;
      if (!quiet) S.panelTab = 0;
      setFocus(T);
      renderPanel();
      saveHash();
      invalidate();
    });
  } catch (e) { if (!(e instanceof AuthError) && !quiet) toast(e.message); }
}

// selectedSpan is the selected event's time on the shared axis.
function selectedSpan() {
  const e = S.selected, off = offsetOf(S.traces[e.ti]);
  return [off + e.start, off + e.end];
}

// setFocus makes T the trace the sidebar and the summaries are about.
function setFocus(T) {
  if (focusTrace() === T) return;
  S.focus = T;
  renderSidebar();
  if (S.panelTab) renderPanel();
  invalidate();
}

// setOff leaves out the traces of a session that off says (by index), and
// shows the others, loading them if needed: their rows and their block in
// the rank overview go, the others stay where they are (still aligned
// with every trace). The last one shown stays. As the desktop app's.
function setOff(off) {
  if (off.every(Boolean)) return;
  S.traces.forEach((T, i) => { T.off = !!off[i]; });
  if (S.selected && S.traces[S.selected.ti].off) S.selected = null;
  if (S.focus && S.focus.off) S.focus = null;
  for (const T of shownTraces()) if (!T.folded) loadMeta(T);
  layoutRows(); renderSidebar(); renderPanel(); saveHash(); invalidate();
}
// setTraceShown shows trace T or leaves it out.
function setTraceShown(T, show) { setOff(S.traces.map((x) => x === T ? !show : x.off)); }
// soloTrace shows only T, or every trace if T is the only one shown
// (Alt/⌥-click, or "only", in the session's list).
function soloTrace(T) {
  const alone = !T.off && shownTraces().length === 1;
  setOff(S.traces.map((x) => alone ? false : x !== T));
}
const showAllTraces = () => setOff(S.traces.map(() => false));

// toggleFold folds or unfolds a trace of a session, loading it if needed.
function toggleFold(T) {
  T.folded = !T.folded;
  if (!T.folded) { setFocus(T); loadMeta(T); }
  layoutRows(); renderSidebar(); invalidate();
}

function toggleGroup(T, g) {
  if (T.collapsed.has(g)) T.collapsed.delete(g); else T.collapsed.add(g);
  setFocus(T);
  layoutRows(); renderSidebar(); invalidate();
}

// ---------------------------------------------------------------- held-key navigation
// While A/D (or ←/→) or W/S is held, the frame loop pans or zooms at a
// constant rate a second (navTargets and navAdvance in shared.js, the
// desktop app's model), not the keyboard's auto-repeat. Blur stops it.

const NAV_KEYS = { a: "l", arrowleft: "l", d: "r", arrowright: "r", w: "i", s: "o" };
S.nav = { held: new Map(), vp: 0, vz: 0 };
S.shift = false;
const navMoving = () => S.nav.held.size > 0 || S.nav.vp !== 0 || S.nav.vz !== 0;
function navStop() { S.nav.held.clear(); S.nav.vp = S.nav.vz = 0; }

// stepNav moves the view by dt seconds of held-key navigation, and says
// whether to go on; a step that starts the motion moves nothing, so that
// idle time does not count.
function stepNav(dt) {
  const n = S.nav;
  if (!S.meta || !navMoving()) return false;
  const d = new Set(n.held.values());
  const [tp, tz] = navTargets(d.has("l"), d.has("r"), d.has("i"), d.has("o"), S.shift, S.navSpeed);
  if (n.vp === 0 && n.vz === 0) {
    const r = navAdvance(0, 0, tp, tz, 1 / 60);
    n.vp = r[0]; n.vz = r[1];
    return true;
  }
  dt = Math.min(Math.max(dt, 0), 0.1);
  const r = navAdvance(n.vp, n.vz, tp, tz, dt);
  n.vp = r[0]; n.vz = r[1];
  const w = dataW(), anchor = S.mouse && S.mouse.x > LABEL_W ? S.mouse.x - LABEL_W : w / 2;
  let v = { ...S.view };
  if (r[3] !== 0) {
    const [lo, hi] = extent();
    v = zoomViewport(v, Math.exp(r[3]), anchor, lo, hi, w);
  }
  v.start += r[2] * w * v.scale;
  S.view = clampView(v);
  S.target = { ...S.view };
  S.animating = false;
  return navMoving();
}

canvas.addEventListener("keydown", (e) => {
  S.shift = e.shiftKey;
  const nk = NAV_KEYS[e.key.toLowerCase()];
  if (nk && S.meta && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault();
    if (!e.repeat) { S.nav.held.set(e.key.toLowerCase(), nk); invalidate(); }
  }
});
window.addEventListener("keyup", (e) => {
  S.shift = e.shiftKey;
  if (S.nav.held.delete(e.key.toLowerCase())) invalidate();
});
window.addEventListener("keydown", (e) => { if (e.key === "Shift") S.shift = true; });
canvas.addEventListener("blur", navStop);
window.addEventListener("blur", navStop);
document.addEventListener("visibilitychange", () => { if (document.hidden) navStop(); });

canvas.addEventListener("keydown", (e) => {
  if (!S.meta || e.ctrlKey || e.metaKey || e.altKey) return;
  const w = dataW();
  const anchor = S.mouse && S.mouse.x > LABEL_W ? S.mouse.x - LABEL_W : w / 2;
  switch (e.key.toLowerCase()) {
    case "=": case "+": zoomAt(1 / 1.6, anchor); break;
    case "-": zoomAt(1.6, anchor); break;
    case "arrowup": S.scrollY -= 3 * LANE_H; clampScroll(); invalidate(); break;
    case "arrowdown": S.scrollY += 3 * LANE_H; clampScroll(); invalidate(); break;
    case "pageup": S.scrollY -= size.h - RULER_H; clampScroll(); invalidate(); break;
    case "pagedown": S.scrollY += size.h - RULER_H; clampScroll(); invalidate(); break;
    case "f": if (S.selected) zoomTo(...selectedSpan()); else fit(); break;
    case "g": goToLinked(); break; // to the event the selection is linked to (kernel, launch)
    case "0": case "home": fit(); break;
    case "p": S.perf.show = !S.perf.show; invalidate(); break;
    case "o": openPicker(); break;
    default: return;
  }
  e.preventDefault();
});
$("zoom-in").addEventListener("click", () => zoomAt(1 / 1.6, dataW() / 2));
$("zoom-out").addEventListener("click", () => zoomAt(1.6, dataW() / 2));
$("fit").addEventListener("click", () => fit());
$("zoom-range").addEventListener("click", zoomToRange);

// rangeSpan is the measured range (Shift+drag) on the shared axis, or null.
function rangeSpan() {
  const r = S.range;
  return r && r.a !== r.b ? [Math.min(r.a, r.b), Math.max(r.a, r.b)] : null;
}
function zoomToRange() { const r = rangeSpan(); if (r) zoomTo(r[0], r[1]); }
// rangeChanged follows a new range: the summaries are of it.
function rangeChanged() {
  $("zoom-range").hidden = !rangeSpan();
  if (S.panelTab) renderPanel();
  invalidate();
}
function clearRange() {
  if (!S.range) return;
  S.range = null;
  rangeChanged();
}

// The splitter between the timeline and the panel.
$("splitter").addEventListener("pointerdown", (e) => {
  const sp = $("splitter"), panel = $("panel"), startY = e.clientY, startH = panel.getBoundingClientRect().height;
  sp.setPointerCapture(e.pointerId); sp.classList.add("dragging");
  const move = (ev) => { panel.style.height = Math.max(90, Math.min(window.innerHeight - 200, startH - (ev.clientY - startY))) + "px"; };
  const up = () => { sp.classList.remove("dragging"); sp.removeEventListener("pointermove", move); sp.removeEventListener("pointerup", up); };
  sp.addEventListener("pointermove", move); sp.addEventListener("pointerup", up);
});

// ---------------------------------------------------------------- sidebar

function kv(k, v, cls = "") { return `<div class="kv ${cls}"><span class="k">${esc(k)}</span><span class="v" title="${esc(v)}">${esc(v)}</span></div>`; }

function renderSidebar() {
  if (BACKGROUND) return; // a tab not shown: its state only
  const sb = $("sidebar"), F = focusTrace();
  let h = "";
  if (S.traces.length > 1) h += sessionCard();
  const m = F && F.meta;
  h += densityCard();
  if (!m) { sb.innerHTML = h + navSpeedCard(); bindSession(sb); bindNavSpeed(sb); bindDensity(sb); return; }
  const multi = S.traces.length > 1;
  h += `<div class="section-title">${multi ? "IN FOCUS" : "TRACE"}</div><div class="card">
    ${kv("File", F.name)}${F.host ? kv("Host", F.host) : ""}${kv("Size", fmtBytes(m.size))}${kv("Duration", fmtDur(m.duration))}
    ${kv("Events", fmtCount(m.events))}${kv("Rows", fmtCount(m.rows.length))}${kv("Parsed on server in", m.loadMs + " ms")}</div>`;
  h += `<div class="section-title">EVENTS</div><div class="card">`;
  for (const k of ["Kernel", "Memcpy", "Memset", "CUDA API", "Sync", "NVTX range", "NVTX mark", "CPU op", "Python"]) if (m.counts[k]) h += kv(k, fmtCount(m.counts[k]));
  h += "</div>";
  const ids = Object.keys(m.devices).map(Number).sort((a, b) => a - b);
  if (ids.length) {
    h += `<div class="section-title">GPUS</div><div class="card">`;
    for (const id of ids) if (!m.idle.includes(id)) h += kv("GPU " + id, m.devices[id]);
    if (m.idle.length) {
      h += `<div class="switch-row" id="idle-switch" role="switch" aria-checked="${F.showIdle}" tabindex="0"><span>Show ${m.idle.length} idle GPU${m.idle.length === 1 ? "" : "s"}</span><div class="switch ${F.showIdle ? "on" : ""}"></div></div>`;
      if (F.showIdle) for (const id of m.idle) h += kv("GPU " + id, (m.devices[id] || "") + " · idle", "idle");
    }
    h += "</div>";
  }
  if ((m.missing && m.missing.length) || (m.warnings && m.warnings.length)) {
    h += `<div class="section-title">NOTES</div><div class="card">`;
    for (const x of m.missing || []) h += `<div class="note">No ${esc(x)}</div>`;
    for (const x of m.warnings || []) h += `<div class="note warn">${esc(x)}</div>`;
    h += "</div>";
  }
  h += navSpeedCard();
  h += `<div class="section-title">ROWS</div><div class="rows">`;
  let group = null;
  m.rows.forEach((r, i) => {
    if (r.group !== group) {
      group = r.group;
      h += `<div class="row-group" data-group="${esc(group)}"><span class="chev">${F.collapsed.has(group) ? "▸" : "▾"}</span><span class="name">${esc(group)}</span></div>`;
    }
    if (F.collapsed.has(group)) return;
    const on = !F.hidden.has(i);
    h += `<label class="row-item ${on ? "" : "off"}"><input type="checkbox" data-row="${i}" ${on ? "checked" : ""}><span class="name" title="${esc(r.name)}">${esc(r.name)}</span><span class="count">${compact(r.count)}</span></label>`;
  });
  h += "</div>";
  sb.innerHTML = h;
  bindSession(sb);
  bindNavSpeed(sb);
  bindDensity(sb);
  const sw = $("idle-switch");
  if (sw) {
    const flip = () => { F.showIdle = !F.showIdle; layoutRows(); renderSidebar(); invalidate(); };
    sw.addEventListener("click", flip);
    sw.addEventListener("keydown", (e) => { if (e.key === " " || e.key === "Enter") { e.preventDefault(); flip(); } });
  }
  sb.querySelectorAll(".row-group").forEach((el) => el.addEventListener("click", () => toggleGroup(F, el.dataset.group)));
  sb.querySelectorAll("input[data-row]").forEach((el) => el.addEventListener("change", () => {
    const i = +el.dataset.row;
    if (el.checked) F.hidden.delete(i); else F.hidden.add(i);
    el.parentElement.classList.toggle("off", !el.checked);
    layoutRows(); invalidate();
  }));
}

// navSpeedCard chooses how fast held W/A/S/D move the view, kept in the
// browser.
function navSpeedCard() {
  return `<div class="section-title">NAVIGATION SPEED</div><div class="card">
    <span class="segmented wide" id="nav-speed" role="group" aria-label="Navigation speed">${THEME.navSpeeds.map((s) =>
      `<button data-speed="${s.times}" class="${s.times === S.navSpeed ? "on" : ""}" title="${s.times}× the default rates">${esc(s.name)}</button>`).join("")}</span>
    <div class="note">How fast held W/A/S/D move the view</div></div>`;
}
function bindNavSpeed(sb) {
  sb.querySelectorAll("#nav-speed button").forEach((el) => el.addEventListener("click", () => {
    S.navSpeed = navSpeedOf(el.dataset.speed);
    localStorage.setItem("nsa-nav-speed", String(S.navSpeed));
    sb.querySelectorAll("#nav-speed button").forEach((b) => b.classList.toggle("on", b === el));
  }));
}

// sessionCard lists the traces of a session, and how they are aligned.
function sessionCard() {
  const n = S.traces.length, F = focusTrace();
  let h = `<div class="section-title">SESSION</div><div class="card">
    ${kv("Traces", n + " · " + loadedTraces().length + " loaded")}
    <div class="align-row"><span>Align</span><span class="segmented" id="align-mode" role="group" aria-label="Alignment">
      <button data-align="collectives" class="${S.align === "collectives" ? "on" : ""}" title="Collective kernels (all-reduce, all-gather, ...) end together on every rank: lined up on them, to microseconds; by their clocks where too few match">Collectives</button>
      <button data-align="abs" class="${S.align === "abs" ? "on" : ""}" title="At the moment each event happened, by the captures' clocks (hosts' clocks differ by up to milliseconds)">Clocks</button>
      <button data-align="start" class="${S.align === "start" ? "on" : ""}" title="Every trace's first event at 0">Starts</button></span></div>`;
  if (S.align !== "start") h += alignSummary();
  const shown = shownTraces().length;
  h += `<div class="traces-head"><span>${shown} of ${n} shown</span>${shown < n ? '<a id="traces-all" href="#" title="Show every trace">Show all</a>' : ""}</div><div class="traces">`;
  for (const T of S.traces) {
    const st = T.error ? "error" : T.loading ? loadingText(T) : T.meta ? compact(T.meta.events) : "—";
    const sub = [T.host, T.rank >= 0 ? "rank " + T.rank : ""].filter(Boolean).join(" · ");
    const only = !T.off && shown === 1 ? "all" : "only";
    h += `<div class="trace-item ${T === F && !T.off ? "focus" : ""} ${T.error ? "bad" : ""} ${T.off ? "off" : ""}" data-i="${T.i}" title="${esc(T.error || T.path)}">
      <input type="checkbox" data-show="${T.i}" ${T.off ? "" : "checked"} ${!T.off && shown === 1 ? "disabled" : ""} title="Show or leave out · Alt/⌥-click: only this one" aria-label="Show ${esc(T.name)}">
      <span class="chev" data-fold="${T.i}">${T.folded ? "▸" : "▾"}</span><span class="name">${esc(T.name)}</span>
      <span class="count">${esc(st)}</span><a class="only" data-only="${T.i}" href="#" title="${only === "all" ? "Show every trace" : "Show only this one"}">${only}</a>
      <span class="sub">${esc(sub)}</span></div>`;
  }
  return h + "</div></div>";
}

function bindSession(sb) {
  sb.querySelectorAll("#align-mode button").forEach((el) => el.addEventListener("click", () => setAlign(el.dataset.align)));
  const all = sb.querySelector("#traces-all");
  if (all) all.addEventListener("click", (e) => { e.preventDefault(); showAllTraces(); });
  sb.querySelectorAll(".trace-item").forEach((el) => el.addEventListener("click", (e) => {
    const T = S.traces[+el.dataset.i];
    if (e.target.dataset.only !== undefined) { e.preventDefault(); soloTrace(T); return; }
    if (e.target.dataset.show !== undefined) {
      if (e.altKey) { e.preventDefault(); soloTrace(T); } else setTraceShown(T, e.target.checked);
      return;
    }
    if (T.off) { setTraceShown(T, true); revealTrace(T); return; } // a name left out: shows it
    if (e.target.dataset.fold !== undefined) { toggleFold(T); return; }
    if (T.folded) toggleFold(T); else setFocus(T);
    revealTrace(T);
  }));
}

// revealTrace scrolls to the heading of a trace.
function revealTrace(T) {
  const r = S.rows.find((r) => r.kind === "trace" && r.t === T);
  if (r) { S.scrollY = r.y; clampScroll(); invalidate(); }
}

// ---------------------------------------------------------------- panel

const TABS = [["Details", null], ["Top kernels", "kernel"], ["Top CUDA API", "api"], ["Top NVTX ranges", "nvtx"], ["Top CPU ops", "cpu"]];
// tabsShown are the panel's tabs for the trace in focus: Top CPU ops for a
// PyTorch profile's operators.
const tabsShown = () => TABS.filter(([, k]) => k !== "cpu" || (S.meta && S.meta.counts["CPU op"] > 0));

// operatorStack lists the operators and annotations an event ran under on
// its thread, the innermost first (as the desktop app's): for GPU work,
// those of the CUDA call that launched it; a click selects one.
function operatorStack(e) {
  const gpu = [KIND.kernel, KIND.memcpy, KIND.memset].includes(e.kindId);
  let h = `<div class="py-title ops">Operator stack${gpu ? " where it was launched" : ""} · innermost first</div><div class="py-stack">`;
  [...e.ops.keys()].reverse().forEach((i) => {
    h += `<div class="py-frame op" data-ops="1" data-i="${i}" tabindex="0" title="Select it"><span class="fn">${esc(e.ops[i].name)}</span></div>`;
  });
  return h + "</div>";
}

// pythonStack is the Python stack an event ran under, the innermost frame
// first, as a debugger's call stack lists them (as the desktop app's): each
// frame's function and where it is, the program's own frames stronger than
// the libraries'; a click selects the frame's call. For GPU work, the stack
// of the CUDA API call that launched it.
function pythonStack(e) {
  const gpu = [KIND.kernel, KIND.memcpy, KIND.memset].includes(e.kindId);
  let h = `<div class="py-title">Python stack${gpu && e.stack && e.stack.length ? " where it was launched" : ""} · innermost first</div>`;
  if (!e.stack || !e.stack.length) {
    let why = "No Python calls on this thread around it.";
    if (gpu && e.linked) why = `No Python stack: launched from ${esc(e.linked.track)}, which ran no Python then (autograd's thread runs the backward pass without one).`;
    else if (gpu) why = "No Python stack: the CUDA call that launched it is not in the trace.";
    return h + `<div class="py-none">${why}</div>`;
  }
  h += '<div class="py-stack">';
  [...e.stack.keys()].reverse().forEach((i) => {
    const f = e.stack[i];
    const m = /^(.*)\((\d+)\): (.*)$/.exec(f.name);
    const fn = m ? m[3] : f.name, where = m ? `${m[1]}:${m[2]}` : "";
    const lib = !where || /^torch\/|\/torch\/|site-packages|dist-packages|^runpy\.py|^</.test(where);
    h += `<div class="py-frame ${lib ? "lib" : "own"}" data-i="${i}" tabindex="0" title="Select this call (${esc(f.name)})">` +
      `<span class="fn">${esc(fn)}</span><span class="at">${esc(where)}</span></div>`;
  });
  return h + "</div>";
}

function renderPanel() {
  if (BACKGROUND) return;
  const tabs = $("tabs");
  if (S.panelTab >= tabsShown().length) S.panelTab = 0;
  tabs.innerHTML = tabsShown().map(([t], i) => `<div class="tab ${i === S.panelTab ? "active" : ""}" data-tab="${i}">${t}</div>`).join("") +
    '<span class="spacer"></span>' +
    (S.highlight !== null ? `<span class="highlight">Highlighting ${esc(S.highlight.slice(0, 60))}<button id="hl-clear">Clear</button></span>` : "");
  tabs.querySelectorAll(".tab").forEach((el) => el.addEventListener("click", () => { S.panelTab = +el.dataset.tab; renderPanel(); saveHash(); }));
  if ($("hl-clear")) $("hl-clear").addEventListener("click", () => { S.highlight = null; renderPanel(); invalidate(); });
  const body = $("panel-body");
  if (!S.meta) { body.innerHTML = ""; return; }
  if (S.panelTab === 0) renderDetails(body); else renderSummary(body, TABS[S.panelTab][1]);
}

function field(k, v) { return `<div class="field"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`; }

function renderDetails(body) {
  const e = S.selected;
  if (!e) { body.innerHTML = '<div class="empty">Click an event on the timeline to see its details. Double-click zooms to it; clicking merged events zooms into them.</div>'; return; }
  const T = S.traces[e.ti], col = colorOf(T, nameIndexOf(T, e.name), e.kindId);
  let h = `<div class="detail-head"><div class="bar"></div><div class="name">${esc(e.name)}</div>
    <span class="badge">${esc(e.kind)}</span><span class="spacer"></span>
    <span class="actions"><button id="zoom-event" title="F">Zoom to event</button><button id="hl-event">Highlight name</button>${e.linked ?
      `<button class="primary" id="goto-head" title="Select ${esc(e.linked.name.slice(0, 60))} and show it with this one (G)">${goToLabel(e)}</button>` : ""}</span></div>`;
  // With a Python or an operator stack, they go beside the rest.
  const cols = e.python || (e.ops && e.ops.length);
  if (cols) h += '<div class="detail-cols"><div class="detail-main">';
  h += '<div class="fields">';
  if (S.traces.length > 1) h += field("Trace", T.name + (T.host ? " · " + T.host : ""));
  h += field("Row", e.group + " › " + e.track) + field("Duration", fmtDur(e.end - e.start)) + field("Start", fmtTime(offsetOf(T) + e.start, 0.1)) + field("End", fmtTime(offsetOf(T) + e.end, 0.1));
  if (e.kernel) {
    const k = e.kernel;
    h += field("Grid", k.grid.join(" × ")) + field("Block", k.block.join(" × ")) + field("Registers / thread", k.registers) +
      field("Shared memory", fmtBytes(k.staticShared) + " static · " + fmtBytes(k.dynamicShared) + " dynamic");
  }
  if (e.memop) {
    h += field("Size", fmtBytes(e.memop.bytes));
    if (e.end > e.start) h += field("Throughput", trim(e.memop.bytes / (e.end - e.start), 2) + " GB/s");
    if (e.kindId === KIND.memset) h += field("Value", e.memop.value);
  }
  if (e.corr) h += field("Correlation ID", e.corr);
  if (["nvtx", "cpu", "python"].includes(T.meta.rows[e.row].kind) && T.meta.rows[e.row].lanes > 1) h += field("Depth", e.lane);
  h += "</div>";
  if (e.kernel && e.kernel.fullName && e.kernel.fullName !== e.name) h += `<div class="demangled">${esc(e.kernel.fullName)}</div>`;
  if (e.linked) {
    const o = e.linked, launched = e.kindId === KIND.api;
    const lat = launched ? o.start - e.start : e.start - o.start;
    h += `<div class="linked"><span class="k">${launched ? "Launched" : "Launched by"}</span><span class="n">${esc(o.name)}</span>
      <span class="m">on ${esc(o.group + " › " + o.track)} · launch to start ${fmtDur(lat)}</span><button id="goto-linked">Go to</button></div>`;
  }
  if (cols) h += `</div><div class="detail-side">${e.python ? pythonStack(e) : ""}${e.ops && e.ops.length ? operatorStack(e) : ""}</div></div>`;
  body.innerHTML = h;
  body.querySelector(".bar").style.background = col.solid;
  body.querySelectorAll(".py-frame").forEach((el) => {
    const go = () => { const f = (el.dataset.ops ? e.ops : e.stack)[+el.dataset.i]; revealRow(T, f.row); select(T, f.row, f.lane, f.index); };
    el.addEventListener("click", go);
    el.addEventListener("keydown", (ev) => { if (ev.key === "Enter") go(); });
  });
  $("zoom-event").addEventListener("click", () => zoomTo(...selectedSpan()));
  $("hl-event").addEventListener("click", () => { S.highlight = e.name; renderPanel(); invalidate(); });
  if (e.linked) {
    $("goto-linked").addEventListener("click", goToLinked);
    $("goto-head").addEventListener("click", goToLinked);
  }
}
// goToLabel is what the button that goes to the linked event says: to a
// kernel's launch, or to the GPU work an API call launched (as the
// desktop app's).
function goToLabel(e) {
  if (e.kindId !== KIND.api) return "Go to launch";
  return e.linked.kindId === KIND.memcpy ? "Go to copy" : e.linked.kindId === KIND.memset ? "Go to memset" : "Go to kernel";
}
// goToLinked selects the event the selection's correlation id links it to
// and shows it with the one it came from where both fit (zoomed to both,
// their rows scrolled into view), else the linked one alone; in the rank
// overview the summary lane of its kind turns on (as the desktop app's).
function goToLinked() {
  const e = S.selected;
  if (!e || !e.linked) return;
  const T = S.traces[e.ti], o = e.linked, off = offsetOf(T);
  if (T.off) setTraceShown(T, true);
  if (compactOn()) {
    const kind = T.meta.rows[o.row].kind, lane = THEME.compactLanes.find((l) => l.kind === kind);
    if (lane && !lanesOn().includes(lane.key)) setLane(lane.key, true);
  }
  revealRow(T, o.row, e.row);
  let a = o.start, b = o.end;
  const ua = Math.min(a, e.start), ub = Math.max(b, e.end);
  if ((b - a) * 1000 >= ub - ua) { a = ua; b = ub; } // both, unless the linked one would be too thin to see
  zoomTo(off + a, off + b);
  select(T, o.row, o.lane, o.index);
}
function nameIndexOf(T, name) { for (const [k, v] of T.names) if (v === name) return k; return -1; }

// revealRow scrolls so that row i of T shows (shown again if left out,
// folded, hidden or collapsed), with row other of T too where both fit;
// in the rank overview, the summary lane it draws into.
function revealRow(T, i, other = -1) {
  const g = T.meta.rows[i].group;
  if (T.off) setTraceShown(T, true);
  if (T.folded || T.hidden.has(i) || T.collapsed.has(g)) {
    T.folded = false; T.hidden.delete(i); T.collapsed.delete(g); layoutRows(); renderSidebar();
  }
  const at = (j) => S.rows.find((r) => r.t === T && (r.kind === "track" && r.track === j || r.kind === "summary" && r.members.includes(j)));
  const r = at(i), view = size.h - RULER_H;
  if (!r) return;
  const o = other >= 0 ? at(other) : null;
  if (o) {
    const lo = Math.min(r.y, o.y), hi = Math.max(r.y + r.h, o.y + o.h);
    if (hi - lo <= view) {
      if (lo < S.scrollY || hi > S.scrollY + view) { S.scrollY = lo - (view - (hi - lo)) / 2; clampScroll(); }
      return;
    }
  }
  if (r.y < S.scrollY || r.y + r.h > S.scrollY + view) { S.scrollY = r.y - view / 3; clampScroll(); }
}

async function renderSummary(body, kind) {
  // Over the measured range, if there is one (each event by its overlap
  // with it: see /api/summary), in the trace's own times; else the trace.
  const F = focusTrace(), r = rangeSpan(), off = offsetOf(F);
  const q = { path: F.path, kind, n: 100 };
  if (r) { q.from = Math.floor(r[0] - off); q.to = Math.ceil(r[1] - off); }
  const key = r ? `${kind}:${q.from}:${q.to}` : kind;
  let stats = F.summaries[key];
  if (!stats) {
    body.innerHTML = '<div class="empty">Loading…</div>';
    try { stats = await api("summary", q); }
    catch (e) { if (!(e instanceof AuthError)) body.innerHTML = `<div class="empty">${esc(e.message)}</div>`; return; }
    if (Object.keys(F.summaries).length > 40) F.summaries = {};
    F.summaries[key] = stats;
    const r2 = rangeSpan();
    if (TABS[S.panelTab][1] !== kind || focusTrace() !== F || String(r) !== String(r2)) return;
  }
  const scope = r ? `In range <b>${fmtTime(r[0], (r[1] - r[0]) / 1000)}–${fmtTime(r[1], (r[1] - r[0]) / 1000)}</b> (${fmtDur(r[1] - r[0])}) · events clipped to it`
    : "Whole trace · Shift+drag on the timeline to sum a range";
  const where = S.traces.length > 1 ? ` · in ${esc(F.name)}${F.host ? " · " + esc(F.host) : ""}, the trace in focus` : "";
  const of = `<div class="sum-head">${scope}${where}</div>`;
  if (!stats.length) { body.innerHTML = of + `<div class="empty">No events of this kind in the ${r ? "range" : "trace"}.</div>`; return; }
  const what = { kernel: "GPU time", api: "CPU time in CUDA API", nvtx: "time in ranges", cpu: "CPU time in operators" }[kind];
  let h = `<table class="summary"><colgroup><col><col class="c-count"><col class="c-total"><col class="c-num"><col class="c-num"><col class="c-num"><col class="c-share"></colgroup>
    <tr><th>Name</th><th>Count</th><th>Total</th><th>Avg</th><th>Min</th><th>Max</th><th>Share of ${what}</th></tr>`;
  stats.forEach((s, i) => {
    h += `<tr class="stat ${S.highlight === s.name ? "active" : ""}" data-i="${i}"><td title="${esc(s.name)}">${esc(s.name)}</td><td>${fmtCount(s.count)}</td>
      <td>${fmtDur(s.total)}</td><td>${fmtDur(s.avg)}</td><td>${fmtDur(s.min)}</td><td>${fmtDur(s.max)}</td>
      <td><div class="share"><div class="track"><div class="fill" data-share="${Math.max(100 * s.share, 0.5)}"></div></div><span>${trim(100 * s.share, 1)}%</span></div></td></tr>`;
  });
  body.innerHTML = of + h + "</table>";
  body.querySelectorAll(".fill").forEach((el) => (el.style.width = el.dataset.share + "%"));
  body.querySelectorAll("tr.stat").forEach((el) => el.addEventListener("click", () => {
    const n = stats[+el.dataset.i].name;
    S.highlight = S.highlight === n ? null : n;
    renderPanel(); invalidate();
  }));
}

// ---------------------------------------------------------------- appearance
// The system's appearance (prefers-color-scheme, followed live), or the
// user's choice, kept in localStorage.

const darkQuery = matchMedia("(prefers-color-scheme: dark)");
S.themeMode = ["light", "dark"].includes(localStorage.getItem("nsa-theme")) ? localStorage.getItem("nsa-theme") : "system";

// CSS variables of style.css, from the palette.
const CSS_VARS = {
  "--bg": "bg", "--panel": "panel", "--panel-alt": "panelAlt", "--border": "border", "--text": "text",
  "--muted": "muted", "--faint": "faint", "--accent": "accent", "--accent-hi": "accentHi",
  "--on-accent": "accentText", "--hover": "hover", "--pressed": "pressed", "--tooltip": "tooltip",
  "--warn": "warn", "--shadow": "shadow", "--scrollbar": "scrollbar",
};

function applyTheme() {
  const dark = S.themeMode === "dark" || (S.themeMode === "system" && darkQuery.matches);
  PAL = THEME.palettes[dark ? "dark" : "light"];
  C = PAL.colors;
  BG_RGB = hexRGB(C.bg);
  clearColors();
  const root = document.documentElement.style;
  for (const [v, k] of Object.entries(CSS_VARS)) root.setProperty(v, C[k]);
  root.setProperty("--overlay", dark ? "rgba(5,7,10,.62)" : "rgba(26,31,43,.28)");
  root.setProperty("color-scheme", dark ? "dark" : "light");
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  document.querySelectorAll("#theme-mode button").forEach((b) => b.classList.toggle("on", b.dataset.mode === S.themeMode));
  invalidate();
}
darkQuery.addEventListener("change", () => { if (S.themeMode === "system") applyTheme(); });
document.querySelectorAll("#theme-mode button").forEach((b) => b.addEventListener("click", () => {
  S.themeMode = b.dataset.mode;
  if (S.themeMode === "system") localStorage.removeItem("nsa-theme"); else localStorage.setItem("nsa-theme", S.themeMode);
  applyTheme();
}));
applyTheme();

// ---------------------------------------------------------------- tabs
// Each tab keeps a timeline open: a trace, or a session of several, with
// its view, selection, range and rows, switched between without loading
// again (the traces keep their tiles; a tab not shown fetches nothing). The
// tab shown has its state in S; the others keep theirs (DOC_KEYS) in their
// doc. What a load brings back lands in the tab it was asked for in
// (withDoc), shown or not. The tabs and their views are kept for the
// browser tab (sessionStorage): a reload brings them back, each loading
// when first shown. As the desktop app's; tabTitle (shared.js) names them.

const DOC_KEYS = ["inputs", "traces", "focus", "align", "rows", "contentH", "view", "target", "anchorX", "animating", "scrollY",
  "generation", "range", "rangeClose", "selected", "highlight", "panelTab", "ranks", "savedStart", "savedScale", "screen", "pathErr", "pathText", "listing"];
let GEN = 0, DOC_IDS = 0;
const DOCS = [];
function freshState() {
  return {
    inputs: [], traces: [], focus: null, align: "collectives", rows: [], contentH: 0,
    view: { start: 0, scale: 1 }, target: { start: 0, scale: 1 }, anchorX: 0, animating: false, scrollY: 0,
    generation: 0, range: null, rangeClose: null, selected: null, highlight: null, panelTab: 0, ranks: [],
    savedStart: undefined, savedScale: undefined, screen: { kind: "empty" }, pathErr: "", pathText: "",
    listing: null, // the paths the server is listing, while it does (loadItems)
  };
}
function makeDoc(lazy = null) {
  const d = { id: ++DOC_IDS, state: freshState(), lazy };
  if (lazy) d.state.inputs = lazy.inputs;
  return d;
}
function saveDoc(d) { for (const k of DOC_KEYS) d.state[k] = S[k]; }
function loadDoc(d) { for (const k of DOC_KEYS) S[k] = d.state[k]; DOC = d; }
DOC = makeDoc();
DOCS.push(DOC);
loadDoc(DOC);
for (const [v, k] of [["--tabs-h", "tabBarH"], ["--tab-max-w", "tabMaxW"], ["--tab-min-w", "tabMinW"]]) document.documentElement.style.setProperty(v, M[k] + "px");

// withDoc runs fn with tab d's state in S (nothing drawn from it unless d
// is shown), and returns what fn does; nothing if d was closed.
function withDoc(d, fn) {
  if (d === DOC && !BACKGROUND) return fn();
  if (!DOCS.includes(d)) return undefined;
  const shown = DOC, was = BACKGROUND;
  saveDoc(shown); loadDoc(d); BACKGROUND = true;
  try { return fn(); } finally { saveDoc(d); loadDoc(shown); BACKGROUND = was; renderDocTabs(); }
}

// clearTraces lets go of traces no tab shows any more: their tiles.
function clearTraces(ts) { for (const T of ts) { T.tiles.clear(); T.dead = true; } }

// openTab opens an empty tab after the one shown and shows it; false if
// there are too many.
function openTab() {
  if (DOCS.length >= M.maxTabs) { toast(`At most ${M.maxTabs} tabs: close one first.`); return false; }
  const d = makeDoc();
  DOCS.splice(DOCS.indexOf(DOC) + 1, 0, d);
  showTab(d);
  return true;
}
// newTab opens an empty tab, to type a path in (⌘/Ctrl+Enter there, or
// ⌘/Ctrl+click in the recent paths or the browser, opens one in a new tab).
function newTab() { if (openTab()) focusPath(); }

// showTab shows tab d: its timeline, sidebar, panel, path and address.
function showTab(d) {
  if (!DOCS.includes(d)) return;
  if (d !== DOC) {
    navStop();
    S.pathText = $("path-input").value; // what was being typed stays with its tab
    forgetTiles(true);
    saveDoc(DOC);
    loadDoc(d);
    S.mouse = null; S.dragging = null;
    $("tooltip").hidden = true;
  }
  if (S.view.scale && S.meta) { S.view = clampView(S.view); S.target = clampView(S.target); clampScroll(); }
  syncChrome();
  renderScreen();
  renderSidebar();
  renderPanel();
  renderDocTabs();
  saveHash();
  saveTabs();
  invalidate();
  if (d.lazy) { const l = d.lazy; d.lazy = null; openPaths(l.inputs, { link: new URLSearchParams(l.hash || ""), all: true }); }
  else if (S.meta) canvas.focus();
  else if (!S.inputs.length) focusPath();
}

// closeTab closes tab d; the last one closed leaves an empty tab.
function closeTab(d) {
  const i = DOCS.indexOf(d);
  if (i < 0) return;
  clearTraces(d === DOC ? S.traces : d.state.traces);
  if (DOCS.length === 1) { const e = makeDoc(); DOCS[0] = e; enterTab(e); return; }
  DOCS.splice(i, 1);
  if (d === DOC) enterTab(DOCS[Math.min(i, DOCS.length - 1)]);
  else { renderDocTabs(); saveTabs(); }
}
// enterTab shows d when the tab shown was closed: its state is not kept.
function enterTab(d) {
  navStop();
  loadDoc(d);
  S.mouse = null; S.dragging = null;
  $("tooltip").hidden = true;
  showTab(d);
}

// cycleTab shows the tab by steps after (or before) the one shown.
function cycleTab(by) { showTab(DOCS[(DOCS.indexOf(DOC) + by + DOCS.length) % DOCS.length]); }

// syncChrome sets what the page around the timeline says of the tab shown:
// its path, its title, its error, Zoom to range.
function syncChrome() {
  if (BACKGROUND) return;
  $("path-input").value = S.pathText || S.inputs.join(" ");
  const e = $("path-error");
  e.textContent = S.pathErr || ""; e.hidden = !S.pathErr;
  $("path-input").classList.toggle("bad", !!S.pathErr);
  $("zoom-range").hidden = !rangeSpan();
  const n = S.traces.length;
  document.title = DOC.name ? "Tracelight — " + DOC.name : !S.inputs.length ? "Tracelight" : "Tracelight — " + (n === 1 ? S.traces[0].name : n ? `${n} traces` : tabTitle(S.inputs));
}

// renderDocTabs draws the tab bar: a tab a timeline (its name, a spinner
// while it loads, ×), and + for a new one.
function renderDocTabs() {
  if (BACKGROUND) return;
  const bar = $("doc-tabs"), mod = modKey();
  bar.innerHTML = DOCS.map((d, i) => {
    const st = d === DOC ? S : d.state, title = d.name || tabTitle(st.inputs);
    const busy = st.screen && st.screen.kind === "loading", n = i < 8 ? `Alt+${i + 1}` : i === DOCS.length - 1 ? "Alt+9" : "";
    const tip = (st.inputs.length ? st.inputs.join("\n") : "An empty tab: type a path above") + (n ? `\n(${n})` : "") + "\nDouble-click to rename";
    return `<div class="doc-tab ${d === DOC ? "active" : ""}" role="tab" aria-selected="${d === DOC}" data-id="${d.id}" title="${esc(tip)}">` +
      (busy ? '<span class="doc-spin"></span>' : "") + `<span class="doc-name">${esc(title)}</span>` +
      `<button class="doc-close" data-close="${d.id}" title="Close tab (Alt+W)" aria-label="Close tab">×</button></div>`;
  }).join("") + `<button class="doc-new" id="doc-new" title="New tab (Alt+T) · ${mod}+Enter in the path box opens there in a new tab" aria-label="New tab">+</button>`;
  const byId = (id) => DOCS.find((d) => d.id === +id);
  bar.querySelectorAll(".doc-tab").forEach((el) => {
    el.addEventListener("mousedown", (e) => { if (e.button === 1) e.preventDefault(); });
    el.addEventListener("auxclick", (e) => { if (e.button === 1) closeTab(byId(el.dataset.id)); });
    el.addEventListener("pointerdown", (e) => { if (e.button === 0 && !e.target.dataset.close && e.target.tagName !== "INPUT") tabDragStart(e, el); });

    el.addEventListener("click", (e) => {
      if (tabDragged) { tabDragged = false; return; } // the end of a drag, no click
      if (e.target.tagName === "INPUT") return; // its name being edited
      if (e.target.dataset.close) { closeTab(byId(e.target.dataset.close)); return; }
      // A second click on the tab soon after the first renames it (the
      // first one's showTab redrew the bar, so no dblclick comes).
      const id = +el.dataset.id, now = performance.now(), again = tabClick.id === id && now - tabClick.t < 400;
      tabClick = again ? { id: 0, t: 0 } : { id, t: now };
      const d = byId(id);
      if (d !== DOC) showTab(d);
      if (again) renameTab(d, $("doc-tabs").querySelector(`.doc-tab[data-id="${id}"]`));
    });
  });
  $("doc-new").addEventListener("click", newTab);
  bar.querySelector(".doc-tab.active")?.scrollIntoView({ block: "nearest", inline: "nearest" });
}

// Dragging a tab along the bar moves it there, the others making room as
// it passes them, and shows it (as the desktop app's). A press that moves
// less than a few pixels is a click.
let tabDrag = null, tabDragged = false, tabClick = { id: 0, t: 0 };
function tabDragStart(e, el) {
  const els = [...$("doc-tabs").querySelectorAll(".doc-tab")];
  const rects = els.map((x) => x.getBoundingClientRect());
  tabDrag = { el, els, from: els.indexOf(el), x0: e.clientX, lefts: rects.map((r) => r.left), widths: rects.map((r) => r.width), to: -1, id: e.pointerId };
  el.setPointerCapture(e.pointerId);
}
function tabDragMove(e) {
  const d = tabDrag;
  if (!d || e.pointerId !== d.id) return;
  const dx = e.clientX - d.x0;
  if (d.to < 0 && Math.abs(dx) < 5) return;
  if (d.to < 0) { d.el.classList.add("dragging"); $("doc-tabs").classList.add("dragging"); }
  const lo = d.lefts[0] - d.lefts[d.from], hi = d.lefts[d.lefts.length - 1] - d.lefts[d.from];
  const x = Math.min(Math.max(dx, lo - 20), hi + 20); // along the bar, a little past its ends
  d.el.style.transform = `translateX(${x}px)`;
  d.to = tabDropIndex(d.lefts, d.widths, d.from, d.lefts[d.from] + d.widths[d.from] / 2 + x);
  const room = d.widths[d.from] + 2;
  d.els.forEach((o, j) => {
    if (j === d.from) return;
    const shift = d.from < j && j <= d.to ? -room : d.to <= j && j < d.from ? room : 0;
    o.style.transform = shift ? `translateX(${shift}px)` : "";
  });
}
function tabDragEnd(e) {
  const d = tabDrag;
  if (!d || e.pointerId !== d.id) return;
  tabDrag = null;
  $("doc-tabs").classList.remove("dragging");
  if (d.to < 0) return; // a click
  tabDragged = true;
  setTimeout(() => { tabDragged = false; }, 0); // no click came (released off the tab)
  moveTab(DOCS.find((x) => x.id === +d.el.dataset.id), d.to);
}
document.addEventListener("pointermove", tabDragMove);
document.addEventListener("pointerup", tabDragEnd);
document.addEventListener("pointercancel", tabDragEnd);

// renameTab edits tab d's name in its place in the tab bar (el): Enter or
// leaving the field keeps the name typed, Esc the one it had; an empty
// name gives the tab its paths' again. Kept with the tabs (saveTabs).
const MAX_TAB_NAME = 80;
function renameTab(d, el) {
  if (!d || el.querySelector("input")) return;
  const span = el.querySelector(".doc-name");
  const input = document.createElement("input");
  input.className = "doc-rename";
  input.value = d.name || "";
  input.placeholder = "Tab name";
  input.maxLength = MAX_TAB_NAME;
  input.setAttribute("aria-label", "Tab name");
  span.replaceWith(input);
  input.focus();
  input.select();
  let done = false;
  const finish = (keep) => {
    if (done) return;
    done = true;
    if (keep) setTabName(d, input.value);
    renderDocTabs();
  };
  input.addEventListener("keydown", (e) => {
    e.stopPropagation(); // the timeline's keys and the tabs' stay out of it
    if (e.key === "Enter") { e.preventDefault(); finish(true); }
    if (e.key === "Escape") { e.preventDefault(); finish(false); }
  });
  input.addEventListener("blur", () => finish(true));
}
function setTabName(d, name) {
  d.name = name.trim().replace(/\s+/g, " ").slice(0, MAX_TAB_NAME);
  if (d === DOC) syncChrome();
  saveTabs();
}

// moveTab moves tab d to the place to (0 the first), and shows it.
function moveTab(d, to) {
  const i = DOCS.indexOf(d);
  if (i < 0) return;
  to = Math.min(Math.max(to, 0), DOCS.length - 1);
  DOCS.splice(i, 1);
  DOCS.splice(to, 0, d);
  if (d !== DOC) showTab(d);
  else { renderDocTabs(); saveTabs(); }
}

// tabShortcut handles the tabs' keys, wherever the focus is: Alt+T new,
// Alt+W close, Alt+1…8 the nth, Alt+9 the last, Alt+] and Alt+[ the next and
// the one before, Alt+Shift+Page Up and Page Down move the tab shown (the
// browser keeps ⌘/Ctrl+T, W, Tab, digits and Ctrl+Shift+Page Up for its
// own tabs). Codes, not keys: Option+T types † on a Mac.
function tabShortcut(e) {
  if (!e.altKey || e.metaKey || e.ctrlKey) return false;
  const c = e.code;
  let act = null;
  if (c === "KeyT" && !e.shiftKey) act = newTab;
  else if (c === "KeyW" && !e.shiftKey) act = () => closeTab(DOC);
  else if (c === "PageUp" && e.shiftKey) act = () => moveTab(DOC, DOCS.indexOf(DOC) - 1);
  else if (c === "PageDown" && e.shiftKey) act = () => moveTab(DOC, DOCS.indexOf(DOC) + 1);
  else if (c === "BracketRight") act = () => cycleTab(1);
  else if (c === "BracketLeft") act = () => cycleTab(-1);
  else if (/^Digit[1-9]$/.test(c)) { const k = +c.slice(5); act = () => showTab(k === 9 ? DOCS[DOCS.length - 1] : DOCS[k - 1]); }
  if (!act) return false;
  e.preventDefault();
  act();
  return true;
}

// saveTabs keeps the tabs (their paths and views) for the browser tab.
function saveTabs() {
  const cur = DOCS.indexOf(DOC);
  const tabs = DOCS.map((d) => {
    const st = d === DOC ? S : d.state;
    if (d.lazy) return { inputs: d.lazy.inputs, hash: d.lazy.hash || "", name: d.name || "" };
    const h = new URLSearchParams();
    if (st.traces.some((t) => t.meta)) h.set("v", Math.round(st.target.start) + "," + Math.round(st.target.start + st.target.scale * dataW()));
    if (st.align !== "collectives") h.set("align", st.align);
    if (st.panelTab) h.set("tab", st.panelTab);
    return { inputs: st.inputs, hash: h.toString(), name: d.name || "" };
  });
  try { sessionStorage.setItem("nsa-tabs", JSON.stringify({ tabs, cur })); } catch { /* full or off */ }
}
// restoreTabs opens the address's paths in a tab, and brings back the
// other tabs kept for the browser tab (each loads when first shown), if
// the address is the one they were kept with.
function restoreTabs(paths) {
  let kept = null;
  try { kept = JSON.parse(sessionStorage.getItem("nsa-tabs")); } catch { kept = null; }
  const ok = kept && Array.isArray(kept.tabs) && kept.tabs.length > 1 && kept.tabs[kept.cur] &&
    JSON.stringify(kept.tabs[kept.cur].inputs) === JSON.stringify(paths);
  if (ok) {
    kept.tabs.forEach((t, i) => {
      if (i === kept.cur || !Array.isArray(t.inputs)) return;
      const d = t.inputs.length ? makeDoc({ inputs: t.inputs, hash: t.hash }) : makeDoc();
      d.name = typeof t.name === "string" ? t.name : "";
      DOCS.splice(i < kept.cur ? DOCS.indexOf(DOC) : DOCS.length, 0, d);
    });
    DOC.name = typeof kept.tabs[kept.cur].name === "string" ? kept.tabs[kept.cur].name : "";
  }
  renderDocTabs();
  if (paths.length) openPaths(paths, { link: new URLSearchParams(location.hash.slice(1)) });
  else showScreen({ kind: "empty" });
}

// ---------------------------------------------------------------- start

async function start() {
  try {
    S.info = await api("info");
  } catch (e) {
    if (!(e instanceof AuthError)) toast("Server: " + e.message);
    return;
  }
  renderPanel();
  if (S.info.app) $("app-version").textContent = S.info.app;
  $("path-input").placeholder = `Path of a trace, a folder of traces or a glob under ${S.info.root}  (⌘L or /)`;
  // ?path=/abs/path, repeated for several (or the older #path=), opens them
  // directly.
  let paths = new URLSearchParams(location.search).getAll("path").filter(Boolean);
  if (!paths.length) paths = new URLSearchParams(location.hash.slice(1)).getAll("path").filter(Boolean);
  restoreTabs(paths);
}

// ---------------------------------------------------------------- the path input
// The main way to open a trace: its absolute path on the server (or one
// relative to the root), with the recent ones offered as it is typed. The
// browse listing (openPicker) is the second way.

const RECENT_KEY = "nsa-recent-paths";
function recents() {
  try { const r = JSON.parse(localStorage.getItem(RECENT_KEY)); return Array.isArray(r) ? r.filter((x) => typeof x === "string") : []; }
  catch { return []; }
}
function addRecent(p) {
  localStorage.setItem(RECENT_KEY, JSON.stringify([p, ...recents().filter((x) => x !== p)].slice(0, 12)));
  renderRecents();
}
function renderRecents() {
  $("recent-paths").innerHTML = recents().map((p) => `<option value="${esc(p)}"></option>`).join("");
}
function focusPath() { if (BACKGROUND) return; const i = $("path-input"); i.focus(); i.select(); }
function pathError(msg) {
  S.pathErr = msg;
  if (BACKGROUND) return;
  const e = $("path-error");
  e.textContent = msg;
  e.hidden = !msg;
  $("path-input").classList.toggle("bad", !!msg);
}

// showScreen sets what covers the timeline of the tab shown: nothing
// (null), the empty state ({kind: "empty"}: the recent paths, and browsing
// as the second way) or a loading message ({kind: "loading", text}).
// ---------------------------------------------------------------- loading
// Loading several traces (the ranks of a job) takes a while: the loading
// screen says how far each has got and how far they all are, as the
// server reports its loads (/api/progress, asked every 250 ms), as the
// desktop app's (internal/viewer/loading.go).

// loadItems are the loading screen's lines: a path while the server lists
// them, then a trace (of those that load at once, not the folded ones).
function loadItems() {
  if (S.listing) {
    return S.listing.map((x) => ({ name: x.name, size: 0, listing: true, error: "",
      phase: x.progress ? x.progress.phase : "listing", fraction: x.progress ? x.progress.fraction : -1 }));
  }
  return S.traces.filter((T) => !T.folded).map((T) => ({ name: T.name, size: T.size, error: T.error,
    phase: T.meta ? "done" : T.error ? "failed" : T.progress ? T.progress.phase : "queued",
    fraction: T.meta ? 1 : T.error ? 0 : T.progress ? T.progress.fraction : 0 }));
}
function phaseText(it) {
  switch (it.phase) {
    case "listing": return "listing…";
    case "reading": return `reading ${Math.floor(it.fraction * 100)}%`;
    case "failed": return it.error ? "failed: " + it.error : "failed";
    case "converting": return "converting (nsys)";
  }
  return it.phase;
}
function loadingText(T) { return T.progress ? phaseText({ phase: T.progress.phase, fraction: T.progress.fraction }) : "loading…"; }
const bar = (f, cls = "") => `<span class="bar ${cls}">${f > 0 ? `<i style="width:${(Math.min(f, 1) * 100).toFixed(1)}%"></i>` : ""}</span>`;
function loadingHTML(text) {
  const items = loadItems();
  if (!items.length) return `<div class="spinner"></div><div>${esc(text)}</div>`;
  const known = items.reduce((n, it) => n + it.size, 0);
  let got = 0, all = 0, done = 0, failed = 0;
  for (const it of items) {
    const w = known > 0 ? it.size / known : 1;
    all += w; got += w * Math.max(it.fraction, 0);
    if (it.phase === "done") done++;
    if (it.phase === "failed") failed++;
  }
  const f = all ? got / all : 0;
  const what = S.listing ? text : items.length === 1 ? `Loading ${items[0].name}…` : `Loading ${items.length} traces…`;
  let status = `${Math.floor(f * 100)}%`;
  if (items.length > 1) status = `${done} of ${items.length} done · ${status}`;
  if (failed) status += ` · ${failed} failed`;
  let h = `<div class="spinner"></div><div class="load-title">${esc(what)} <span class="faint">${esc(status)}</span></div>${bar(f, "big")}`;
  if (items.length === 1 && !items[0].listing) return h + `<div class="faint">${esc(phaseText(items[0]))}</div>`;
  h += '<div class="load-list">';
  items.slice(0, 16).forEach((it) => {
    h += `<div class="load-row"><span class="n" title="${esc(it.name)}">${esc(it.name)}</span>${bar(it.fraction, it.phase === "failed" ? "bad" : "")}<span class="st">${esc(phaseText(it))}</span></div>`;
  });
  if (items.length > 16) h += `<div class="faint">and ${items.length - 16} more</div>`;
  return h + "</div>";
}

// ---------------------------------------------------------------- choosing
// Too many traces at once (as the desktop app's, internal/viewer/
// chooser.go): a folder of 64 ranks, a glob, a pattern or a merged file
// that would load more than THEME's chooseOver traces, or chooseBytes of
// files, first asks which to open: a box each, choosePick ticked, quick
// picks, and opens those ticked side by side, or all.

// uniqueBytes is the size of the traces' files, each file once (the ranks
// of a merged file are one file).
function uniqueBytes(traces) {
  const seen = new Set();
  let n = 0;
  for (const t of traces) {
    const file = t.path.replace(/#rank\d+$/, "");
    if (!seen.has(file)) { seen.add(file); n += t.size || 0; }
  }
  return n;
}
function renderChooser(el, sc) {
  const n = sc.traces.length, k = M.choosePick;
  const name = sc.inputs.length === 1 ? sc.inputs[0].replace(/\/+$/, "").split("/").pop() : sc.inputs.length + " paths";
  el.innerHTML = `<div class="chooser" role="dialog" aria-label="Choose the traces to open">
    <div class="choose-title">${n} traces in ${esc(name)} <span class="faint">${esc(fmtBytes(uniqueBytes(sc.traces)))}</span></div>
    <div class="choose-hint">That's a lot to open at once (more than ${M.chooseOver} traces, or ${esc(fmtBytes(M.chooseBytes))}): tick the ones to open, or open them all.</div>
    <div class="choose-bar"><button data-pick="first" title="The first traces, in order">First ${k}</button><button data-pick="spread" title="Traces spread across the job: the first, the last and between">Spread ${k}</button>
      <button data-pick="all">All</button><button data-pick="none">None</button><span class="choose-count faint"></span></div>
    <div class="choose-list">${sc.traces.map((t, i) => `<label class="choose-row"><input type="checkbox" data-i="${i}" ${sc.ticked[i] ? "checked" : ""} aria-label="Open ${esc(t.name)}">
      <span class="n">${esc(t.name)}</span><span class="s">${esc(fmtBytes(t.size || 0))}</span></label>`).join("")}</div>
    <div class="choose-foot"><button class="primary" id="choose-open"></button><button id="choose-all" title="Every one: it takes a while, and memory">Open all ${n}</button><span class="grow"></span><button id="choose-cancel">Cancel</button></div>
  </div>`;
  let last = -1; // the box last clicked: Shift-click ticks the run up to another
  const sync = () => {
    const picked = sc.traces.filter((_, i) => sc.ticked[i]);
    el.querySelectorAll(".choose-list input").forEach((b) => { b.checked = sc.ticked[+b.dataset.i]; });
    el.querySelector(".choose-count").textContent = `${picked.length} ticked · ${fmtBytes(uniqueBytes(picked))}`;
    const open = el.querySelector("#choose-open");
    open.textContent = `Open ${picked.length} ticked`;
    open.disabled = !picked.length;
  };
  el.querySelectorAll(".choose-list input").forEach((b) => b.addEventListener("click", (e) => {
    const i = +b.dataset.i;
    if (e.shiftKey && last >= 0) for (let j = Math.min(i, last); j <= Math.max(i, last); j++) sc.ticked[j] = b.checked;
    else sc.ticked[i] = b.checked;
    last = i;
    sync();
  }));
  el.querySelectorAll("[data-pick]").forEach((b) => b.addEventListener("click", () => {
    const p = b.dataset.pick;
    const spread = new Set(spreadPick(n, k));
    sc.ticked = sc.traces.map((_, i) => p === "all" || (p === "first" && i < k) || (p === "spread" && spread.has(i)));
    sync();
  }));
  el.querySelector("#choose-open").addEventListener("click", () => openPaths(sc.traces.filter((_, i) => sc.ticked[i]).map((t) => t.path), { all: true }));
  el.querySelector("#choose-all").addEventListener("click", () => openPaths(sc.inputs, { all: true }));
  el.querySelector("#choose-cancel").addEventListener("click", () => { showScreen(S.meta ? null : { kind: "empty" }); });
  sync();
}

// pollProgress asks the server how far the loads of the tab shown are: the
// paths it is listing, or its traces loading.
let polling = false;
async function pollProgress() {
  if (polling || BACKGROUND || !S) return;
  const asks = S.listing ? S.listing.map((x) => ({ x, path: x.path, listing: true }))
    : S.traces.filter((T) => T.loading && !T.meta).map((T) => ({ x: T, path: T.path }));
  if (!asks.length) return;
  polling = true;
  const doc = DOC;
  try {
    const res = await api("progress", { path: asks.map((a) => a.path) });
    withDoc(doc, () => {
      (res.files || []).forEach((f, i) => {
        const a = asks[i];
        if (!a || (a.listing && f.phase === "queued")) return;
        // Loaded there: on its way here until its rows come.
        const phase = f.phase === "done" || f.phase === "failed" ? "receiving" : f.phase;
        a.x.progress = { phase, fraction: Math.min(f.fraction, 0.99) };
      });
      if (S.screen && S.screen.kind === "loading") renderScreen();
      renderSidebar();
      invalidate();
    });
  } catch { /* an older server, or the network: the bars wait */ } finally { polling = false; }
}
setInterval(pollProgress, 250);

function showScreen(screen) {
  S.screen = screen;
  renderScreen();
  renderDocTabs();
}
function renderScreen() {
  if (BACKGROUND) return;
  const loading = $("loading"), sc = S.screen;
  loading.hidden = !sc;
  if (!sc) return;
  if (sc.kind === "loading") { loading.innerHTML = loadingHTML(sc.text); return; }
  if (sc.kind === "choose") { renderChooser(loading, sc); return; }
  const r = recents();
  loading.innerHTML = `<div class="empty-title">Open a trace</div>
    <div class="empty-hint">Type its absolute path on the server in the box above and press Enter: a folder opens every trace in it, side by side (one per rank or node), as do a glob, a brace pattern (rank{0..7}.nsys-rep) or several paths. ${esc(modKey())}+Enter opens it in a new tab.</div>
    ${r.length ? `<div class="recents"><div class="section-title">RECENT</div>${r.map((p, i) => `<a class="recent" data-i="${i}" tabindex="0" title="${esc(p)}">${esc(p)}</a>`).join("")}</div>` : ""}
    <button id="empty-browse">Browse files under the server's root…</button>`;
  loading.querySelectorAll(".recent").forEach((a) => {
    const go = (e) => openPaths(splitPaths(r[+a.dataset.i]), { newTab: e.metaKey || e.ctrlKey });
    a.addEventListener("click", go);
    a.addEventListener("keydown", (e) => { if (e.key === "Enter") go(e); });
  });
  $("empty-browse").addEventListener("click", () => openPicker(""));
}
const modKey = () => /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";

function submitPath(newTab) {
  const p = $("path-input").value.trim();
  if (!p) { pathError("Type the path of a trace or a folder."); return; }
  openPaths(splitPaths(p), { newTab });
}
$("path-form").addEventListener("submit", (e) => { e.preventDefault(); submitPath(false); });
$("path-input").addEventListener("input", () => pathError(""));
$("path-input").addEventListener("keydown", (e) => {
  if (e.key === "Escape") { pathError(""); if (S.meta) { $("path-input").value = S.inputs.join(" "); canvas.focus(); } }
  if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); submitPath(true); }
});
document.addEventListener("keydown", (e) => {
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement && document.activeElement.tagName);
  if (((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === "l") || (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey)) {
    e.preventDefault();
    focusPath();
    return;
  }
  if (tabShortcut(e)) return;
  // Wherever the focus is (but in a field, or a dialog): Esc clears the
  // selection and the range, Enter or Z zooms to the range.
  if (typing || e.metaKey || e.ctrlKey || e.altKey || !S.meta || !$("picker").hidden || !$("token-dialog").hidden) return;
  const k = e.key.toLowerCase(), onButton = document.activeElement && document.activeElement.tagName === "BUTTON";
  if (k === "escape") { S.selected = null; clearRange(); renderPanel(); saveHash(); invalidate(); }
  else if (k === "c") toggleCompact();
  else if ((k === "enter" && !onButton) || k === "z") { if (rangeSpan()) { e.preventDefault(); zoomToRange(); } }
});
renderRecents();
focusPath();

// A token comes in the URL's fragment (#token=…), which browsers never send
// to the server, and stays there, so that the address can be bookmarked;
// it is also kept for the tab. One put in the query by mistake moves to the
// fragment.
(function takeToken() {
  const u = new URL(location.href);
  const fromQuery = u.searchParams.get("token");
  const fromHash = /(?:^#|&)token=([^&]+)/.exec(u.hash);
  const tok = (fromHash && decodeURIComponent(fromHash[1])) || fromQuery;
  if (tok) {
    S.token = tok;
    S.tokenInURL = true;
    sessionStorage.setItem("nsa-token", tok);
    if (fromQuery) {
      u.searchParams.delete("token");
      if (!fromHash) u.hash = (u.hash ? u.hash + "&" : "#") + "token=" + encodeURIComponent(tok);
      history.replaceState(null, "", u.toString());
    }
  }
})();
start();
