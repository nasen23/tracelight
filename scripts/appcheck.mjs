// appcheck drives the WebAssembly web client (app.html) in headless Chrome
// through window.tracelight (cmd/webui/bridge.go): a ?path=…#v=… link opens
// its traces at its view, and the address follows the tab shown; tabs keep
// their views and come back after a reload; and behind a sign-in gateway
// (a test one here: a path prefix, a cookie session, a sign-in page on
// another origin) the app works, and a session that ends sends the page
// through the sign-in again; a touchpad pinch (Chrome's and Safari's) and a
// Ctrl + wheel zoom around the pointer, never drifting past the widest
// view. Writes a screenshot.
//
//   tracelight serve --root DIR &
//   node --experimental-websocket scripts/appcheck.mjs http://127.0.0.1:8737/ /abs/dir /abs/trace.sqlite out.png
//
// No packages needed: Node 20+ and Chrome (CHROME=/path/to/chrome).
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [server, dir, file, out = "appcheck.png", view = ""] = process.argv.slice(2);
if (!file) { console.error("usage: appcheck.mjs SERVER_URL DIR TRACE [out.png] [a,b]"); process.exit(2); }
const W = 1600, H = 1000, PORT = 9300 + Math.floor(Math.random() * 500);
const profile = mkdtempSync(join(tmpdir(), "appcheck-"));
const chrome = spawn(process.env.CHROME || "google-chrome", [
  "--headless=new", "--no-sandbox", "--hide-scrollbars", "--no-first-run", "--disable-3d-apis",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, `--window-size=${W},${H}`, "about:blank",
], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
const check = (ok, what, detail = "") => { console.log(`${ok ? "ok  " : "FAIL"} ${what}${detail ? ": " + detail : ""}`); if (!ok) failures++; };

// The gateway: /sso/… forwards to the server (the prefix stripped) for a
// browser with the session cookie; without it, a redirect to the sign-in
// page on another port (another origin, as an SSO host), which sets the
// cookie and sends the browser back.
const upstream = new URL(server);
let signIns = 0;
const login = http.createServer((req, res) => {
  const back = new URL(req.url, "http://x").searchParams.get("back");
  signIns++;
  res.writeHead(302, { "Set-Cookie": "session=ok; Path=/; SameSite=Lax", Location: back });
  res.end();
});
const gateway = http.createServer((req, res) => {
  if (!req.url.startsWith("/sso/")) { res.writeHead(404); res.end(); return; }
  if (!/(^|;\s*)session=ok/.test(req.headers.cookie || "")) {
    res.writeHead(302, { Location: `http://127.0.0.1:${login.address().port}/login?back=${encodeURIComponent(`http://127.0.0.1:${gateway.address().port}${req.url}`)}` });
    res.end();
    return;
  }
  const p = http.request({ host: upstream.hostname, port: upstream.port, path: req.url.slice(4), method: req.method, headers: { ...req.headers, host: upstream.host } }, (r) => {
    res.writeHead(r.statusCode, r.headers);
    r.pipe(res);
  });
  req.pipe(p);
});
await new Promise((r) => login.listen(0, "127.0.0.1", r));
await new Promise((r) => gateway.listen(0, "127.0.0.1", r));

async function main() {
  let target;
  for (let i = 0; i < 100 && !target; i++) {
    await sleep(100);
    try { target = (await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()).find((t) => t.type === "page"); } catch {}
  }
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let id = 0;
  const pending = new Map(), logs = [];
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
    if (msg.method === "Runtime.exceptionThrown") logs.push("exception: " + msg.params.exceptionDetails.text);
    if (msg.method === "Runtime.consoleAPICalled" && /panic|fatal/i.test(msg.params.args.map((a) => a.value ?? "").join(" "))) logs.push("console: " + msg.params.args.map((a) => a.value ?? a.description).join(" ").slice(0, 200));
  };
  const send = (method, params = {}) => new Promise((r) => { const n = ++id; pending.set(n, r); ws.send(JSON.stringify({ id: n, method, params })); });
  const evaluate = async (expr) => {
    const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result.exceptionDetails) throw new Error(expr.slice(0, 80) + ": " + (r.result.exceptionDetails.exception?.description || r.result.exceptionDetails.text));
    return r.result.result.value;
  };
  const state = () => evaluate(`JSON.parse(tracelight.state())`);
  const until = async (fn, what, ms = 30000) => {
    for (let t = 0; t < ms; t += 100) { try { if (await fn()) return true; } catch {} await sleep(100); }
    check(false, what, "timed out");
    return false;
  };
  const settled = async () => { const s = await state(); return s.shown && !s.loading && s.slots[0] > 0 && !s.slots[1] && !s.slots[2]; };
  await send("Runtime.enable"); await send("Page.enable"); await send("Network.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: W, height: H, deviceScaleFactor: 1, mobile: false });
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: process.env.APPEARANCE || "dark" }] });

  // A link: its traces, at its view; the address follows.
  const enc = (p) => encodeURIComponent(p).replace(/%2F/g, "/");
  const [a, b] = view ? view.split(",").map(Number) : [400e6, 440e6];
  await send("Page.navigate", { url: `${server.replace(/\/$/, "")}/app.html?path=${enc(dir)}#v=${a},${b}` });
  await until(settled, "the linked traces open and draw");
  let s = await state();
  const span = b - a;
  check(Math.abs(s.view[0] - a) < span * 0.001 && Math.abs(s.view[1] - b) < span * 0.001, "the link's view shows, exactly", s.view.map((x) => (x / 1e6).toFixed(1)).join("–") + " ms");
  check(s.tabs[0] === 1 && s.titles[0] === dir.split("/").pop(), "one tab, named after the folder", s.titles.join(" | "));
  await sleep(600);
  let loc = await evaluate(`location.pathname + location.search + location.hash`);
  check(loc.includes(`?path=${enc(dir)}`) && /#v=\d+(,|%2C)\d+/.test(loc), "the address names the traces and the view", loc.replace(dir, "DIR"));
  // Zooming, as a touchpad and a mouse do, around the pointer, at a point
  // of the timeline 70% of the way across.
  {
    const [dx, dy, dw] = s.data, px = dx + dw * 0.7, py = dy + 120;
    const timeAt = (st) => st.view[0] + (px - dx) / dw * (st.view[1] - st.view[0]);
    const span0 = s.view[1] - s.view[0], t0 = timeAt(s);
    // A pinch to 3×, as Chrome sends it: wheels with ctrlKey, deltaY
    // adding up to -100 ln 3; MyGo's web surface gives it as InputZoom.
    await evaluate(`(() => { const c = document.querySelector("canvas");
      for (let i = 0; i < 12; i++) c.dispatchEvent(new WheelEvent("wheel", { deltaY: -100 * Math.log(3) / 12, deltaMode: 0, ctrlKey: true,
        clientX: ${px}, clientY: ${py}, bubbles: true, cancelable: true })); })()`);
    await sleep(300);
    let z = await state();
    check(Math.abs(span0 / (z.view[1] - z.view[0]) - 3) < 0.02, "a touchpad pinch to 3× zooms in 3×", `${(span0 / 1e6).toFixed(2)} → ${((z.view[1] - z.view[0]) / 1e6).toFixed(2)} ms`);
    check(Math.abs(timeAt(z) - t0) < (z.view[1] - z.view[0]) / dw * 2, "and keeps the time under the pointer", `${(t0 / 1e6).toFixed(3)} vs ${(timeAt(z) / 1e6).toFixed(3)} ms`);
    // Safari's: gesture events, their scale from 1 as the pinch began.
    await evaluate(`(() => { const c = document.querySelector("canvas");
      const fire = (type, scale) => { const e = new Event(type, { bubbles: true, cancelable: true });
        Object.assign(e, { scale, clientX: ${px}, clientY: ${py}, shiftKey: false, altKey: false, ctrlKey: false, metaKey: false }); c.dispatchEvent(e); };
      fire("gesturestart", 1); fire("gesturechange", 0.5); fire("gestureend", 0.5); })()`);
    await sleep(300);
    const z2 = await state();
    check(Math.abs((z2.view[1] - z2.view[0]) / (z.view[1] - z.view[0]) - 2) < 0.02, "Safari's pinch-out gesture zooms out 2×");
    // A mouse wheel with Ctrl, far past the widest view: the trace stays in it.
    for (let i = 0; i < 40; i++) await send("Input.dispatchMouseEvent", { type: "mouseWheel", x: px, y: py, deltaX: 0, deltaY: 120, modifiers: 2 });
    await sleep(1200);
    z = await state();
    check(z.view[0] <= z.extent[0] && z.view[1] >= z.extent[1], "zooming out past the widest view keeps the trace in view",
      `view ${(z.view[0] / 1e6).toFixed(1)}–${(z.view[1] / 1e6).toFixed(1)}, trace ${(z.extent[0] / 1e6).toFixed(1)}–${(z.extent[1] / 1e6).toFixed(1)} ms`);
    await evaluate(`tracelight.zoomTo(${a}, ${b})`);
    await sleep(300);
    await until(settled, "the link's view again");
    s = await state();
  }
  // Tabs: a trace in a new tab; back and forth, each keeps its view.
  const viewA = s.view;
  await evaluate(`tracelight.open(${JSON.stringify(file)}, true)`);
  await until(async () => { const t = await state(); return t.tabs[0] === 2 && t.tabs[1] === 1 && await settled(); }, "the trace opens in a new tab");
  // A window inside the trace, wherever its events are.
  const [x0, x1] = (await state()).extent;
  await evaluate(`tracelight.zoomTo(${x0 + (x1 - x0) * 0.3}, ${x0 + (x1 - x0) * 0.31})`);
  await sleep(300);
  const viewB = (await state()).view;
  await sleep(600);
  loc = await evaluate(`location.search`);
  check(loc.includes(enc(file)), "the address follows the tab shown", loc.replace(file, "TRACE"));
  await evaluate(`tracelight.showTab(0)`);
  await sleep(300);
  s = await state();
  check(s.tabs[1] === 0 && Math.abs(s.view[0] - viewA[0]) < 1 && Math.abs(s.view[1] - viewA[1]) < 1, "the first tab keeps its view");
  await evaluate(`tracelight.showTab(1)`);
  await sleep(300);
  s = await state();
  check(s.tabs[1] === 1 && Math.abs(s.view[0] - viewB[0]) < 1, "and the second its own");
  // A reload: the tabs come back, the other one loading when shown.
  await sleep(600);
  await send("Page.reload");
  await until(async () => (await state()).tabs[0] === 2 && await settled(), "the tabs come back after a reload");
  s = await state();
  check(s.tabs[0] === 2 && s.tabs[1] === 1 && Math.abs(s.view[0] - viewB[0]) / span < 0.05, "a reload brings back the tabs, and the shown one's view", JSON.stringify(s.titles));
  await evaluate(`tracelight.showTab(0)`);
  await until(settled, "the restored tab opens when shown");
  s = await state();
  check(Math.abs(s.view[0] - viewA[0]) < span * 0.05, "at its view");

  // A session's traces left out (the sidebar's boxes; here the bridge's
  // soloTrace and showTrace): the address says which (#only=, #omit=),
  // and a link with it opens so.
  if (s.traces[1] > 2) {
    const n = s.traces[1];
    await evaluate(`tracelight.soloTrace(1)`);
    await sleep(700);
    s = await state();
    loc = await evaluate(`location.hash`);
    check(s.traces[0] === 1 && /only=/.test(loc), "one trace alone, and the address says so", `${s.traces[0]} of ${n}`);
    const alone = await evaluate(`location.href`);
    await evaluate(`sessionStorage.clear()`);
    await send("Page.navigate", { url: "about:blank" });
    await sleep(200);
    await send("Page.navigate", { url: alone });
    // (Loaded, not drawn: the trace alone may have nothing in the view.)
    await until(async () => { const t = await state(); return t.traces[1] === n && t.shown && !t.loading && t.complete; }, "the link with only= opens");
    s = await state();
    check(s.traces[0] === 1, "a link with only= opens that trace alone", `${s.traces[0]} of ${s.traces[1]}`);
    await evaluate(`tracelight.showTrace(0, true)`);
    await sleep(700);
    s = await state();
    loc = await evaluate(`location.hash`);
    check(s.traces[0] === 2 && /(only|omit)=/.test(loc), "another shown again, the address following", `${s.traces[0]} of ${n}`);
  }

  // Behind the gateway, under a prefix: signed in, it works; the session
  // ending sends the page through the sign-in, and it comes back.
  await send("Network.clearBrowserCookies");
  await evaluate(`sessionStorage.clear()`);
  const gw = `http://127.0.0.1:${gateway.address().port}/sso/`;
  await send("Page.navigate", { url: `${gw}app.html?path=${enc(file)}` });
  await until(settled, "the app opens through the gateway");
  check(signIns === 1, "the gateway's sign-in, once", `${signIns}`);
  loc = await evaluate(`location.pathname`);
  check(loc === "/sso/app.html", "the page stays under the prefix", loc);
  await send("Network.clearBrowserCookies");
  const [g0, g1] = (await state()).extent;
  await evaluate(`tracelight.zoomTo(${g0 + (g1 - g0) * 0.4}, ${g0 + (g1 - g0) * 0.402})`);
  await until(async () => signIns === 2, "an ended session goes through the sign-in again", 20000);
  await until(settled, "and the app comes back, drawn", 30000);
  check(signIns === 2 && (await evaluate(`location.pathname`)) === "/sso/app.html", "the app comes back after signing in again");

  // The screenshot: the link again, with more tabs (EXTRA=path|path).
  await evaluate(`sessionStorage.clear()`);
  await send("Page.navigate", { url: `${server.replace(/\/$/, "")}/app.html?path=${enc(dir)}#v=${a},${b}` });
  await until(settled, "the screenshot's view");
  const extra = (process.env.EXTRA || "").split("|").filter(Boolean);
  for (const [i, p] of extra.entries()) {
    await evaluate(`tracelight.open(${JSON.stringify(p)}, true)`);
    await until(async () => (await state()).tabs[0] === i + 2 && await settled(), "a tab of " + p.split("/").pop());
    await evaluate(`tracelight.zoomTo(${a}, ${b})`);
  }
  if (extra.length) { await evaluate(`tracelight.showTab(0)`); await sleep(300); await until(settled, "the first tab again"); }
  await evaluate(`document.getElementById("boot-note")?.remove()`); // headless Chrome draws without a GPU
  await sleep(500);
  const shot = await send("Page.captureScreenshot", { format: "png" });
  writeFileSync(out, Buffer.from(shot.result.data, "base64"));
  check(logs.length === 0, "no errors in the page", logs.join("; "));
  console.log(`wrote ${out}`);
  ws.close();
}
main().catch((e) => { console.error(e); failures++; }).finally(() => {
  chrome.kill(); login.close(); gateway.close();
  setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch {} process.exit(failures ? 1 : 0); }, 300);
});
