// 走阿里云无痕 SDK 拿 captchaVerifyParam，供 tools/probe_claim.py 的 claim 使用。
//
// 链路（对齐 12 文档的实包结论）：
//   InitCaptcha(SceneId=11xygtvd, prefix=no8xfe, region=cn)
//     -> SDK 自行 PoW + 提交
//     -> success 回调第一个参数即 param
//
// DEBUG=1 时打印 SDK 发出的请求 URL 与响应状态。
// 用法：node tools/captcha_solve.js            # 自动选SDK：CAPTCHA_SDK env > aliyunsdk新样本 > 旧 v1.3.4 样本
// 背景（37 §8.6）：新样本（224,977 B）上 happy-dom 三连 100s 超时；旧 v1.3.4（273,948 B）是 12 号 §5m.13 A 级跑通用的同一版本
// 输出：单行 JSON {ok, param?, verifyCode?, certifyId?, msg?}
const fs = require("fs");
const path = require("path");
const { Window } = require("happy-dom");

const TMP = process.env.TEMP || "C:\\Users\\12925\\AppData\\Local\\Temp";
const SDK_CANDIDATES = [
  process.env.CAPTCHA_SDK,
  path.join(TMP, "aliyunsdk", "AliyunCaptcha.js"),
  path.join(TMP, "aliyun_captcha.js"),
].filter(Boolean);
const SDK = process.env.CAPTCHA_SDK || (function(){
  const c = [path.join(process.cwd(),"AliyunCaptcha.js"), path.join(process.cwd(),"tools","AliyunCaptcha.js"), ...SDK_CANDIDATES];
  return c.find((p)=>fs.existsSync(p))||c[0];
})();
const SCENE = "11xygtvd";
const PREFIX = "no8xfe";
const REGION = "cn";
const DEBUG = !!process.env.DEBUG;

const log = (...a) => { if (DEBUG) process.stderr.write("  " + a.join(" ") + "\n"); };
function out(o) {
  process.stdout.write(JSON.stringify(o) + "\n");
  process.exit(0);
}


(async () => {
  if (!fs.existsSync(SDK)) return out({ ok: false, msg: "SDK missing: " + SDK });

  const win = new Window({ url: "https://zcode.z.ai/", pretendToBeVisual: true });
  const { document } = win;
  // 伪造最小环境，避免 SDK 直接判定「需要交互」
  win.navigator.userAgent =
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36";
  win.navigator.webdriver = false;
  win.navigator.languages = ["zh-CN", "zh"];
  win.navigator.platform = "Win32";
  win.navigator.hardwareConcurrency = 8;
  win.navigator.deviceMemory = 8;
  win.screen.width = 1920;
  win.screen.height = 1080;
  win.screen.colorDepth = 24;
  win.screen.pixelDepth = 24;

  // 混淆代码裸调 print/alert 等（happy-dom 未实现），补 stub 防脚本执行中断
  win.print = () => {};
  win.alert = () => {};
  win.confirm = () => true;
  win.prompt = () => null;

  // 指纹采集要用 canvas / webgl，happy-dom 默认没有，给最小可用实现
  if (win.HTMLCanvasElement && win.HTMLCanvasElement.prototype) {
    win.HTMLCanvasElement.prototype.getContext = function (kind) {
      if (kind === "2d") {
        return {
          fillStyle: "#000", font: "10px sans-serif", textBaseline: "alphabetic",
          fillRect() {}, fillText() {},
          getImageData: (x, y, w, h) => ({
            data: new Uint8ClampedArray(Math.max(4, w * h * 4)).fill(120),
            width: w, height: h,
          }),
          measureText: () => ({ width: 42 }),
          getTransform: () => ({ a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 }),
          save() {}, restore() {}, beginPath() {}, closePath() {}, stroke() {},
          moveTo() {}, lineTo() {}, arc() {}, rect() {}, scale() {}, rotate() {},
          translate() {}, transform() {}, setTransform() {},
        };
      }
      if (String(kind).indexOf("webgl") === 0) {
        return {
          getExtension: () => null,
          getParameter: () => "WebKit WebGL",
          getSupportedExtensions: () => [],
          getShaderPrecisionFormat: () => ({ rangeMin: 127, rangeMax: 127, precision: 23 }),
          createBuffer: () => ({}), createShader: () => ({}), createProgram: () => ({}),
          getContextAttributes: () => ({}),
        };
      }
      return null;
    };
    win.HTMLCanvasElement.prototype.toDataURL = function () {
      return "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk";
    };
  }

  // PoW 动态包会探测 prompt()；headless 下缺失会抛 "prompt is not defined"，
  // 导致整条链路中断。补一个自动确认的 stub（阿里云这条链路是纯无痕验证，不真需要人工输入）。
  if (typeof win.prompt !== "function") {
    win.prompt = () => "";
  }
  if (typeof win.confirm !== "function") {
    win.confirm = () => true;
  }
  if (typeof win.alert !== "function") {
    win.alert = () => {};
  }

  // happy-dom 缺的历史 DOM API，PoW 动态包会直接抛错中断整条链路。
  if (typeof win.moveBy !== "function") win.moveBy = () => {};
  if (typeof win.moveTo !== "function") win.moveTo = () => {};
  if (typeof win.scroll !== "function") win.scroll = () => {};
  if (typeof win.scrollTo !== "function") win.scrollTo = () => {};
  if (typeof win.scrollBy !== "function") win.scrollBy = () => {};
  if (typeof win.getComputedStyle !== "function") {
    win.getComputedStyle = () => ({ getPropertyValue: () => "" });
  }
  // 节点操作兜底：动态包可能对不存在的父节点做 removeChild
  const fixNode = (Node) => {
    if (!Node || !Node.prototype) return;
    const rm = Node.prototype.removeChild;
    if (rm) {
      Node.prototype.removeChild = function (child) {
        try { return rm.call(this, child); }
        catch (e) { return child; }        // 父节点不在，忽略
      };
    }
    const ap = Node.prototype.appendChild;
    if (ap) {
      Node.prototype.appendChild = function (child) {
        try { return ap.call(this, child); }
        catch (e) { return child; }
      };
    }
    const ib = Node.prototype.insertBefore;
    if (ib) {
      Node.prototype.insertBefore = function (a, b) {
        try { return ib.call(this, a, b); }
        catch (e) { return a; }
      };
    }
  };
  fixNode(win.Node);
  fixNode(win.Element);

  const code = fs.readFileSync(SDK, "utf8");
  try {
    win.eval(code);
  } catch (e) {
    return out({ ok: false, msg: "SDK eval error: " + e.message });
  }

  const init = win.initAliyunCaptcha;
  if (typeof init !== "function") {
    return out({ ok: false, msg: "initAliyunCaptcha not exported", keys: Object.keys(win).slice(0, 20) });
  }

  win.AliyunCaptchaConfig = { region: REGION, prefix: PREFIX };

  const btn = document.createElement("button");
  btn.id = "zcode-captcha-btn";
  btn.style.display = "none";
  document.body.appendChild(btn);

  const done = (o) => out(o);
  const timer = setTimeout(() => done({ ok: false, msg: "timeout 100s" }), 100000);

  // ---- 关键：执行外部注入的 <script src> ----
  // happy-dom 默认不执行动态插入的外部脚本，而 PoW 引擎就在那个包里。
  // 这里 hook createElement/appendChild，把 src 抓下来本地 eval。
  const origCreate = document.createElement.bind(document);
  document.createElement = function (tag, ...rest) {
    const el = origCreate(tag, ...rest);
    if (String(tag).toLowerCase() !== "script") return el;
    const _set = el.setAttribute.bind(el);
    el.setAttribute = function (k, v) {
      _set(k, v);
      if (String(k).toLowerCase() === "src" && /^https?:/i.test(v)) el.__src = v;
    };
    let _src = "";
    Object.defineProperty(el, "src", {
      configurable: true,
      get() { return _src; },
      set(v) { _src = v; if (/^https?:/i.test(v)) el.__src = v; },
    });
    return el;
  };

  let evalDepth = 0;
  const origAppend = document.head.appendChild.bind(document.head);
  async function handleAppend(el) {
    if (el && el.tagName === "SCRIPT" && el.__src) {
      const url = el.__src;
      log("SCRIPT-SRC " + url);
      try {
        const r = await fetch(url, { headers: { Referer: "https://zcode.z.ai/" } });
        const code = await r.text();
        log("  fetched " + r.status + " " + code.length + "B");
        if (r.ok && code.length > 100) {
          // 缓存动态包，便于离线分析
          try {
            const nm = url.split("/").pop().slice(0, 40);
            fs.writeFileSync(path.join(TMP, "aliyunsdk", "pkg_" + nm), code);
            log("  cached pkg_" + nm);
          } catch (e) { /* 缓存失败不影响主流程 */ }
          evalDepth++;
          try { win.eval(code); } finally { evalDepth--; }
          if (typeof el.onload === "function") el.onload();
          return;
        }
      } catch (e) { log("  script err " + e.message); }
      if (typeof el.onerror === "function") el.onerror(new Error("load fail"));
      return;
    }
    if (evalDepth > 0) return;   // 动态包内部再插 script，忽略防递归
    return origAppend(el);
  }
  document.head.appendChild = handleAppend;
  document.body.appendChild = handleAppend;

  const inspect = (p) => {
    try { return JSON.parse(Buffer.from(p, "base64").toString("utf8")); }
    catch (e) { return null; }
  };

  try {
    init({
      SceneId: SCENE,
      prefix: PREFIX,
      region: REGION,
      element: "#zcode-captcha-btn",
      button: "#zcode-captcha-btn",
      mode: "popup",
      showErrorTip: false,
      captchaLogoImg: undefined,

      getInstance: (inst) => {
        // 无痕通道：不弹交互，直接起
        try {
          if (inst && typeof inst.startTracelessVerification === "function") {
            inst.startTracelessVerification();
          } else if (inst && typeof inst.run === "function") {
            inst.run();
          }
        } catch (e) {
          /* 忽略：部分版本 getInstance 里不能再触发 */
        }
      },

      success: (param) => {
        clearTimeout(timer);
        const j = inspect(param) || {};
        log("SUCCESS keys=" + Object.keys(j).join(","));
        done({ ok: true, param, len: (param || "").length,
               certifyId: j.certifyId, hasToken: !!j.securityToken });
      },
      fail: (err) => {
        const p = (err && (err.captchaVerifyParam || err.CaptchaVerifyParam)) || null;
        const vc = (err && (err.verifyCode || err.VerifyCode)) || null;
        clearTimeout(timer);
        log("FAIL verifyCode=" + vc);
        if (p) {
          const j = inspect(p) || {};
          done({ ok: true, param: p, verifyCode: vc, from: "fail",
                 certifyId: j.certifyId, hasToken: !!j.securityToken });
        } else {
          done({ ok: false, verifyCode: vc, certifyId: (err && err.certifyId) || null,
                 msg: (err && (err.Message || err.message || err.errorMessage)) || "fail" });
        }
      },
      onError: (err) => {
        clearTimeout(timer);
        done({ ok: false, msg: "onError: " + ((err && (err.Message || err.message)) || String(err)) });
      },
    });
  } catch (e) {
    clearTimeout(timer);
    out({ ok: false, msg: "init throw: " + e.message });
  }
})();
