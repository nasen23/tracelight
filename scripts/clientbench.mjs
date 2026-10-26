// clientbench compares the web clients on one server, in headless Chrome:
// the JavaScript one (tracelight serve --ui js) and the desktop app's views
// in WebAssembly (--ui wasm, the default). For a session (or a trace) it
// times: the start (navigation to the first events drawn, and the bytes
// downloaded); the refill after each view of overviewbench's script (4×
// zoom steps, half-view pans, jumps), until every tile slot drawn is of
// the view's level; and 2 s of held-key panning (D): frames a second and
// the time of each frame's work. Each frame's work is measured alike in
// both: the requestAnimationFrame callbacks of a frame, summed. Aggregates
// only.
//
//   tracelight serve --root DIR --latency 30ms-80ms --ui js   --addr :8761 &
//   tracelight serve --root DIR --latency 30ms-80ms --ui wasm --addr :8762 &
//   node --experimental-websocket scripts/clientbench.mjs "http://127.0.0.1:8761/?path=/abs/dir" [overview]
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [url, mode = ""] = process.argv.slice(2);
if (!url) { console.error("usage: clientbench.mjs URL [overview]"); process.exit(2); }
const W = 1600, H = 1000, PORT = 9300 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(tmpdir(), "clientbench-"));
const chrome = spawn(process.env.CHROME || "google-chrome", [
  "--headless=new", "--no-sandbox", "--hide-scrollbars", "--no-first-run", ...(process.env.NOGL ? ["--disable-3d-apis"] : ["--enable-unsafe-swiftshader"]),
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, `--window-size=${W},${H}`, "about:blank",
], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const q = (v, p) => { const s = [...v].sort((x, y) => x - y); return s.length ? s[Math.min(s.length - 1, Math.floor(p * s.length))] : NaN; };

// The page's frames: each requestAnimationFrame callback timed, summed by
// frame (callbacks of a frame share its timestamp).
const FRAMES = `(() => {
  const raf = window.requestAnimationFrame.bind(window);
  window.__frames = new Map();
  window.requestAnimationFrame = (cb) => raf((t) => {
    const t0 = performance.now();
    try { cb(t); } finally { window.__frames.set(t, (window.__frames.get(t) || 0) + performance.now() - t0); }
  });
})();`;

async function main() {
  let target;
  for (let i = 0; i < 100 && !target; i++) {
    await sleep(100);
    try { target = (await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()).find((t) => t.type === "page"); } catch {}
  }
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let id = 0, bytes = 0;
  const pending = new Map();
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
    if (msg.method === "Network.loadingFinished") bytes += msg.params.encodedDataLength;
  };
  const send = (method, params = {}) => new Promise((r) => { const n = ++id; pending.set(n, r); ws.send(JSON.stringify({ id: n, method, params })); });
  const evaluate = async (expr) => {
    const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result.exceptionDetails) throw new Error(expr.slice(0, 80) + ": " + (r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text));
    return r.result.result.value;
  };
  const until = async (expr, what, ms = 120000) => {
    for (let t = 0; t < ms; t += 50) { try { if (await evaluate(expr)) return true; } catch {} await sleep(50); }
    throw new Error("timed out: " + what);
  };
  await send("Runtime.enable"); await send("Page.enable"); await send("Network.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: W, height: H, deviceScaleFactor: 1, mobile: false });
  await send("Page.addScriptToEvaluateOnNewDocument", { source: FRAMES });
  const t0 = Date.now();
  await send("Page.navigate", { url });
  await until(`typeof S !== "undefined" || typeof tracelight !== "undefined"`, "the client");
  const wasm = await evaluate(`typeof tracelight !== "undefined"`);
  // The client's state, alike: loaded, the view's span, the last frame's
  // tile slots (exact, coarse, blank).
  const ST = wasm
    ? `(() => { const s = JSON.parse(tracelight.state()); return { ready: s.shown && !s.loading && s.extent[1] > 0, extent: s.extent, slots: s.slots }; })()`
    : `(() => ({ ready: !!S.meta && S.traces.every(t => t.folded || t.meta || t.error), extent: S.meta ? extent() : [0, 0], slots: [S.perf.slots.exact, S.perf.slots.coarse, S.perf.slots.blank] }))()`;
  await until(`(() => { const s = ${ST}; return s.ready && s.slots[0] > 0; })()`, "the first events");
  const start = Date.now() - t0;
  const startBytes = bytes;
  if (wasm) await evaluate(`tracelight.animations(false)`);
  if (mode === "overview") {
    await evaluate(wasm ? `tracelight.compact("on")` : `setCompact("on")`);
    await sleep(500);
  }
  const zoom = (a, b) => wasm ? `tracelight.zoomTo(${a}, ${b})` : `(zoomTo(${a}, ${b}, false), invalidate())`;
  const { extent: [a0, b0] } = await evaluate(ST);
  const views = [];
  const mid = (a0 + b0) / 2;
  for (let span = b0 - a0, k = 0; span > (b0 - a0) / 300; span /= 4, k++) {
    views.push([mid - span / 2, mid + span / 2], [mid, mid + span], [mid - span, mid]);
    const far = a0 + (b0 - a0) * (0.15 + 0.1 * k);
    views.push([far, far + span], [far + 3 * span, far + 4 * span]);
  }
  const p = a0 + (b0 - a0) * 0.8;
  for (let span = (b0 - a0) / 256; span < (b0 - a0); span *= 4) views.push([p - span / 2, p + span / 2]);
  // Settle, then each view: until every slot is exact.
  await until(`(() => { const s = ${ST}; return s.slots[0] > 0 && !s.slots[1] && !s.slots[2]; })()`, "settled", 60000);
  await evaluate(`window.__frames.clear()`);
  const full = [];
  let timeouts = 0;
  for (const [a, b] of views) {
    const r = await evaluate(`new Promise((res) => {
      ${zoom(a, b)};
      const t0 = performance.now();
      let frames = 0;
      const tick = () => requestAnimationFrame(() => {
        const s = ${ST}, now = performance.now() - t0;
        if (frames++ > 1 && s.slots[0] && !s.slots[1] && !s.slots[2]) return res(now);
        if (now > 10000) return res(null);
        tick();
      });
      tick();
    })`);
    if (r === null) timeouts++; else full.push(r);
  }
  const refillFrames = await evaluate(`[...window.__frames.values()]`);
  // Held D: panning, every frame.
  await evaluate(zoom(a0 + (b0 - a0) * 0.3, a0 + (b0 - a0) * 0.35));
  await sleep(1500);
  await send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 1000, y: 100 });
  await send("Input.dispatchMouseEvent", { type: "mousePressed", x: 1000, y: 100, button: "left", clickCount: 1, buttons: 1 });
  await send("Input.dispatchMouseEvent", { type: "mouseReleased", x: 1000, y: 100, button: "left", clickCount: 1 });
  if (!wasm) await evaluate(`canvas.focus()`);
  await evaluate(`window.__frames.clear()`);
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "d", code: "KeyD", text: "d", windowsVirtualKeyCode: 68 });
  await sleep(2000);
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "d", code: "KeyD", windowsVirtualKeyCode: 68 });
  const pan = await evaluate(`(() => { const t = [...window.__frames.keys()].sort((x, y) => x - y), w = [...window.__frames.values()];
    return { n: t.length, span: t.length > 1 ? t[t.length - 1] - t[0] : 0, gaps: t.slice(1).map((x, i) => x - t[i]), work: w }; })()`);
  console.log(`${wasm ? "wasm" : "js  "} ${mode || "default"}: start to first events ${start} ms, ${(startBytes / 1e6).toFixed(2)} MB downloaded`);
  console.log(`  ${views.length} views: full refill p50 ${q(full, 0.5).toFixed(0)} ms, p95 ${q(full, 0.95).toFixed(0)} ms, ${timeouts} not full after 10 s; frame work p50 ${q(refillFrames, 0.5).toFixed(1)} ms, p95 ${q(refillFrames, 0.95).toFixed(1)} ms (${refillFrames.length} frames)`);
  console.log(`  held D 2 s: ${(1000 * (pan.n - 1) / pan.span).toFixed(1)} frames/s, frame interval p95 ${q(pan.gaps, 0.95).toFixed(1)} ms, frame work p50 ${q(pan.work, 0.5).toFixed(1)} ms, p95 ${q(pan.work, 0.95).toFixed(1)} ms`);
  ws.close();
}
main().catch((e) => { console.error(e); process.exitCode = 1; }).finally(() => {
  chrome.kill();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch {} process.exit(process.exitCode || 0); }, 300);
});
