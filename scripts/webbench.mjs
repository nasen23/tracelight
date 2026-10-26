// webbench times the web UI's frames in headless Chrome: for the rank overview
// and the full view, through 4× zoom steps from the whole session down to 2 µs
// around its middle and back, with half-view pans, it waits for each view's
// tiles and times 20 frames, printing p50, p95 and max. A frame is draw()
// and the canvas's rasterization, forced by reading a pixel back: without
// it, Chrome defers the drawing of frames drawn back to back and pays it
// all at once every twenty or so. Aggregates only.
//
//   tracelight serve --root samples --addr 127.0.0.1:8737 &
//   node --experimental-websocket scripts/webbench.mjs "http://127.0.0.1:8737/?path=/abs/samples/ranks16"
//
// No packages needed: Node 20+ and Chrome (CHROME=/path/to/chrome).
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [url] = process.argv.slice(2);
if (!url) { console.error("usage: webbench.mjs URL"); process.exit(2); }
const W = 1600, H = 1000, PORT = 9300 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(tmpdir(), "webcheck-"));
const chrome = spawn(process.env.CHROME || "google-chrome", [
  "--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, `--window-size=${W},${H}`, "about:blank",
], { stdio: "ignore" });
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
    await evaluate(`setLane("api", false)`);
    const filtered = await evaluate(`S.rows.filter(r => r.kind === "summary").length`);
    check(filtered < st.lanes && JSON.parse(await evaluate(`localStorage.getItem("nsa-compact-lanes")`)).join() === "gpu,nvtx", "the lane filter applies to every trace", `${st.lanes} → ${filtered} lanes`);
    await evaluate(`setLane("api", true)`);
    await evaluate(`document.getElementById("timeline").focus()`);
    await send("Input.dispatchKeyEvent", { type: "keyDown", key: "c", code: "KeyC", text: "c", windowsVirtualKeyCode: 67 });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: "c", code: "KeyC", windowsVirtualKeyCode: 67 });
    const off = await evaluate(`({ on: compactOn(), kept: localStorage.getItem("nsa-compact"), heads: S.rows.filter(r => r.kind === "trace").length })`);
    check(!off.on && off.kept === "off" && off.heads === st.n, "C switches to the full view, and is kept", JSON.stringify(off));
    await until(settled, "the full view settles");
  };

  // A session of several traces: a heading for each, on one time axis.
  const checkSession = async () => {
    if (await evaluate(`compactOn()`)) await checkCompact();
    const st = await evaluate(`({ n: S.traces.length, heads: S.rows.filter(r => r.kind === "trace").length,
      offs: S.traces.map(offsetOf), clocks: S.traces.filter(t => t.sessionStart > 0).length,
      want: S.traces.map(t => t.sessionStart > 0 && t.meta ? t.sessionStart - S.origin + t.meta.start : 0) })`);
    check(st.heads === st.n, "a heading per trace", `${st.heads} of ${st.n}`);
    const spread = Math.max(...st.offs) - Math.min(...st.offs);
    check(st.offs.every((o, i) => o === st.want[i]), "capture-time alignment places each trace at its capture's clock", `${st.clocks} clocks, spread ${(spread / 1e6).toFixed(1)} ms`);
    await evaluate(`document.querySelector('#align-mode [data-align="start"]').click()`);
    const s2 = await evaluate(`({ offs: S.traces.map(offsetOf), on: document.querySelector('#align-mode .on').dataset.align })`);
    check(s2.offs.every((o) => o === 0) && s2.on === "start", "Starts aligns every trace at 0");
    await sleep(400);
    check(/align=start/.test(await evaluate(`location.hash`)), "the address keeps the alignment");
    await evaluate(`document.querySelector('#align-mode [data-align="abs"]').click()`);
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
    await until(settled, "the session settles");
  };

  // A Shift+drag range: the summaries are of it; Esc, its ×, and a click on
  // an empty part of the timeline clear it; Enter zooms to it.
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
      const ri = rowAt(yy); if (ri >= 0 && S.rows[ri].kind === "track" && !hitAt(xx, yy)) return { x: xx, y: yy }; } return null; })()`);
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

  await send("Runtime.enable"); await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: W, height: H, deviceScaleFactor: 1, mobile: false });
  await send("Page.navigate", { url });
  await until(`S.meta && S.traces.every(t => t.meta || t.error)`, "every trace loads", 60000);
  await until(settled, "the first tiles", 30000);
  const info = await evaluate(`({ n: S.traces.length, events: loadedTraces().reduce((a, t) => a + t.meta.events, 0) })`);
  console.log(`${info.n} traces, ${info.events} events, ${W}x${H}`);
  for (const mode of ["on", "off"]) {
    await evaluate(`setCompact(${JSON.stringify(mode)})`);
    const views = await evaluate(`(() => { const [a, b] = extent(), mid = (a + b) / 2, out = [];
      for (let span = b - a; span > 2000; span /= 4) out.push([mid - span / 2, mid + span / 2], [mid, mid + span], [mid - span / 2, mid + span / 2]);
      return out; })()`);
    const times = [];
    for (const [a, b] of views) {
      await evaluate(`zoomTo(${a}, ${b}, false)`);
      await until(settled, "tiles of the view", 30000);
      const t = await evaluate(`(() => { const out = []; for (let i = 0; i < 20; i++) { const t0 = performance.now(); draw(); ctx.getImageData(0, 0, 1, 1); out.push(performance.now() - t0); } return out; })()`);
      times.push(...t);
    }
    times.sort((x, y) => x - y);
    const q = (p) => times[Math.min(times.length - 1, Math.floor(p * times.length))];
    const rows = await evaluate(`S.rows.length`);
    console.log(`${mode === "on" ? "overview" : "full    "}: ${rows} rows, ${times.length} frames over ${views.length} views: draw+raster p50 ${q(0.5).toFixed(2)} ms, p95 ${q(0.95).toFixed(2)} ms, max ${times[times.length - 1].toFixed(2)} ms`);
  }
  await evaluate(`setCompact("auto")`);
  ws.close();
}

main().catch((e) => { console.error(e); failures++; }).finally(() => {
  chrome.kill();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch {} process.exit(failures ? 1 : 0); }, 300);
});
