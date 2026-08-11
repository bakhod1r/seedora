// A headless loader for the page's script.
//
// app.js is a plain script, not a module: it is served from a Go binary with no
// build step, and giving it exports so it could be tested would mean adding the
// toolchain the project exists without. So it is loaded here the way a browser
// loads it — evaluated in one scope — against a DOM stubbed just far enough to
// get through the top of the file, and its top-level function declarations are
// read off the resulting global object.
//
// This buys tests for the parts that are pure geometry: the edge router and the
// layout relaxation, which are the largest and previously least-tested things
// in the codebase. It does not pretend to test rendering. Anything that needs a
// real box model needs a real browser, and that is a different tool.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
export const appPath = join(here, "..", "..", "internal", "ui", "assets", "app.js");

// A DOM element that answers to everything and remembers nothing. The script
// reaches for elements at load time and wires listeners to them; none of that
// matters to the geometry, and a stub that throws on an unexpected property
// would make this harness a maintenance burden rather than a test.
function stubElement(tag = "div") {
  const node = {
    tagName: tag.toUpperCase(),
    style: {},
    dataset: {},
    classList: {
      add() {}, remove() {}, toggle() {}, contains() { return false; },
    },
    children: [],
    hidden: false,
    value: "",
    textContent: "",
    innerHTML: "",
    className: "",
    checked: false,
    offsetWidth: 220,
    offsetHeight: 120,
    clientWidth: 1280,
    clientHeight: 800,
    scrollLeft: 0,
    scrollTop: 0,
    appendChild(c) { this.children.push(c); return c; },
    append() {},
    removeChild() {},
    remove() {},
    replaceChildren() { this.children = []; },
    insertBefore(c) { this.children.push(c); return c; },
    addEventListener() {},
    removeEventListener() {},
    setAttribute() {},
    getAttribute() { return null; },
    removeAttribute() {},
    closest() { return null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    getBoundingClientRect() {
      return { left: 0, top: 0, right: 220, bottom: 120, width: 220, height: 120 };
    },
    focus() {},
    blur() {},
    click() {},
    contains() { return false; },
    scrollIntoView() {},
    showModal() {},
    close() {},
    matches() { return false; },
  };
  return node;
}

// load evaluates app.js and returns its globals.
export function loadApp() {
  const source = readFileSync(appPath, "utf8");

  const document = {
    documentElement: stubElement("html"),
    body: stubElement("body"),
    head: stubElement("head"),
    getElementById: () => stubElement(),
    createElement: (tag) => stubElement(tag),
    createElementNS: (_ns, tag) => stubElement(tag),
    createDocumentFragment: () => stubElement(),
    querySelector: () => stubElement(),
    querySelectorAll: () => [],
    addEventListener() {},
    removeEventListener() {},
    get activeElement() { return null; },
  };

  const storage = new Map();
  const localStorage = {
    getItem: (k) => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => storage.set(k, String(v)),
    removeItem: (k) => storage.delete(k),
    clear: () => storage.clear(),
  };

  const sandbox = {
    document,
    localStorage,
    console,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    requestAnimationFrame: (fn) => setTimeout(() => fn(0), 0),
    cancelAnimationFrame: clearTimeout,
    // boot() calls the API immediately. Failing is fine and is what a page with
    // no server behind it does; the script catches it and shows a toast.
    fetch: () => Promise.reject(new Error("no server in the test harness")),
    EventSource: class {
      addEventListener() {}
      close() {}
    },
    URL,
    Blob: class {},
    matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
    getComputedStyle: () => ({ getPropertyValue: () => "" }),
    navigator: { clipboard: { writeText: () => Promise.resolve() }, userAgent: "node" },
    location: { href: "http://127.0.0.1:7777/", origin: "http://127.0.0.1:7777" },
    history: { replaceState() {} },
    performance,
    Math,
    Date,
    JSON,
    structuredClone,
  };
  sandbox.addEventListener = () => {};
  sandbox.removeEventListener = () => {};
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.self = sandbox;

  const context = vm.createContext(sandbox);
  vm.runInContext(source, context, { filename: "app.js" });
  return context;
}

// reader evaluates an expression inside the loaded script's own scope.
//
// Top-level `const` and `let` are lexical declarations: they live in the
// context's global lexical environment, not on its global object, so they
// cannot be read off the object loadApp returns. Evaluating in the same context
// can see them.
export function reader(context) {
  return (expression) => vm.runInContext(expression, context, { filename: "read.js" });
}

// box is a card as the router sees one.
export function box(table, left, top, width = 200, height = 100) {
  return { table, left, top, right: left + width, bottom: top + height };
}

// grid lays out n×n cards with a real gap between them, which is the shape the
// layout produces and the shape the router has to cope with.
export function grid(n, { width = 200, height = 100, gapX = 120, gapY = 60 } = {}) {
  const boxes = [];
  for (let col = 0; col < n; col++) {
    for (let row = 0; row < n; row++) {
      boxes.push(box(
        `t${col}_${row}`,
        col * (width + gapX),
        row * (height + gapY),
        width, height,
      ));
    }
  }
  return boxes;
}
