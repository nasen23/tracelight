// Starts Tracelight's web client in WebAssembly (the desktop app's views,
// drawn by MyGo on a canvas): the app is fetched relative to this page, so
// that the server works under any prefix (a reverse proxy, a sign-in
// gateway). Where the browser draws without GPU acceleration, a note says
// that the classic page is faster there.
"use strict";
(async () => {
  const text = document.getElementById("boot-text");
  try {
    const go = new Go();
    const res = fetch("app.wasm", { credentials: "same-origin" });
    const { instance } = await WebAssembly.instantiateStreaming(res, go.importObject);
    go.run(instance);
    const shown = () => document.querySelector("#mygo canvas") ? (document.getElementById("boot").hidden = true) : requestAnimationFrame(shown);
    shown();
    if (software()) note();
  } catch (e) {
    text.textContent = "Tracelight could not start here (" + e.message + "). ";
    const a = classicLink("Open the classic page");
    text.append(a);
  }
})();

// software reports whether WebGL 2 is missing or drawn on the CPU.
function software() {
  try {
    const gl = document.createElement("canvas").getContext("webgl2");
    if (!gl) return true;
    const ext = gl.getExtension("WEBGL_debug_renderer_info");
    const r = ext ? String(gl.getParameter(ext.UNMASKED_RENDERER_WEBGL)) : "";
    return /swiftshader|llvmpipe|softpipe|software|basic render/i.test(r);
  } catch { return true; }
}

function classicLink(label) {
  const a = document.createElement("a");
  a.textContent = label;
  a.href = "classic.html" + location.search + location.hash;
  a.addEventListener("click", () => { a.href = "classic.html" + location.search + location.hash; });
  return a;
}

// note says, for a while, that this browser draws without the GPU.
function note() {
  const n = document.createElement("div");
  n.id = "boot-note";
  n.append("This browser draws without GPU acceleration: the classic page is faster here. ", classicLink("Switch"));
  const x = document.createElement("button");
  x.textContent = "×";
  x.title = "Dismiss";
  x.addEventListener("click", () => n.remove());
  n.append(x);
  document.body.append(n);
  setTimeout(() => n.remove(), 15000);
}
