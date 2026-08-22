// Tests for the layout relaxation.
//
// relax pulls every card towards the average height of what it is joined to and
// then separates whatever that pushed into an overlap. The two properties worth
// holding it to are the ones it exists for: related cards end up nearer each
// other, and no two cards in a column overlap when it is finished. Both were
// previously verified by looking at the canvas.

import assert from "node:assert/strict";
import test from "node:test";

import { loadApp, reader } from "./harness.mjs";

const app = loadApp();
const read = reader(app);
const GAP_Y = read("GAP_Y");
const { relax, separate, close, lift, slackFor } = app;
const state = read("app");

// relax orders each column by where its parents sit in the one before it, and
// that reads the live schema and plan. The fixture below is the schema those
// nodes describe, in the shape the page holds it.
function installSchema() {
  const fk = (name, parent) => ({
    name,
    type: "integer",
    fk: parent ? { table: parent, column: "id" } : null,
  });
  state.schema = {
    tables: [
      { name: "users", columns: [fk("id")] },
      { name: "products", columns: [fk("id")] },
      { name: "orders", columns: [fk("id"), fk("user_id", "users")] },
      { name: "addresses", columns: [fk("id"), fk("user_id", "users")] },
      { name: "sessions", columns: [fk("id"), fk("user_id", "users")] },
      {
        name: "order_items",
        columns: [fk("id"), fk("order_id", "orders"), fk("product_id", "products")],
      },
    ],
  };
  state.plan = { tables: {} };
  for (const t of state.schema.tables) {
    state.plan.tables[t.name] = { columns: Object.fromEntries(t.columns.map((c) => [c.name, {}])) };
  }
  state.pinned = {};
  state.drafts = [];
  state.layout = {};
}

installSchema();

// node builds one card for the relaxation, in the shape buildNodes produces.
function node(name, depth, y, links = [], { h = 200, fixed = false } = {}) {
  return { name, depth, x: depth * 400, y, h, fixed, links: new Set(links) };
}

// chain is a schema shaped like most schemas: a few roots, children hanging off
// them, laid out in dependency columns and deliberately scattered vertically so
// the relaxation has something to do.
function chain() {
  const nodes = new Map();
  const add = (n) => nodes.set(n.name, n);

  add(node("users", 0, 0, ["orders", "addresses", "sessions"]));
  add(node("products", 0, 2400, ["order_items"]));
  add(node("orders", 1, 3000, ["users", "order_items"]));
  add(node("addresses", 1, 200, ["users"]));
  add(node("sessions", 1, 2800, ["users"]));
  add(node("order_items", 2, 100, ["orders", "products"]));
  return nodes;
}

// meanLinkDistance is the number the relaxation exists to bring down: how far
// apart, vertically, two cards that are joined to each other sit.
function meanLinkDistance(nodes) {
  let total = 0;
  let pairs = 0;
  for (const n of nodes.values()) {
    for (const other of n.links) {
      const o = nodes.get(other);
      if (!o) continue;
      total += Math.abs((n.y + n.h / 2) - (o.y + o.h / 2));
      pairs++;
    }
  }
  return pairs === 0 ? 0 : total / pairs;
}

function overlaps(nodes) {
  const byColumn = new Map();
  for (const n of nodes.values()) {
    if (!byColumn.has(n.depth)) byColumn.set(n.depth, []);
    byColumn.get(n.depth).push(n);
  }
  const found = [];
  for (const column of byColumn.values()) {
    const sorted = [...column].sort((a, b) => a.y - b.y);
    for (let i = 1; i < sorted.length; i++) {
      const above = sorted[i - 1];
      const here = sorted[i];
      if (here.y < above.y + above.h) found.push(`${above.name}/${here.name}`);
    }
  }
  return found;
}

test("relax brings related tables closer together", () => {
  const nodes = chain();
  const before = meanLinkDistance(nodes);

  relax(nodes);
  const after = meanLinkDistance(nodes);

  assert.ok(after < before,
    `relax made it worse: ${before.toFixed(0)} -> ${after.toFixed(0)}`);
  // Not a marginal improvement: the starting arrangement is deliberately bad
  // and the relaxation should close most of the gap.
  assert.ok(after < before / 2,
    `relax barely moved anything: ${before.toFixed(0)} -> ${after.toFixed(0)}`);
});

test("relax leaves no two cards in a column overlapping", () => {
  const nodes = chain();
  relax(nodes);
  assert.deepEqual(overlaps(nodes), [], "cards overlap after relaxation");
});

// A card the user dragged is a decision, and the layout arranges around it
// rather than undoing it.
test("relax does not move a pinned card", () => {
  const nodes = chain();
  const pinned = nodes.get("orders");
  pinned.fixed = true;
  const where = pinned.y;

  relax(nodes);

  assert.equal(pinned.y, where, "a pinned card moved");
});

// The arrangement has to settle, or the diagram rearranges itself every time it
// is redrawn. What settles is the shape — where the cards are relative to each
// other. The group as a whole keeps drifting downwards, because nothing in the
// relaxation anchors it to an absolute height and every card is pulled towards
// its neighbours' centres, which sit below their tops. That drift never reaches
// the page: autoLayout measures the group and subtracts its top before writing
// any of it into app.layout, and it builds the nodes fresh each time, so relax
// never actually runs twice over the same ones. This is the property to hold,
// and the absolute one would be a false alarm.
test("relax settles into a stable arrangement", () => {
  const shape = (nodes) => {
    const top = Math.min(...[...nodes.values()].map((n) => n.y));
    return [...nodes.values()].map((n) => n.y - top);
  };

  const nodes = chain();
  relax(nodes);
  const settled = shape(nodes);

  relax(nodes);
  const again = shape(nodes);

  for (let i = 0; i < settled.length; i++) {
    assert.ok(Math.abs(settled[i] - again[i]) < 1,
      `card ${i} shifted ${Math.abs(settled[i] - again[i]).toFixed(1)}px relative to the rest`);
  }
});

test("separate keeps the order it was given", () => {
  const column = [
    node("a", 0, 0), node("b", 0, 10), node("c", 0, 20),
  ];
  separate(column);

  const order = [...column].sort((a, b) => a.y - b.y).map((n) => n.name);
  assert.deepEqual(order, ["a", "b", "c"]);
});

test("separate leaves the declared gap between cards", () => {
  const column = [node("a", 0, 0), node("b", 0, 5)];
  separate(column);

  const [a, b] = [...column].sort((x, y) => x.y - y.y);
  assert.ok(b.y >= a.y + a.h + GAP_Y - 0.001,
    `gap is ${(b.y - a.y - a.h).toFixed(1)}, want at least ${GAP_Y}`);
});

// An anchored card cannot be moved, so the cards around it are what give way —
// including the one above, which the downward pass cannot help with because
// moving it down is what caused the overlap.
test("separate moves a card up out of an anchor rather than through it", () => {
  const anchored = node("middle", 0, 1000, [], { fixed: true });
  // Starts overlapping the anchor from above: its bottom is inside it.
  const above = node("top", 0, 900);
  const column = [above, anchored, node("bottom", 0, 1050)];

  separate(column);

  assert.equal(anchored.y, 1000, "the anchor moved");
  assert.ok(above.y + above.h + GAP_Y <= anchored.y + 0.001,
    `the card above still overlaps the anchor: its bottom is ${above.y + above.h}, ` +
    `the anchor starts at ${anchored.y}`);
  assert.deepEqual(overlaps(new Map(column.map((n) => [n.name, n]))), []);
});

// ---- closing the holes
//
// The relaxation leaves gaps: a card pulled down towards its neighbours takes
// its column's height with it and nothing fills the space it left. close and
// lift are what take that space back, and both have to do it without disturbing
// the order the pull worked out or the cards somebody pinned.

test("close pulls a card up to the slack above it", () => {
  const column = [node("a", 0, 0), node("b", 0, 4000)];

  close(column);

  const [a, b] = [...column].sort((x, y) => x.y - y.y);
  assert.ok(b.y < 4000, "the card did not move");
  assert.ok(b.y <= a.y + a.h + slackFor(a.h) + 0.001,
    `gap is ${(b.y - a.y - a.h).toFixed(1)}, want at most ${slackFor(a.h)}`);
});

test("close leaves a card that is already close alone", () => {
  const column = [node("a", 0, 0), node("b", 0, 200 + GAP_Y)];
  close(column);

  const b = column[1];
  assert.equal(b.y, 200 + GAP_Y, "a card that was already tight was moved");
});

test("close does not move a pinned card", () => {
  const column = [node("a", 0, 0), node("b", 0, 4000, [], { fixed: true })];
  close(column);

  assert.equal(column[1].y, 4000, "a pinned card was pulled up");
});

test("close keeps the order it was given", () => {
  const column = [node("a", 0, 0), node("b", 0, 3000), node("c", 0, 5000)];
  close(column);

  const order = [...column].sort((x, y) => x.y - y.y).map((n) => n.name);
  assert.deepEqual(order, ["a", "b", "c"]);
});

test("lift slides a column up to the top of the drawing", () => {
  const left = [node("a", 0, 0), node("b", 0, 300)];
  const right = [node("c", 1, 2000), node("d", 1, 2300)];

  lift([left, right]);

  assert.equal(left[0].y, 0, "the topmost column moved");
  assert.ok(right[0].y <= slackFor(right[0].h) + 0.001,
    `the lifted column starts at ${right[0].y}, want at most ${slackFor(right[0].h)}`);
  // Moved as one piece: the arrangement inside it is what the pull worked out.
  assert.equal(right[1].y - right[0].y, 300, "the column's internal spacing changed");
});

test("lift leaves a column holding a pinned card where it is", () => {
  const left = [node("a", 0, 0)];
  const right = [node("c", 1, 2000, [], { fixed: true }), node("d", 1, 2300)];

  lift([left, right]);

  assert.equal(right[0].y, 2000, "a column with a pinned card was lifted");
  assert.equal(right[1].y, 2300, "a card beside a pinned one was lifted");
});

// Every column is measured against the top of the drawing, not against the one
// before it, so which order they arrive in changes nothing.
test("lift raises a column whatever its place in the list", () => {
  const late = [node("a", 0, 500)];
  const first = [node("c", 1, 0)];

  lift([late, first]);

  assert.equal(first[0].y, 0, "the topmost column was moved");
  assert.ok(late[0].y <= slackFor(late[0].h) + 0.001,
    `the trailing column starts at ${late[0].y}, want at most ${slackFor(late[0].h)}`);
});
