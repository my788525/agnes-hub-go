// 冒烟测试：用假 canvas 上下文运行 console.html 里的图表绘制函数，确保无运行时异常。
const fs = require("fs");
const path = require("path");
const vm = require("vm");

const html = fs.readFileSync(
  path.join(__dirname, "..", "internal", "web", "static", "console.html"),
  "utf8"
);
// 注意：CSS 里也有同样前缀的注释，这里必须精确定位 JS 块
const start = html.indexOf("/* ---------------- 实时动态图表（运行实况 · 总览第二屏）");
const end = html.indexOf("/* ---------------- 数据拉取");
if (start < 0 || end < 0 || end <= start) {
  console.error("SMOKE_FAIL: 找不到图表代码块");
  process.exit(1);
}
// const 在 vm 里不会挂到全局对象上，改成 globalThis. 以便在外部注入数据后调用
const code = html
  .slice(start, end)
  .replace("const LIVE = {", "globalThis.LIVE = {");

// ---- 假 canvas 2D 上下文 ----
const calls = { fill: 0, stroke: 0, fillRect: 0, fillText: 0 };
function makeCtx() {
  return new Proxy(
    {
      setTransform() {}, clearRect() {}, beginPath() {}, moveTo() {}, lineTo() {},
      closePath() {}, stroke() { calls.stroke++; }, fill() { calls.fill++; },
      fillRect() { calls.fillRect++; }, fillText() { calls.fillText++; },
      strokeText() { calls.strokeText = (calls.strokeText || 0) + 1; },
      setLineDash() {}, measureText: () => ({ width: 10 }),
    },
    { get: (t, k) => (k in t ? t[k] : undefined), set: () => true }
  );
}
function makeCanvas(id) {
  return {
    id, clientWidth: 800, clientHeight: 150, width: 0, height: 0,
    getContext: () => makeCtx(),
  };
}
const canvases = { cvLoad: makeCanvas("cvLoad"), cvScale: makeCanvas("cvScale"), cvAcc: makeCanvas("cvAcc") };
const readouts = {};

const sandbox = {
  console,
  Math, JSON, Array, Object, String, Number, Date,
  getComputedStyle: () => ({ color: "#333333" }),
  document: {
    visibilityState: "visible",
    getElementById: (id) =>
      canvases[id] ||
      (readouts[id] = readouts[id] || { id, innerHTML: "" }),
    querySelectorAll: () => [],
    addEventListener() {},
  },
  window: { devicePixelRatio: 2, addEventListener() {} },
  esc: (s) => String(s == null ? "" : s),
  num: (n) => String(n == null ? 0 : n),
  state: { tab: "overview" },
  api: async () => ({}),
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

// ---- 构造 60 个采样点（模拟真实 history 返回）----
const meta = [
  { id: "a1", name: "账号A", color: "#2f6fd6" },
  { id: "a2", name: "账号B", color: "#2f9e6f" },
];
const points = [];
for (let i = 0; i < 60; i++) {
  points.push({
    ts: 1700000000 + i,
    load_pct: i < 30 ? 40 + i : 120 - (i - 30),
    arrive_rps: (i % 7) / 2,
    done_rps: (i % 5) / 2,
    queue_depth: i % 9,
    inflight: i % 6,
    r429_pm: 0,
    events: i === 20 ? 1 : i === 40 ? 2 : 0,
    acc: [
      { id: "a1", inflight: i % 3, done_delta: i % 4 },
      { id: "a2", inflight: i % 2, done_delta: (i + 1) % 3 },
    ],
  });
}
sandbox.LIVE.hist = { points, accounts: meta };
sandbox.LIVE.meta = meta;

// 三种窗口各画一遍
for (const win of ["30s", "60s", "5m", "1h"]) {
  sandbox.LIVE.win = win;
  sandbox.drawLive();
}
// 空数据也要能画（刚启动、还没采到点）
sandbox.LIVE.hist = { points: [], accounts: meta };
sandbox.drawLive();
// 单账号 + 无流量
sandbox.LIVE.hist = {
  points: [{ ts: 1, load_pct: 0, arrive_rps: 0, done_rps: 0, queue_depth: 0, inflight: 0, r429_pm: 0, events: 0, acc: [{ id: "a1", inflight: 0, done_delta: 0 }] }],
  accounts: [{ id: "a1", name: "账号A", color: "#2f6fd6" }],
};
sandbox.drawLive();

console.log("draw calls:", JSON.stringify(calls));
console.log("readout load:", readouts.roLoad ? readouts.roLoad.innerHTML.slice(0, 60) : "(none)");
console.log("legend:", readouts.lgAcc ? readouts.lgAcc.innerHTML.slice(0, 80) : "(none)");
if (calls.fill === 0 || calls.stroke === 0) {
  console.error("SMOKE_FAIL: 没有实际绘制发生");
  process.exit(1);
}
console.log("SMOKE_OK");
