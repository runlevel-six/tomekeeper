// Tests for the touch gestures in static/tome.js.
//
// There is no build step, no framework and no package.json, and this does not add
// any: it is a stub DOM built by hand, the real tome.js run against it, and
// synthetic touch sequences dispatched at the listeners it registered. Run it with
// `task test:js`, which is `node` and nothing else.
//
// It exists because the two gestures have guards that cannot be checked by hand.
// Whether a left-to-right drag inside a wide code block scrolls the block or
// navigates away is not something anybody can reliably reproduce on a phone, and
// "I let go earlier than I meant to" is a real explanation for a failed manual
// test — one that already cost a round of investigation.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(here, "..", "static", "tome.js"), "utf8");

let failures = 0;
let checks = 0;

function check(what, got, want) {
  checks++;
  if (got !== want) {
    failures++;
    console.error(`FAIL ${what}\n       got  ${JSON.stringify(got)}\n       want ${JSON.stringify(want)}`);
  }
}

// --- the stub -------------------------------------------------------------
//
// Only what tome.js actually touches. Anything it reaches for that is missing
// throws, which is the point: a stub that quietly answers undefined would let the
// script take a path no browser takes.

function element(attrs = {}) {
  const el = {
    tagName: attrs.tagName || "DIV",
    className: attrs.className || "",
    parentElement: null,
    scrollWidth: attrs.scrollWidth ?? 100,
    clientWidth: attrs.clientWidth ?? 100,
    overflowX: attrs.overflowX || "visible",
    attributes: new Map(),
    style: {
      props: new Map(),
      setProperty(k, v) { this.props.set(k, v); },
      removeProperty(k) { this.props.delete(k); },
    },
    clicks: 0,
    classList: { contains: () => false, add() {}, remove() {} },
    setAttribute(k, v) { this.attributes.set(k, v); },
    removeAttribute(k) { this.attributes.delete(k); },
    hasAttribute(k) { return this.attributes.has(k); },
    getAttribute(k) { return this.attributes.get(k) ?? null; },
    click() { this.clicks++; },
    querySelector: () => null,
    closest: () => null,
  };
  return el;
}

function makeWorld({ page = "article", bodyScrollsSideways = false } = {}) {
  const listeners = new Map();
  const navigated = [];

  const backLink = element({ tagName: "A", className: "back" });
  backLink.click = () => navigated.push("up");

  const markLink = element({ tagName: "A", className: "mark-read" });
  markLink.href = "/mark-read?from=unread";

  const streamEnd = element({ className: "stream-end" });
  streamEnd.querySelector = (sel) => (sel === "a.mark-read" ? markLink : null);

  // Where a touch starts. A wide code block is the case that must not be stolen.
  const wideBlock = element({
    tagName: "PRE", scrollWidth: 800, clientWidth: 300, overflowX: "auto",
  });
  const proseNode = element({ tagName: "P" });
  const body = element({ tagName: "BODY" });
  wideBlock.parentElement = body;
  proseNode.parentElement = body;

  const article = element({ tagName: "ARTICLE", className: "reader" });

  const document = {
    body,
    documentElement: { scrollHeight: 2000 },
    visibilityState: "visible",
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    querySelector(sel) {
      if (sel === ".reader-nav a[rel='up']") return page === "article" ? backLink : null;
      if (sel === "article.reader") return page === "article" ? article : null;
      if (sel === ".stream-end[data-pull-to-mark]") return page === "list" ? streamEnd : null;
      if (sel === ".stream[data-mark-on-scroll]") return null;
      return null;
    },
    querySelectorAll: () => [],
  };
  body.addEventListener = () => {};

  const window = {
    scrollY: 1000,
    innerHeight: 1000,
    addEventListener() {},
    setTimeout: () => 0,
    clearTimeout() {},
    getComputedStyle: (el) => ({ overflowX: el.overflowX }),
    location: { href: "", assign(h) { this.href = h; } },
    IntersectionObserver: class { observe() {} disconnect() {} },
  };

  const sandbox = {
    document, window, navigator: {}, performance: { now: () => 0 },
    IntersectionObserver: window.IntersectionObserver,
    Map, Set, Array, Math, String, Number, JSON, URLSearchParams,
    setTimeout: window.setTimeout, clearTimeout: window.clearTimeout,
    fetch: () => Promise.resolve({ ok: true }),
    console,
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  function fire(type, points, changed) {
    const handlers = listeners.get(type) || [];
    const event = {
      touches: points.map(([x, y]) => ({ clientX: x, clientY: y })),
      changedTouches: (changed || points).map(([x, y]) => ({ clientX: x, clientY: y })),
      target: bodyScrollsSideways ? wideBlock : proseNode,
      preventDefault() {},
    };
    handlers.forEach((h) => h(event));
  }

  return { fire, navigated, article, streamEnd, get href() { return window.location.href; } };
}

// --- swipe left-to-right to go back --------------------------------------

{
  const w = makeWorld();
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[140, 405]]);
  w.fire("touchend", [[140, 405]]);
  check("a clear left-to-right swipe goes back", w.navigated.join(","), "up");
}

{
  const w = makeWorld();
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[60, 402]]);
  w.fire("touchend", [[60, 402]]);
  check("a short swipe does not go back", w.navigated.length, 0);
}

{
  const w = makeWorld();
  w.fire("touchstart", [[200, 400]]);
  w.fire("touchmove", [[80, 402]]);
  w.fire("touchend", [[80, 402]]);
  check("a right-to-left swipe does nothing", w.navigated.length, 0);
}

{
  // Reading is mostly vertical scrolling, and a diagonal must not finish as a
  // navigation just because it ended up far enough across.
  const w = makeWorld();
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[60, 600]]);
  w.fire("touchmove", [[140, 700]]);
  w.fire("touchend", [[140, 700]]);
  check("a drag that turns into a scroll does not go back", w.navigated.length, 0);
}

{
  // The guard nobody can test on a phone: archived bodies carry wide code blocks
  // and tables, and the first sideways drag inside one belongs to the block.
  const w = makeWorld({ bodyScrollsSideways: true });
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[140, 405]]);
  w.fire("touchend", [[140, 405]]);
  check("a swipe starting in a wide code block is left alone", w.navigated.length, 0);
}

{
  // A drag that begins vertical and ends horizontal. The end-of-gesture check
  // cannot catch this one — by the time the finger lifts it has travelled further
  // across than down — so this is what makes the mid-drag bail load-bearing. Found
  // by neutering that bail and watching every case still pass.
  const w = makeWorld();
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[40, 700]]);
  w.fire("touchmove", [[460, 760]]);
  w.fire("touchend", [[460, 760]]);
  check("a scroll that turns into a swipe does not go back", w.navigated.length, 0);
}

{
  // Two fingers down, then one lifted and dragged. The mid-drag guard sees a
  // single touch by then, so only refusing to start on two fingers stops this.
  const w = makeWorld();
  w.fire("touchstart", [[20, 400], [40, 500]]);
  w.fire("touchmove", [[140, 405]]);
  w.fire("touchend", [[140, 405]]);
  check("a pinch that becomes a drag does not go back", w.navigated.length, 0);
}

{
  const w = makeWorld();
  w.fire("touchstart", [[20, 400], [40, 500]]);
  w.fire("touchmove", [[140, 405], [160, 505]]);
  w.fire("touchend", [[140, 405]]);
  check("two fingers is not a swipe", w.navigated.length, 0);
}

{
  const w = makeWorld({ page: "list" });
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[140, 405]]);
  w.fire("touchend", [[140, 405]]);
  check("there is nothing to go back from on a list", w.navigated.length, 0);
}

{
  // The article follows the finger, and stops following it when the swipe is
  // abandoned. This is the feedback, so it is worth asserting it clears.
  const w = makeWorld();
  w.fire("touchstart", [[20, 400]]);
  w.fire("touchmove", [[65, 402]]);
  check("mid-swipe, the article is offset", w.article.style.props.get("--swipe"), "0.5");
  check("mid-swipe, the article is marked", w.article.hasAttribute("data-swiping"), true);
  w.fire("touchend", [[65, 402]]);
  check("after release, the offset is gone", w.article.style.props.has("--swipe"), false);
  check("after release, the mark is gone", w.article.hasAttribute("data-swiping"), false);
}

// --- pull past the bottom to mark a list read ----------------------------

{
  const w = makeWorld({ page: "list" });
  w.fire("touchstart", [[200, 800]]);
  w.fire("touchmove", [[200, 700]]);
  check("past the threshold, the control is armed", w.streamEnd.hasAttribute("data-pull-armed"), true);
  w.fire("touchend", [[200, 700]]);
  check("releasing an armed pull follows the mark link", w.href, "/mark-read?from=unread");
}

{
  // The behaviour the maintainer could not confirm by hand: pulling up past the
  // threshold and then back down again abandons it.
  const w = makeWorld({ page: "list" });
  w.fire("touchstart", [[200, 800]]);
  w.fire("touchmove", [[200, 700]]);
  check("armed on the way up", w.streamEnd.hasAttribute("data-pull-armed"), true);
  w.fire("touchmove", [[200, 780]]);
  check("disarmed on the way back down", w.streamEnd.hasAttribute("data-pull-armed"), false);
  w.fire("touchend", [[200, 780]]);
  check("releasing a disarmed pull does nothing", w.href, "");
}

{
  const w = makeWorld({ page: "list" });
  w.fire("touchstart", [[200, 800]]);
  w.fire("touchmove", [[200, 770]]);
  w.fire("touchend", [[200, 770]]);
  check("a short pull does nothing", w.href, "");
}

if (failures > 0) {
  console.error(`\n${failures} of ${checks} checks failed.`);
  process.exit(1);
}
console.log(`ok   ${checks} gesture checks passed`);
