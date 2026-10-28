// webcheck drives the web UI in headless Chrome through the DevTools
// protocol: it zooms with Ctrl+wheel, pans by dragging, hovers and clicks a
// kernel, checks what the page did after each, and saves a screenshot.
//
//   tracelight serve --root samples --addr 127.0.0.1:8737 &
//   node --experimental-websocket scripts/webcheck.mjs "http://127.0.0.1:8737/?path=/abs/sample.sqlite" out.png [dark|light]
//
// A path of a folder of traces (one per rank) checks the session too: a
// heading per trace, folding, and the alignment toggle.
//
// The last argument is the system appearance the page sees for the
// screenshot (prefers-color-scheme).
//
// No packages needed: Node 20+ and Chrome (CHROME=/path/to/chrome).
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [url, out = "webcheck.png", appearance = "dark"] = process.argv.slice(2);
if (!url) { console.error("usage: webcheck.mjs URL [out.png]"); process.exit(2); }
const W = 1600, H = 1000, PORT = 9300 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(tmpdir(), "webcheck-"));
const chrome = spawn(process.env.CHROME || "google-chrome", [
  "--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, `--window-size=${W},${H}`, "about:blank",
], { stdio: "ignore" });
const splitPathsLocal = (v) => v.trim().split(/\s+(?=\/)/).filter(Boolean);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const check = (ok, what, detail = "") => { console.log(`${ok ? "ok  " : "FAIL"} ${what}${detail ? ": " + detail : ""}`); if (!ok) failures++; };

async function main() {
  let target;
  for (let i = 0; i < 100 && !target; i++) {
    await sleep(100);
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
      target = list.find((t) => t.type === "page");
    } catch {}
  }
  if (!target) throw new Error("Chrome did not start");
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let id = 0;
  const pending = new Map(), logs = [];
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
    if (msg.method === "Runtime.exceptionThrown") logs.push("exception: " + msg.params.exceptionDetails.text + " " + (msg.params.exceptionDetails.exception?.description || ""));
    if (msg.method === "Runtime.consoleAPICalled" && msg.params.type === "error") logs.push("console: " + msg.params.args.map((a) => a.value ?? a.description).join(" "));
    if (msg.method === "Log.entryAdded" && msg.params.entry.level === "error") logs.push("log: " + msg.params.entry.text);
  };
  const send = (method, params = {}) => new Promise((r) => { const n = ++id; pending.set(n, r); ws.send(JSON.stringify({ id: n, method, params })); });
  const evaluate = async (expr) => {
    const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result.exceptionDetails) throw new Error(expr + ": " + (r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text));
    return r.result.result.value;
  };
  const until = async (expr, what, ms = 15000) => {
    for (let t = 0; t < ms; t += 100) {
      try { if (await evaluate(expr)) return true; } catch { /* the page is still loading */ }
      await sleep(100);
    }
    check(false, what, "timed out");
    return false;
  };
  const settled = `S.meta && S.traces.every(t => t.folded || t.meta || t.error) && !S.dirty && !S.animating && S.inflight === 0 && S.queue.length === 0`;
  const mouse = (type, x, y, extra = {}) => send("Input.dispatchMouseEvent", { type, x, y, ...extra });

  // The compact view (on by itself with many traces): a strip per trace and
  // its summary lanes, names in the gutter, the lane filter, C toggling it
  // (left in the full view for the checks after).
  const checkCompact = async () => {
    await until(settled, "the compact view settles");
    await until(`S.traces.every(t => t.meta || t.error)`, "every trace of the compact view loads", 30000);
    await until(settled, "the compact view's tiles");
    const st = await evaluate(`({ n: S.traces.length, strips: S.rows.filter(r => r.kind === "strip").length, lanes: S.rows.filter(r => r.kind === "summary").length,
      heads: S.rows.filter(r => r.kind === "trace").length, ranks: S.ranks.length, idle: S.ranks.map(r => r.idle), boxes: S.boxes.length / BOX })`);
    check(st.strips === st.n && st.heads === 0 && st.ranks === st.n && st.lanes >= st.n, "the compact view: a strip and summary lanes per trace",
      `${st.n} traces, ${st.lanes} lanes, ${st.boxes} blocks drawn`);
    check(st.idle.every((v) => v >= -1 && v <= 1) && st.idle.some((v) => v >= 0), "busy/idle strips measured", st.idle.map((v) => v.toFixed(2)).join(" "));
    const lane = await evaluate(`lanesOn().find((k) => k !== "gpu")`);
    await evaluate(`setLane(${JSON.stringify(lane)}, false)`);
    const filtered = await evaluate(`S.rows.filter(r => r.kind === "summary").length`);
    check(filtered < st.lanes && !JSON.parse(await evaluate(`localStorage.getItem("nsa-compact-lanes")`)).includes(lane), "the lane filter applies to every trace", `${st.lanes} → ${filtered} lanes`);
    await evaluate(`setLane(${JSON.stringify(lane)}, true)`);
    // Parallel work stacked in sub-lanes; Overlaid draws a kind in one.
    const subs = await evaluate(`S.rows.filter(r => r.kind === "summary" && r.sub > 0).length`);
    const bands = await evaluate(`S.rows.filter(r => r.kind === "summary" && r.top).length`);
    check(bands > 0, "each thread's nested ranges in a band of their own, marked", `${bands} bands past the first`);
    await evaluate(`setOverlaid(true)`);
    const flat = await evaluate(`({ lanes: S.rows.filter(r => r.kind === "summary").length, kept: localStorage.getItem("nsa-compact-overlaid") })`);
    check(subs > 0 && flat.lanes === st.lanes - subs && flat.kept === "1", "parallel work stacks in sub-lanes; Overlaid draws a kind in one, kept",
      `${subs} sub-lanes past the first; ${st.lanes} → ${flat.lanes} lanes`);
    await evaluate(`setOverlaid(false)`);
    await evaluate(`document.getElementById("timeline").focus()`);
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "c", code: "KeyC", text: "c", windowsVirtualKeyCode: 67 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "c", code: "KeyC", windowsVirtualKeyCode: 67 });
    const off = await evaluate(`({ on: compactOn(), kept: localStorage.getItem("nsa-compact"), heads: S.rows.filter(r => r.kind === "trace").length })`);
    check(!off.on && off.kept === "off" && off.heads === st.n, "C switches to the full view, and is kept", JSON.stringify(off));
    await until(settled, "the full view settles");
  };

  // A session's traces left out and shown again: a box each in the
  // sidebar, Alt-click for one alone, Show all; the address says which
  // (#omit=, #only=), and a link with it opens so. Then some traces picked
  // in the file browser open side by side.
  const checkTraceFilter = async (n) => {
    const state = `({ rows: [...new Set(S.rows.filter(r => r.t).map(r => r.t.i))], head: document.querySelector(".traces-head").textContent, hash: location.hash })`;
    const box = (i, alt) => evaluate(`document.querySelector('.trace-item input[data-show="${i}"]').dispatchEvent(new MouseEvent("click", { bubbles: true, altKey: ${alt} }))`);
    await box(1, false);
    await sleep(400);
    let f = await evaluate(state);
    check(!f.rows.includes(1) && f.rows.length === n - 1 && f.head.startsWith(`${n - 1} of ${n} shown`), "unticking a trace leaves it out", `${f.rows.length} of ${n} drawn: ${f.head}`);
    check(/omit=/.test(f.hash), "the address says which is left out", f.hash.replace(/token=[^&]*/, "token=…"));
    await box(2, true);
    await sleep(400);
    f = await evaluate(state);
    check(f.rows.length === 1 && f.rows[0] === 2 && /only=/.test(f.hash), "Alt-click shows that trace alone", `${f.rows}`);
    const link = await evaluate(`location.href`);
    await evaluate(`document.getElementById("traces-all").click()`);
    f = await evaluate(state);
    check(f.rows.length === n, "Show all shows every trace again", `${f.rows.length}`);
    // The link of a trace alone opens so (a page of its own).
    await send("Page.navigate", { url: "about:blank" });
    await sleep(200);
    await send("Page.navigate", { url: link });
    await until(`S.traces.length === ${n} && ${settled}`, "the link with only= opens");
    f = await evaluate(state);
    check(f.rows.length === 1 && f.rows[0] === 2, "a link with only= shows that trace alone", `${f.rows}`);
    await evaluate(`showAllTraces()`);
    // The file browser: two traces picked, opened side by side.
    const inputs = await evaluate(`S.inputs`);
    const dir = await evaluate(`(() => { const p = S.traces[0].path, r = S.info.root.endsWith("/") ? S.info.root : S.info.root + "/"; const d = p.slice(0, p.lastIndexOf("/") + 1); return d.startsWith(r) ? d.slice(r.length, -1) : ""; })()`);
    await evaluate(`openPicker(${JSON.stringify(dir)})`);
    await until(`document.querySelectorAll("#files input[data-pick], #files .file .n").length > 0 && !document.querySelector("#files .muted")`, "the browser lists the folder");
    if ((await evaluate(`document.querySelectorAll("#files input[data-pick]").length`)) < 2) { // one file of ranks merged
      await evaluate(`document.getElementById("picker").hidden = true`);
      return;
    }
    await evaluate(`(() => { const b = document.querySelectorAll("#files input[data-pick]"); b[b.length - 1].click(); b[0].click(); })()`);
    const label = await evaluate(`document.getElementById("open-picked").hidden ? "" : document.getElementById("open-picked").textContent`);
    check(label === "Open 2 picked side by side", "picking traces in the browser offers to open them", label);
    await evaluate(`document.getElementById("open-picked").click()`);
    // (Two files; a file of merged ranks opens as its ranks.)
    await until(`S.inputs.length === 2 && S.traces.length >= 2 && ${settled}`, "the picked traces open side by side");
    check((await evaluate(`S.inputs.length === 2 && S.traces.every(T => S.inputs.some(p => T.path.replace(/#rank\\d+$/, "").endsWith(p.replace(/^\\/*/, "/"))))`)), "the picked traces open side by side, those alone");
    await evaluate(`openPaths(${JSON.stringify(inputs)})`);
    await until(`S.traces.length === ${n} && ${settled}`, "back to the session");
  };

  // A session of several traces: a heading for each, on one time axis.
  const checkSession = async () => {
    const wasCompact = await evaluate(`compactOn()`);
    if (wasCompact) await checkCompact();
    const st = await evaluate(`({ n: S.traces.length, heads: S.rows.filter(r => r.kind === "trace").length,
      offs: S.traces.map(offsetOf), clocks: S.traces.filter(t => t.sessionStart > 0).length,
      // Each first event at the moment it happened: the capture's exact wall
      // clock (BigInt) plus the first event's time, the earliest at 0.
      want: (() => { const abs = S.traces.map(t => BigInt(t.sessionStartNs || "0") + BigInt(t.meta ? t.meta.start : 0));
        const o = abs.reduce((a, b) => b < a ? b : a); return abs.map(a => Number(a - o)); })(),
      bases: S.traces.map(t => t.basis) })`);
    check(st.heads === st.n, "a heading per trace", `${st.heads} of ${st.n}`);
    const spread = Math.max(...st.offs) - Math.min(...st.offs);
    // The default, collectives: on these synthetic ranks (in lockstep)
    // where the clocks put them, to microseconds; then Clocks exactly.
    if (st.clocks === st.n) {
      check(st.bases.every((b) => b === "collectives") && st.offs.every((o, i) => Math.abs(o - st.want[i]) < 50e3), "collective alignment lines the ranks up on their collective kernels", st.offs.map((o, i) => ((o - st.want[i]) / 1e3).toFixed(1)).join(" ") + " us from the clocks");
      await evaluate(`document.querySelector('#align-mode [data-align="abs"]').click()`);
      const ab = await evaluate(`({ offs: S.traces.map(offsetOf), bases: S.traces.map(t => t.basis) })`);
      check(ab.offs.every((o, i) => o === st.want[i]) && ab.bases.every((b) => b === "wall"), "Clocks places each trace at the moment it captured, by its wall clock", `${st.clocks} clocks, spread ${(spread / 1e6).toFixed(1)} ms`);
    } else {
      // Reports read natively have no wall clock: placed through their host's
      // clock (see alignTraces).
      check(st.bases.every((b) => ["wall", "host", "monotonic", "start", "collectives"].includes(b)), "alignment of traces without a wall clock", st.bases.join(", ") + `, spread ${(spread / 1e6).toFixed(1)} ms`);
    }
    await evaluate(`document.querySelector('#align-mode [data-align="start"]').click()`);
    const s2 = await evaluate(`({ offs: S.traces.map(offsetOf), on: document.querySelector('#align-mode .on').dataset.align })`);
    check(s2.offs.every((o) => o === 0) && s2.on === "start", "Starts aligns every trace at 0");
    await sleep(400);
    check(/align=start/.test(await evaluate(`location.hash`)), "the address keeps the alignment");
    await evaluate(`document.querySelector('#align-mode [data-align="collectives"]').click()`);
    // Clicking a heading folds the trace, and again unfolds it.
    const head = await evaluate(`(() => { const r = S.rows.find(r => r.kind === "trace"); return { y: RULER_H + r.y - S.scrollY + 10, rows: S.rows.length }; })()`);
    await mouse("mousePressed", canvas.x + 60, canvas.y + head.y, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseReleased", canvas.x + 60, canvas.y + head.y, { button: "left", clickCount: 1 });
    const folded = await evaluate(`({ folded: S.traces[0].folded, rows: S.rows.length })`);
    check(folded.folded && folded.rows < head.rows, "clicking a heading folds its trace", `${head.rows} → ${folded.rows} rows`);
    await mouse("mousePressed", canvas.x + 60, canvas.y + head.y, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseReleased", canvas.x + 60, canvas.y + head.y, { button: "left", clickCount: 1 });
    check(!(await evaluate(`S.traces[0].folded`)), "and clicking it again unfolds it");
    const items = await evaluate(`document.querySelectorAll(".trace-item").length`);
    check(items === st.n, "the sidebar lists the traces", `${items}`);
    await checkTraceFilter(st.n);
    if (wasCompact) await evaluate(`setCompact("auto")`); // back, for the screenshot
    await until(settled, "the session settles");
  };

  // A Shift+drag range: the summaries are of it; Esc, its ×, and a click on
  // an empty part of the timeline clear it; Enter zooms to it.
  // Go to: a kernel's "Go to launch" (up in the details) selects its CUDA
  // API call and shows it, its row scrolled into view, with the kernel
  // where both fit; back with "Go to kernel". A flow line whose other end
  // is out of view ends in a chip at the view's edge, which goes there.
  const checkGoTo = async () => {
    const visible = `(() => { const b = eventBox(S.selected); return !!b && b.y >= RULER_H && b.y + b.h <= size.h && b.x + b.w >= LABEL_W && b.x <= size.w; })()`;
    const k = await evaluate(`(async () => {
      for (const T of S.traces.filter((t) => t.meta && !t.off)) {
        const rows = T.meta.rows;
        for (let row = 0; row < rows.length; row++) {
          if (rows[row].kind !== "stream") continue;
          for (let i = 0; i < 60; i += 13) {
            const ev = await api("event", { path: T.path, row, lane: 0, index: i }).catch(() => null);
            if (ev && ev.linked && ev.kindId === KIND.kernel) { await select(T, row, 0, i); S.scrollY = 0; return { row, i }; }
          }
        }
      }
      return null;
    })()`);
    check(!!k, "a kernel with its launch, to go from");
    if (!k) return;
    await until(`!!document.getElementById("goto-head")`, "the details offer Go to launch");
    const label = await evaluate(`document.getElementById("goto-head").textContent`);
    const r = await evaluate(`(() => { const b = document.getElementById("goto-head").getBoundingClientRect(); return { top: b.top, bottom: b.bottom, right: b.right, h: innerHeight, w: innerWidth }; })()`);
    check(label === "Go to launch" && r.bottom <= r.h && r.right <= r.w, "Go to launch shows, up in the details, without scrolling them", `${label}, ${Math.round(r.top)}–${Math.round(r.bottom)} px of ${H}`);
    await evaluate(`document.getElementById("goto-head").click()`);
    await until(`S.selected && S.selected.kindId === KIND.api && ${settled}`, "the launch selected");
    check(await evaluate(visible), "Go to launch shows the CUDA API call, scrolled into view");
    await until(`!!document.getElementById("goto-head")`, "the details offer Go to kernel");
    check((await evaluate(`document.getElementById("goto-head").textContent`)).startsWith("Go to "), "and offer to go back", await evaluate(`document.getElementById("goto-head").textContent`));
    // The line's chip: the kernel out of view (zoomed into the launch).
    await evaluate(`zoomTo(...selectedSpan(), false); S.target = { ...S.view }; S.animating = false; invalidate()`);
    await until(`!!S.linkChip`, "a chip where the flow line leaves the view", 5000);
    const chip = await evaluate(`S.linkChip`);
    if (chip) {
      await mouse("mousePressed", canvas.x + chip.x + chip.w / 2, canvas.y + chip.y + chip.h / 2, { button: "left", clickCount: 1, buttons: 1 });
      await mouse("mouseReleased", canvas.x + chip.x + chip.w / 2, canvas.y + chip.y + chip.h / 2, { button: "left", clickCount: 1 });
      await until(`S.selected && S.selected.kindId !== KIND.api && ${settled}`, "the chip goes to the kernel");
      check(await evaluate(visible), "the chip goes to the GPU work, in view");
    }
    await evaluate(`S.selected = null; renderPanel(); fit(false)`);
    // The label gutter's edge drags its width, kept; a double click there
    // sets it back.
    const w0 = await evaluate(`LABEL_W`), gy = canvas.y + 200;
    await mouse("mouseMoved", canvas.x + w0 - 1, gy);
    const cur = await evaluate(`canvas.style.cursor`);
    await mouse("mousePressed", canvas.x + w0 - 1, gy, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseMoved", canvas.x + 400, gy, { buttons: 1 });
    await mouse("mouseReleased", canvas.x + 400, gy, { button: "left", clickCount: 1 });
    const g = await evaluate(`({ w: LABEL_W, kept: localStorage.getItem("nsa-label-w") })`);
    check(cur === "col-resize" && g.w === 400 && g.kept === "400", "the label gutter's edge drags its width, kept", `${cur}, ${w0} → ${g.w} (kept ${g.kept})`);
    await mouse("mousePressed", canvas.x + 399, gy, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseReleased", canvas.x + 399, gy, { button: "left", clickCount: 1 });
    await mouse("mousePressed", canvas.x + 399, gy, { button: "left", clickCount: 2, buttons: 1 });
    await mouse("mouseReleased", canvas.x + 399, gy, { button: "left", clickCount: 2 });
    const back = await evaluate(`({ w: LABEL_W, kept: localStorage.getItem("nsa-label-w") })`);
    check(back.w === w0 && back.kept === null, "a double click on it sets it back", `${back.w}`);
  };

  const checkRange = async () => {
    await evaluate(`fit(false)`);
    await until(settled, "tiles before measuring");
    const y = canvas.y + 200, x0 = canvas.x + 600, x1 = canvas.x + 900;
    const shiftDrag = async () => {
      await mouse("mousePressed", x0, y, { button: "left", clickCount: 1, buttons: 1, modifiers: 8 });
      for (let x = x0 + 30; x <= x1; x += 30) await mouse("mouseMoved", x, y, { button: "left", buttons: 1, modifiers: 8 });
      await mouse("mouseReleased", x1, y, { button: "left", clickCount: 1, modifiers: 8 });
      await mouse("mouseMoved", canvas.x + 10, canvas.y + 10);
      return evaluate(`({ r: rangeSpan(), button: !document.getElementById("zoom-range").hidden })`);
    };
    let st = await shiftDrag();
    check(!!st.r && st.button, "Shift+drag measures a range, and offers to zoom to it");
    // Top kernels over the range, then over the whole trace.
    await evaluate(`document.querySelector('.tab[data-tab="1"]').click()`);
    await until(`document.querySelector(".sum-head") && /In range/.test(document.querySelector(".sum-head").textContent) && document.querySelector("table.summary, .empty:not(:first-child)")`, "the summary of the range");
    const inRange = await evaluate(`({ head: document.querySelector(".sum-head").textContent, n: [...document.querySelectorAll("tr.stat td:nth-child(2)")].reduce((a, td) => a + +td.textContent.replace(/,/g, ""), 0) })`);
    // Esc, with the focus off the timeline.
    await evaluate(`document.querySelector('.tab[data-tab="1"]').focus?.(); document.activeElement.blur()`);
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
    st = await evaluate(`({ r: rangeSpan(), button: !document.getElementById("zoom-range").hidden })`);
    check(!st.r && !st.button, "Esc clears the range, wherever the focus is");
    await until(`document.querySelector(".sum-head") && /Whole trace/.test(document.querySelector(".sum-head").textContent) && document.querySelector("table.summary")`, "the summary of the whole trace");
    const whole = await evaluate(`[...document.querySelectorAll("tr.stat td:nth-child(2)")].reduce((a, td) => a + +td.textContent.replace(/,/g, ""), 0)`);
    check(inRange.n > 0 && inRange.n < whole, "the summary is of the range, then of the whole trace", `${inRange.head}: ${inRange.n} kernels of the top 100, ${whole} in the whole trace`);
    // Its × clears it.
    await shiftDrag();
    const rc = await evaluate(`S.rangeClose`);
    await mouse("mousePressed", canvas.x + rc.x, canvas.y + rc.y, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseReleased", canvas.x + rc.x, canvas.y + rc.y, { button: "left", clickCount: 1 });
    check(!(await evaluate(`rangeSpan()`)), "the × on the ruler clears the range");
    // A plain click where there is no event clears it.
    await shiftDrag();
    const empty = await evaluate(`(() => { for (let yy = RULER_H + 10; yy < canvas.clientHeight; yy += 6) for (let xx = LABEL_W + 20; xx < canvas.clientWidth; xx += 40) {
      const ri = rowAt(yy); if (ri >= 0 && (S.rows[ri].kind === "track" || S.rows[ri].kind === "summary") && !hitAt(xx, yy)) return { x: xx, y: yy }; } return null; })()`);
    if (empty) {
      await mouse("mousePressed", canvas.x + empty.x, canvas.y + empty.y, { button: "left", clickCount: 1, buttons: 1 });
      await mouse("mouseReleased", canvas.x + empty.x, canvas.y + empty.y, { button: "left", clickCount: 1 });
    }
    check(!!empty && !(await evaluate(`rangeSpan()`)), "a click on an empty part of the timeline clears the range");
    // Enter zooms to it.
    st = await shiftDrag();
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
    await until(`!S.animating`, "the zoom to the range");
    const v = await evaluate(`({ a: S.view.start, b: S.view.start + S.view.scale * dataW(), s: S.view.scale })`);
    check(v.a <= st.r[0] && v.b >= st.r[1] && v.b - v.a < 1.1 * (st.r[1] - st.r[0]), "Enter zooms to the range (with zoomTo's margin)");
    await evaluate(`clearRange(); S.panelTab = 0; renderPanel()`);
  };

  // Tabs: Alt+T opens one, Ctrl+Enter in the path box opens a path in a
  // new one, which loads while another is shown; Alt+digit and Alt+[ ]
  // switch, each keeping its view; Alt+W closes; a reload brings them back.
  const altKey = async (key, code, vk) => {
    await send("Input.dispatchKeyEvent", { type: "keyDown", key, code, windowsVirtualKeyCode: vk, modifiers: 1 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key, code, windowsVirtualKeyCode: vk, modifiers: 1 });
  };
  const tabExpr = (extra = "") => `({ n: DOCS.length, cur: DOCS.indexOf(DOC), inputs: S.inputs, view: S.target, title: document.title,
    search: location.search, tabs: [...document.querySelectorAll("#doc-tabs .doc-tab")].map(e => e.textContent.replace("×", "").trim()) ${extra} })`;
  const tabState = tabExpr();
  const checkTabs = async (abs) => {
    await sleep(450); // the address follows after saveHash's debounce
    const a = await evaluate(tabState);
    check(a.n === 1 && a.tabs.length === 1 && a.tabs[0] === await evaluate(`tabTitle(S.inputs)`), "one tab, named after its trace", a.tabs.join(" | "));
    await altKey("t", "KeyT", 84);
    await sleep(450); // the address follows after saveHash's debounce
    const e = await evaluate(tabExpr(", focus: document.activeElement.id"));
    check(e.n === 2 && e.cur === 1 && !e.inputs.length && e.search === "" && e.title === "Tracelight" && e.tabs[1] === "New tab",
      "Alt+T opens an empty tab, shown, the address cleared", JSON.stringify({ n: e.n, cur: e.cur, search: e.search, tabs: e.tabs }));
    await altKey("w", "KeyW", 87);
    check((await evaluate(tabState)).n === 1, "Alt+W closes it");
    // Ctrl+Enter in the path box: a new tab; Alt+1 back while it loads.
    await evaluate(`(() => { const i = document.getElementById("path-input"); i.value = ${JSON.stringify(abs)}; i.focus(); })()`);
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, modifiers: 2 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, modifiers: 2 });
    const opening = await evaluate(tabState);
    await altKey("1", "Digit1", 49);
    const back = await evaluate(tabState);
    check(opening.n === 2 && opening.cur === 1 && back.cur === 0 && JSON.stringify(back.view) === JSON.stringify(a.view) && JSON.stringify(back.inputs) === JSON.stringify(a.inputs),
      "Ctrl+Enter opens the path in a new tab; Alt+1 goes back to the first, its view as it was", `${opening.n} tabs, back on ${back.cur}`);
    await until(`DOCS[1].state.traces.length > 0 && DOCS[1].state.traces.every(t => t.folded || t.meta || t.error) && DOCS[1].state.screen === null`,
      "the new tab loads while not shown", 30000);
    await sleep(450);
    const still = await evaluate(tabState);
    check(still.cur === 0 && JSON.stringify(still.view) === JSON.stringify(a.view) && still.search === a.search, "the first tab stays as it was meanwhile",
      JSON.stringify({ cur: still.cur, view: [still.view, a.view], search: [still.search, a.search] }));
    await altKey("2", "Digit2", 50);
    await until(settled, "the second tab's tiles");
    await sleep(450);
    const b = await evaluate(tabExpr(", tiles: S.traces.reduce((n, t) => n + [...t.tiles.values()].filter(x => x.data).length, 0)"));
    check(b.cur === 1 && b.tiles > 0 && b.search.startsWith("?path=") && JSON.stringify(b.inputs) === JSON.stringify([abs]),
      "Alt+2 shows it, drawn, the address following", `${b.tiles} tiles, ${b.search.slice(0, 60)}`);
    await evaluate(`zoomTo(S.view.start, S.view.start + S.view.scale * dataW() / 8, false)`);
    const bView = await evaluate(`S.target`);
    await altKey("[", "BracketLeft", 219);
    const c0 = await evaluate(tabState);
    await altKey("]", "BracketRight", 221);
    const c1 = await evaluate(tabState);
    check(c0.cur === 0 && c1.cur === 1 && JSON.stringify(c1.view) === JSON.stringify(bView) && JSON.stringify(c0.view) === JSON.stringify(a.view),
      "Alt+[ and Alt+] switch, each tab keeping its own view");
    // Dragging a tab along the bar moves it there and shows it; a click
    // after is a click again; Alt+Shift+Page Up moves the tab shown back.
    {
      const ids = await evaluate(`DOCS.map((d) => d.id)`);
      const rs = await evaluate(`[...document.querySelectorAll("#doc-tabs .doc-tab")].map((e) => { const r = e.getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; })`);
      const y = rs[0].y + rs[0].h / 2, x0 = rs[0].x + rs[0].w / 2, x1 = rs[1].x + rs[1].w * 0.9;
      await mouse("mousePressed", x0, y, { button: "left", clickCount: 1, buttons: 1 });
      for (let k = 1; k <= 8; k++) await mouse("mouseMoved", x0 + (x1 - x0) * k / 8, y, { button: "left", buttons: 1 });
      const mid = await evaluate(`({ dragging: !!document.querySelector("#doc-tabs .doc-tab.dragging"), other: document.querySelectorAll("#doc-tabs .doc-tab")[1].style.transform })`);
      await mouse("mouseReleased", x1, y, { button: "left", clickCount: 1 });
      await sleep(100);
      const after = await evaluate(`({ ids: DOCS.map((d) => d.id), cur: DOCS.indexOf(DOC), id: DOC.id })`);
      check(mid.dragging && /translateX\(-/.test(mid.other) && JSON.stringify(after.ids) === JSON.stringify([ids[1], ids[0]]) && after.id === ids[0] && after.cur === 1,
        "dragging a tab past another moves it there, the other making room, and shows it", JSON.stringify({ mid, after }));
      await send("Input.dispatchKeyEvent", { type: "keyDown", key: "PageUp", code: "PageUp", windowsVirtualKeyCode: 33, modifiers: 9 });
      await send("Input.dispatchKeyEvent", { type: "keyUp", key: "PageUp", code: "PageUp", windowsVirtualKeyCode: 33, modifiers: 9 });
      const back = await evaluate(`({ ids: DOCS.map((d) => d.id), cur: DOCS.indexOf(DOC) })`);
      check(JSON.stringify(back.ids) === JSON.stringify(ids) && back.cur === 0, "Alt+Shift+Page Up moves the tab shown left");
      await altKey("2", "Digit2", 50);
      await until(settled, "the second tab again");
      const kept = await evaluate(`S.target`);
      check(Math.abs(kept.scale - bView.scale) / bView.scale < 1e-9, "each tab keeps its view through the moves");
    }
    // A reload brings the tabs back; the one not shown loads when shown.
    await sleep(500); // saveHash's debounce
    await send("Page.reload");
    await until(settled + " && DOCS.length === 2", "the tabs come back after a reload", 30000);
    const r = await evaluate(tabExpr(", lazy: DOCS.map(d => !!d.lazy)"));
    check(r.n === 2 && r.cur === 1 && r.lazy[0] && Math.abs(r.view.scale - bView.scale) / bView.scale < 0.01, "a reload brings back the tabs and the shown one's view", JSON.stringify({ cur: r.cur, lazy: r.lazy }));
    await altKey("1", "Digit1", 49);
    await until(settled, "the restored tab loads when shown", 30000);
    const r1 = await evaluate(tabState);
    check(r1.cur === 0 && JSON.stringify(r1.inputs) === JSON.stringify(a.inputs) && Math.abs(r1.view.scale - a.view.scale) / a.view.scale < 0.01, "and the other loads when shown, at its view");
    await altKey("2", "Digit2", 50);
    await evaluate(`closeTab(DOC)`);
    await until(settled, "back to one tab");
    check((await evaluate(tabState)).n === 1, "closing leaves the first tab");
  };

  // A PyTorch profile's Python stacks: a kernel's details list the stack
  // it was launched from, the innermost frame first; a frame's click
  // selects its call, in the Python row.
  const checkPython = async () => {
    const found = await evaluate(`(async () => {
      const T = focusTrace(), rows = T.meta.rows;
      const row = rows.findIndex((r) => r.kind === "stream");
      for (let i = 0; i < 400; i += 7) {
        const ev = await api("event", { path: T.path, row, lane: 0, index: i });
        if (ev.stack && ev.stack.length > 3) { await select(T, row, 0, i); return { row, i, n: ev.stack.length, inner: ev.stack[ev.stack.length - 1].name }; }
      }
      return null;
    })()`);
    check(!!found, "a kernel launched under a Python stack", found ? `${found.n} frames` : "");
    if (!found) return;
    await until(`document.querySelectorAll(".py-frame:not(.op)").length === ${found.n}`, "the Python stack in the details");
    const first = await evaluate(`document.querySelector(".py-frame:not(.op)").title`);
    check(first.includes(found.inner), "the innermost frame first", first);
    await evaluate(`document.querySelectorAll(".py-frame:not(.op)")[2].click()`);
    await until(`S.selected && S.traces[S.selected.ti].meta.rows[S.selected.row].kind === "python"`, "the frame's call selected");
    await evaluate(`S.selected = null; renderPanel()`);
  };

  await send("Runtime.enable"); await send("Log.enable"); await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: W, height: H, deviceScaleFactor: 1, mobile: false });
  // (The page hears of a new appearance a moment later, as of a real one.)
  const scheme = async (v) => { await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: v }] }); await sleep(150); };
  await scheme("dark");
  await send("Page.navigate", { url });
  await until(settled, "the trace loads and its first tiles arrive");
  const meta = await evaluate(`({ traces: S.traces.length, events: loadedTraces().reduce((n, t) => n + t.meta.events, 0), rows: loadedTraces().reduce((n, t) => n + t.meta.rows.length, 0),
    tiles: S.traces.reduce((n, t) => n + [...t.tiles.values()].filter(x => x.data).length, 0) })`);
  const ver = await evaluate(`document.getElementById("app-version").textContent`);
  check(/^\d+\.\d+\.\d+$/.test(ver), "the header shows the app's version", ver);
  check(meta.events > 0 && meta.tiles > 0, "trace shown", `${meta.traces} trace(s), ${meta.events} events, ${meta.rows} rows, ${meta.tiles} tiles`);
  const canvas = await evaluate(`(() => { const r = document.getElementById("timeline").getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; })()`);

  const hash = new URLSearchParams(new URL(url).hash.slice(1));
  // lanes=gpu,nvtx in the fragment picks the compact view's lanes.
  if (hash.get("lanes")) {
    await evaluate(`(() => { S.compactLanes = ${JSON.stringify(hash.get("lanes").split(","))}; layoutRows(); renderSidebar(); invalidate(); })()`);
    await until(settled, "the lanes chosen");
  }
  // collapse=<prefix> in the fragment collapses those groups of every trace
  // for the screenshot (say, the CPU ones, for many ranks).
  if (hash.get("collapse")) {
    await evaluate(`(() => { for (const T of loadedTraces()) for (const r of T.meta.rows) if (r.group.startsWith(${JSON.stringify(hash.get("collapse"))})) T.collapsed.add(r.group);
      layoutRows(); renderSidebar(); invalidate(); })()`);
  }

  // tabs=/path1|*|/path2 in the fragment opens those paths in tabs of
  // their own around the address's (*), each at the v window if given, the
  // address's tab shown (for the screenshot).
  if (hash.get("tabs")) {
    const list = hash.get("tabs").split("|"), at = Math.max(0, list.indexOf("*"));
    const win = hash.get("v") ? hash.get("v").split(",").map(Number) : null;
    await evaluate(`window.__home = window.__prev = DOC`);
    for (const [k, p] of list.entries()) {
      if (p === "*") continue;
      // A new tab opens after the one shown: those after * in order.
      await evaluate(`showTab(${k < at ? "window.__home" : "window.__prev"})`);
      await evaluate(`openPaths(${JSON.stringify(splitPathsLocal(p))}, { newTab: true })`);
      await until(settled, "a tab of " + p, 30000);
      if (win) await evaluate(`zoomTo(${win[0]}, ${win[1]}, false)`);
      if (k < at) await evaluate(`(() => { const d = DOCS.splice(DOCS.indexOf(DOC), 1)[0]; DOCS.splice(${k}, 0, d); renderDocTabs(); })()`);
      else await evaluate(`window.__prev = DOC`);
    }
    await evaluate(`showTab(window.__home)`);
    await until(settled, "the address's tab again");
    // How long a switch takes, to the next frame drawn.
    const sw = await evaluate(`(async () => { const ms = []; for (let i = 0; i < 6; i++) { const t0 = performance.now(); cycleTab(1);
      await new Promise(r => requestAnimationFrame(() => r())); ms.push(performance.now() - t0); } return ms; })()`);
    console.log(`tab switch to the next frame: ${sw.map((x) => x.toFixed(1)).join(", ")} ms`);
    await evaluate(`showTab(window.__home)`);
    await until(settled, "the address's tab once more");
  }

  const px = canvas.x + 236 + 600, py = canvas.y + 150;
  const saved = await evaluate(`JSON.stringify([S.view, S.target, S.scrollY])`);
  // Zooming out far past the widest view keeps the trace in view: before
  // 0.6.1 every step past it moved the view left.
  for (const fx of [0.15, 0.85]) {
    const zx = canvas.x + 236 + Math.round((canvas.w - 236) * fx);
    await evaluate(`fit(false)`);
    for (let i = 0; i < 4; i++) await mouse("mouseWheel", zx, py, { deltaX: 0, deltaY: -120, modifiers: 2 });
    await until(settled, "zoomed in");
    for (let i = 0; i < 30; i++) await mouse("mouseWheel", zx, py, { deltaX: 0, deltaY: 120, modifiers: 2 });
    await until(settled, "zoomed all the way out");
    const v = await evaluate(`(() => { const [lo, hi] = extent(); return { lo, hi, start: S.view.start, end: S.view.start + S.view.scale * dataW() }; })()`);
    check(v.start <= v.lo && v.end >= v.hi, `zooming out past the widest view (pointer at ${fx}) keeps the trace in view`,
      `view ${v.start.toFixed(0)}–${v.end.toFixed(0)}, trace ${v.lo.toFixed(0)}–${v.hi.toFixed(0)} ns`);
  }
  // A touchpad's pinch (Chrome sends it as a wheel with ctrlKey) zooms
  // around the pointer, at once.
  await evaluate(`fit(false)`);
  {
    const b = await evaluate(`({ scale: S.view.scale, t: timeAt(${px - canvas.x}) })`);
    // As Chrome sends a pinch to 3×: wheels with ctrlKey whose deltaY add
    // up to -100 ln 3, in pixels (Input.synthesizePinchGesture would make
    // them, but hangs at times in headless Chrome).
    const pinched = await evaluate(`(() => { let n = 0;
      for (let i = 0; i < 12; i++) { const e = new WheelEvent("wheel", { deltaY: -100 * Math.log(3) / 12, deltaMode: 0, ctrlKey: true,
        clientX: ${px}, clientY: ${py}, cancelable: true }); canvas.dispatchEvent(e); n += e.defaultPrevented; }
      return { n, animating: S.animating }; })()`);
    const a = await evaluate(`({ scale: S.view.scale, t: timeAt(${px - canvas.x}) })`);
    check(Math.abs(b.scale / a.scale - 3) < 0.01 && !pinched.animating && pinched.n === 12, "a touchpad pinch to 3× zooms in 3×, at once",
      `${b.scale.toFixed(0)} → ${a.scale.toFixed(0)} ns/px, animating ${pinched.animating}`);
    check(Math.abs(a.t - b.t) < 4 * a.scale + b.scale, "a pinch keeps the time under the pointer", `${b.t.toFixed(0)} vs ${a.t.toFixed(0)} ns`);
    // Safari's gesture events: scale from 1 as the pinch began.
    const g = await evaluate(`(() => {
      const fire = (type, scale) => { const e = new Event(type, { cancelable: true });
        Object.assign(e, { scale, clientX: ${px}, clientY: ${py}, shiftKey: false, altKey: false, ctrlKey: false, metaKey: false }); canvas.dispatchEvent(e); return e.defaultPrevented; };
      const s0 = S.view.scale;
      const prevented = fire("gesturestart", 1) && fire("gesturechange", 0.5) && fire("gestureend", 0.5);
      return { prevented, ratio: S.view.scale / s0 };
    })()`);
    check(g.prevented && Math.abs(g.ratio - 2) < 0.01, "Safari's pinch-out gesture zooms out 2×, not the page", `ratio ${g.ratio.toFixed(3)}, prevented ${g.prevented}`);
  }
  await evaluate(`(() => { const [v, t, y] = JSON.parse(${JSON.stringify(saved)}); S.view = v; S.target = t; S.scrollY = y; S.animating = false; invalidate(); })()`);
  await until(settled, "the view as it was");

  // Ctrl+wheel zooms in around the pointer, keeping the time under it.
  const before = await evaluate(`({ scale: S.view.scale, t: timeAt(${px - canvas.x}) })`);
  for (let i = 0; i < 6; i++) await mouse("mouseWheel", px, py, { deltaX: 0, deltaY: -100, modifiers: 2 });
  await until(settled, "the zoom settles");
  const after = await evaluate(`({ scale: S.view.scale, t: timeAt(${px - canvas.x}) })`);
  check(after.scale < before.scale / 5, "Ctrl+wheel zooms in", `${before.scale.toFixed(0)} → ${after.scale.toFixed(0)} ns/px`);
  check(Math.abs(after.t - before.t) < 2 * after.scale, "the time under the pointer stays", `${before.t.toFixed(0)} vs ${after.t.toFixed(0)} ns`);

  // Dragging pans.
  const start = await evaluate(`S.view.start`);
  await mouse("mousePressed", px, py + 200, { button: "left", clickCount: 1, buttons: 1 });
  for (let dx = 20; dx <= 200; dx += 20) await mouse("mouseMoved", px - dx, py + 200, { button: "left", buttons: 1 });
  await mouse("mouseReleased", px - 200, py + 200, { button: "left", clickCount: 1 });
  const moved = await evaluate(`S.view.start`);
  const scale = await evaluate(`S.view.scale`);
  check(Math.abs(moved - start - 200 * scale) < 2 * scale, "dragging pans", `${((moved - start) / scale).toFixed(1)} px`);
  await until(settled, "tiles for the new view arrive");

  // Zoom to the requested window, then hover and click a kernel there.
  if (hash.get("v")) {
    const [a, b] = hash.get("v").split(",").map(Number);
    await evaluate(`zoomTo(${a}, ${b}, false)`);
  } else {
    await evaluate(`fit(false)`);
  }
  await until(settled, "tiles of the window arrive");
  await evaluate(`new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))`);
  const want = hash.get("hover"); // "row" to hover a kernel of that row
  // findSpot finds a kernel to point at, alone and wide enough, near the
  // middle of the view.
  const findSpot = () => evaluate(`(() => {
    const b = S.boxes, want = ${want === null ? -1 : +want};
    let best = null;
    for (let i = 0; i < b.length; i += BOX) {
      const flat = S.boxFlats[i / BOX], k = b[i + 3], w = b[i + 5] - b[i + 4];
      if (flat[k + 4] !== 1 || flat[k + 3] !== 1 || w < 12) continue;
      if (want >= 0 && b[i + 1] !== want) continue;
      const x = (b[i + 4] + b[i + 5]) / 2, y = b[i + 6] + b[i + 7] / 2;
      if (x < 600 || x > 1000 || y < RULER_H + 4 || y > canvas.clientHeight - 4) continue;
      const h = hitAt(x, y); // on top here (compact lanes overlay streams)
      if (!h || h.row !== b[i + 1] || h.index !== flat[k + 5]) continue;
      if (!best || Math.abs(x - 780) < Math.abs(best.x - 780)) best = { x, y, row: b[i + 1], name: S.traces[b[i]].names.get(flat[k + 2]) };
    }
    return best;
  })()`);
  const spot = await findSpot();
  check(!!spot, "a kernel to point at", spot ? `${spot.name} on row ${spot.row}` : "none");
  if (spot) {
    const x = canvas.x + spot.x, y = canvas.y + spot.y;
    await mouse("mouseMoved", x, y);
    await mouse("mousePressed", x, y, { button: "left", clickCount: 1, buttons: 1 });
    await mouse("mouseReleased", x, y, { button: "left", clickCount: 1 });
    await until(`S.selected !== null`, "clicking selects the kernel");
    await mouse("mouseMoved", x + 1, y);
    await until(`!document.getElementById("tooltip").hidden`, "the tooltip shows");
    const sel = await evaluate(`({ name: S.selected.name, linked: S.selected.linked && S.selected.linked.name, details: document.getElementById("panel-body").innerText.slice(0, 80) })`);
    check(sel.name === spot.name, "the details are of the kernel clicked", sel.name);
    check(!!sel.linked, "it links to the API call that launched it", sel.linked || "");
  }
  // Holding D pans continuously at the shared rate (THEME.nav), from the
  // frame loop rather than auto-repeat; letting go eases out and stops.
  await evaluate(`fit(false)`);
  await evaluate(`(() => { const [a, b] = extent(); zoomTo(a + (b - a) * 0.1, a + (b - a) * 0.2, false); })()`);
  await until(settled, "tiles before holding a key");
  await send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 10, y: 10 }); // the pointer off the timeline
  await evaluate(`document.getElementById("timeline").focus()`);
  const key = (type, k, code, vk) => send("Input.dispatchKeyEvent", { type, key: k, code, windowsVirtualKeyCode: vk, autoRepeat: false });
  const p0 = await evaluate(`({ start: S.view.start, scale: S.view.scale, t: performance.now() })`);
  await key("keyDown", "d", "KeyD", 68);
  await sleep(1000);
  const p1 = await evaluate(`({ start: S.view.start, scale: S.view.scale, t: performance.now() })`);
  await key("keyUp", "d", "KeyD", 68);
  await sleep(600);
  const p2 = await evaluate(`({ start: S.view.start, moving: navMoving() })`);
  const rate = (p1.start - p0.start) / p0.scale / ((p1.t - p0.t) / 1000);
  const steady = await evaluate(`THEME.nav.pan * dataW()`);
  check(rate > 0.7 * steady && rate < 1.05 * steady, "holding D pans continuously", `${rate.toFixed(0)} px/s, steady rate ${steady.toFixed(0)}`);
  check(!p2.moving && p2.start > p1.start, "letting go eases out and stops");
  // The navigation speed setting multiplies the rate, and is kept.
  await evaluate(`document.querySelector('#nav-speed [data-speed="2"]').click()`);
  await evaluate(`(() => { const [a, b] = extent(); zoomTo(a + (b - a) * 0.1, a + (b - a) * 0.2, false); })()`);
  await until(settled, "tiles before holding a key, faster");
  const f0 = await evaluate(`({ start: S.view.start, scale: S.view.scale, t: performance.now() })`);
  await key("keyDown", "d", "KeyD", 68);
  await sleep(500);
  const f1 = await evaluate(`({ start: S.view.start, t: performance.now() })`);
  await key("keyUp", "d", "KeyD", 68);
  await sleep(400);
  const fast = (f1.start - f0.start) / f0.scale / ((f1.t - f0.t) / 1000);
  check(fast > 1.4 * steady && fast < 2.1 * steady && (await evaluate(`localStorage.getItem("nsa-nav-speed")`)) === "2",
    "Fast navigation speed doubles the rate, and is kept", `${fast.toFixed(0)} px/s`);
  await evaluate(`document.querySelector('#nav-speed [data-speed="1"]').click()`);
  await key("keyDown", "w", "KeyW", 87);
  await sleep(300);
  await evaluate(`window.dispatchEvent(new Event("blur"))`);
  const b0 = await evaluate(`({ start: S.view.start, scale: S.view.scale, moving: navMoving() })`);
  await sleep(300);
  const b1 = await evaluate(`({ start: S.view.start, scale: S.view.scale })`);
  check(b0.scale < p0.scale && !b0.moving && b1.scale === b0.scale, "losing focus stops all motion");
  await key("keyUp", "w", "KeyW", 87);

  if (await evaluate(`!!(S.meta && S.meta.counts.Python)`)) await checkPython();
  await checkGoTo();
  await checkRange();
  if (meta.traces > 1) await checkSession();

  // Loading several traces: the loading screen lists each, its bar as the
  // server reports it (/api/progress), until the last is in. The traces'
  // rows are made to take a while here (their requests held), as large
  // traces' do.
  const sess = await evaluate(`({ inputs: S.inputs, n: S.traces.length })`);
  if (sess.n > 1) {
    await evaluate(`(() => { window.__realFetch = window.fetch; window.__progressAsks = 0;
      window.fetch = (u, o) => { const s = String(u);
        if (s.startsWith("api/progress")) window.__progressAsks++;
        return s.startsWith("api/trace?") ? new Promise((r) => setTimeout(r, 1200)).then(() => window.__realFetch(u, o)) : window.__realFetch(u, o); };
      openPaths(${JSON.stringify(sess.inputs)}, { newTab: true }); return 1; })()`);
    await until(`/Loading \\d+ traces/.test(document.querySelector("#loading .load-title")?.textContent || "")`, "the loading screen's line a trace");
    const seen = await evaluate(`({ rows: document.querySelectorAll("#loading .load-row").length, title: document.querySelector("#loading .load-title").textContent })`);
    check(seen.rows === Math.min(sess.n, 16), "loading several traces lists each, with how far they all are", `${seen.rows} lines: ${seen.title}`);
    await until(settled, "the session loads", 30000);
    const asks = await evaluate(`(() => { window.fetch = window.__realFetch; return window.__progressAsks; })()`);
    check(asks > 0 && await evaluate(`document.getElementById("loading").hidden`), "the server is asked how far its loads are, and the screen goes once they are in", `${asks} asks`);
    await evaluate(`closeTab(DOC)`);
    await until(settled, "back to the first tab");
  }


  // The appearance follows the system's, live, unless chosen.
  await scheme("light");
  const sysLight = await evaluate(`document.documentElement.dataset.theme`);
  await scheme("dark");
  const sysDark = await evaluate(`document.documentElement.dataset.theme`);
  check(sysLight === "light" && sysDark === "dark", "the page follows the system's appearance live", `${sysLight}, then ${sysDark}`);
  await evaluate(`document.querySelector('#theme-mode [data-mode="light"]').click()`);
  await scheme("dark");
  const chosen = await evaluate(`({ theme: document.documentElement.dataset.theme, kept: localStorage.getItem("nsa-theme") })`);
  check(chosen.theme === "light" && chosen.kept === "light", "a chosen appearance stays, and is kept", JSON.stringify(chosen));
  await evaluate(`document.querySelector('#theme-mode [data-mode="system"]').click()`);
  await scheme(appearance);
  await evaluate(`new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))`);

  // Back to the window to show, the kernel selected.
  if (hash.get("v")) {
    const [a, b] = hash.get("v").split(",").map(Number);
    await evaluate(`zoomTo(${a}, ${b}, false)`);
  }
  await until(settled, "tiles for the screenshot");
  // Found again: the checks since may have moved it (the overview's lanes,
  // the alignment).
  const spot2 = spot && await findSpot();
  if (spot2) await mouse("mouseMoved", canvas.x + spot2.x + 1, canvas.y + spot2.y);
  await until(`!document.getElementById("tooltip").hidden || ${!spot2}`, "the tooltip shows for the screenshot");

  // P shows the overlay.
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "p", text: "p", code: "KeyP", windowsVirtualKeyCode: 80 });
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "p", code: "KeyP", windowsVirtualKeyCode: 80 });
  check(await evaluate(`S.perf.show`), "P shows the overlay");
  await evaluate(`new Promise(r => setTimeout(r, 300))`);

  const shot = await send("Page.captureScreenshot", { format: "png" });
  writeFileSync(out, Buffer.from(shot.result.data, "base64"));
  // The path input: Ctrl+L and / focus it, errors say why, Enter opens an
  // absolute path, and the address and recents follow. (Its refused paths
  // log their 403, 404 and 400 in the page: expected.)
  const logsBefore = logs.length;
  const abs = await evaluate(`S.path`);
  check(abs.startsWith("/") && (await evaluate(`location.search`)).startsWith("?path=/"), "the address names the trace by its absolute path");
  check(/token=/.test(await evaluate(`location.hash`)) === /token=/.test(url), "the token stays in the fragment");
  await evaluate(`canvas.focus()`);
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "l", code: "KeyL", windowsVirtualKeyCode: 76, modifiers: 2 });
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "l", code: "KeyL", windowsVirtualKeyCode: 76, modifiers: 2 });
  check(await evaluate(`document.activeElement.id === "path-input"`), "Ctrl+L focuses the path input");
  await evaluate(`canvas.focus()`);
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "/", code: "Slash", text: "/", windowsVirtualKeyCode: 191 });
  check(await evaluate(`document.activeElement.id === "path-input"`), "/ focuses the path input");
  const tryPath = async (p) => {
    await evaluate(`(() => { const i = document.getElementById("path-input"); i.value = ${JSON.stringify(p)}; i.form.requestSubmit(); })()`);
    // Opened: its inputs (the trace in focus may have that path already,
    // a rank of a merged file).
    await until(`!document.getElementById("path-error").hidden || S.path === ${JSON.stringify(p)} && S.inputs.join(" ") === ${JSON.stringify(p)}`, "an answer to the path");
    return evaluate(`({ err: document.getElementById("path-error").hidden ? "" : document.getElementById("path-error").textContent, path: S.path })`);
  };
  const dir = abs.slice(0, abs.lastIndexOf("/"));
  let r = await tryPath("/etc/hostname");
  check(/outside/.test(r.err) && r.path === abs, "a path outside the root is refused, saying so", r.err);
  r = await tryPath(dir + "/no-such-trace.sqlite");
  check(/no such file/.test(r.err) && r.path === abs, "a missing path says so", r.err);
  r = await tryPath(dir + "/*.no-such-kind");
  check(/no trace files match/.test(r.err) && r.path === abs, "a glob matching nothing says so", r.err);
  r = await tryPath(abs);
  await until(settled, "the trace opens again from the path input");
  check(!r.err && r.path === abs, "Enter opens an absolute path");
  check((await evaluate(`recents()`))[0] === abs, "the path is remembered among the recent ones");
  const expected = logs.splice(logsBefore).filter((l) => !/Failed to load resource: .*status of (400|403|404)/.test(l));
  logs.push(...expected);

  if (!hash.get("tabs")) await checkTabs(abs);

  const perf = await evaluate(`({ draw: pct(S.perf.times, .5), drawP95: pct(S.perf.times, .95), tile: pct(S.perf.tileMs, .5), tileP95: pct(S.perf.tileMs, .95) })`);
  console.log(`draw p50 ${perf.draw.toFixed(2)} ms, p95 ${perf.drawP95.toFixed(2)} ms; tile round trip p50 ${perf.tile.toFixed(0)} ms, p95 ${perf.tileP95.toFixed(0)} ms`);
  check(logs.length === 0, "no errors in the page", logs.join("; "));
  console.log(`wrote ${out}`);
  ws.close();
}

main().catch((e) => { console.error(e); failures++; }).finally(() => {
  chrome.kill();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch {} process.exit(failures ? 1 : 0); }, 300);
});
