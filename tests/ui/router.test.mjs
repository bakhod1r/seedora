// Tests for the edge router.
//
// The router's whole job is one claim — a line between two cards does not pass
// under a third — and that claim is checkable exactly. Before this it was
// checked by looking at the canvas, which finds the line you happen to look at.

import assert from "node:assert/strict";
import test from "node:test";

import { box, grid, loadApp, reader } from "./harness.mjs";

const app = loadApp();
// `app` and the constants are lexical declarations, which live in the context's
// global lexical scope rather than on its global object.
const read = reader(app);
const { routePoints, isClear, segmentHitsBox, roundedPath, lanes } = app;

// The check HANDOFF.md describes as a throwaway script, made permanent: every
// pair of cards on a dense grid gets a route that crosses none of the others.
test("every pair on a 5x5 grid gets a clear route", () => {
  const boxes = grid(5);
  let checked = 0;
  const failures = [];

  for (const from of boxes) {
    for (const to of boxes) {
      if (from === to) continue;
      checked++;
      // Every edge leaves the child's right and enters the parent's left,
      // which is the convention the diagram is drawn to.
      const y1 = (from.top + from.bottom) / 2;
      const y2 = (to.top + to.bottom) / 2;
      const points = routePoints(from.right, y1, to.left, y2, boxes, from.table, to.table);
      if (!isClear(points, boxes, from.table, to.table)) {
        failures.push(`${from.table} -> ${to.table}`);
      }
    }
  }

  assert.equal(checked, 600, "the grid should produce 600 ordered pairs");
  assert.deepEqual(failures, [], `routes crossing a card: ${failures.length} of ${checked}`);
});

// A parent to the left of its child is the case that needs a corridor: the line
// has to go back on itself, and the naive route runs straight through whatever
// is in between.
test("a backwards edge routes around the cards between", () => {
  const boxes = grid(4);
  const from = boxes.find((b) => b.table === "t3_1");
  const to = boxes.find((b) => b.table === "t0_1");

  const points = routePoints(
    from.right, (from.top + from.bottom) / 2,
    to.left, (to.top + to.bottom) / 2,
    boxes, from.table, to.table,
  );

  assert.ok(isClear(points, boxes, from.table, to.table), "route crosses a card");
  assert.ok(points.length > 2, "a backwards edge cannot be a straight line");
});

// Level and unobstructed is the common case and should stay the cheap one.
test("a level unobstructed edge is a straight line", () => {
  const from = box("child", 0, 0);
  const to = box("parent", 500, 0);
  const boxes = [from, to];

  const points = routePoints(from.right, 50, to.left, 50, boxes, "child", "parent");
  assert.equal(points.length, 2);
  assert.equal(points[0].y, 50);
  assert.equal(points[1].y, 50);
});

// Only vertical and horizontal segments. A diagonal on a canvas of rectangles
// reads as a mistake, and the rounding at the corners assumes right angles.
test("routes turn at right angles and never run diagonally", () => {
  const boxes = grid(4);
  for (const from of boxes) {
    for (const to of boxes) {
      if (from === to) continue;
      const points = routePoints(
        from.right, (from.top + from.bottom) / 2,
        to.left, (to.top + to.bottom) / 2,
        boxes, from.table, to.table,
      );
      for (let i = 1; i < points.length; i++) {
        const a = points[i - 1];
        const b = points[i];
        const straight = Math.abs(a.x - b.x) < 0.5 || Math.abs(a.y - b.y) < 0.5;
        assert.ok(straight, `${from.table} -> ${to.table}: diagonal segment`);
      }
    }
  }
});

// The route has to actually start and finish on the anchors it was given, or
// the line detaches from the column it belongs to.
test("a route starts and ends where it was told to", () => {
  const boxes = grid(3);
  const from = boxes[0];
  const to = boxes[8];
  const points = routePoints(from.right, 40, to.left, 700, boxes, from.table, to.table);

  // Compared field by field: the points are built inside the script's own
  // realm, so a structural comparison sees a different Object prototype.
  const first = points[0];
  const last = points[points.length - 1];
  assert.equal(first.x, from.right);
  assert.equal(first.y, 40);
  assert.equal(last.x, to.left);
  assert.equal(last.y, 700);
});

test("segmentHitsBox ignores a line running along a card's edge", () => {
  const b = box("t", 100, 100);
  // Along the top edge exactly: touching, not crossing.
  assert.equal(segmentHitsBox(0, 100, 400, 100, b), false);
  // One pixel inside it: crossing.
  assert.equal(segmentHitsBox(0, 110, 400, 110, b), true);
  // Clear of it entirely.
  assert.equal(segmentHitsBox(0, 50, 400, 50, b), false);
});

test("lanes finds the corridor between two columns of cards", () => {
  const boxes = [box("a", 0, 0), box("b", 400, 0), box("c", 0, 200), box("d", 400, 200)];
  const found = lanes(boxes, "none", "none");
  // The gap between x=200 (right of the left column) and x=400 (left of the
  // right one), so the corridor is at 300.
  assert.ok(found.includes(300), `no corridor at 300, got ${found}`);
});

test("roundedPath drops points that repeat and keeps the ends", () => {
  const d = roundedPath([
    { x: 0, y: 0 }, { x: 0, y: 0 }, { x: 100, y: 0 }, { x: 100, y: 100 },
  ], 12);
  assert.match(d, /^M 0 0/);
  assert.ok(d.includes("100"), d);
});

// A waypoint is a hand-placed instruction and outranks anything computed. It is
// checked through routeAround rather than routePoints, because overriding the
// computation is the whole behaviour.
test("a hand-placed waypoint overrides the computed route", () => {
  const boxes = grid(3);
  const from = boxes[0];
  const to = boxes[8];

  const state = read("app");
  state.waypoints = { "a->b": { x: 999, y: 777 } };
  const withHold = app.routeAround(from.right, 40, to.left, 700, boxes, from.table, to.table, "a->b");
  state.waypoints = {};
  const without = app.routeAround(from.right, 40, to.left, 700, boxes, from.table, to.table, "a->b");

  assert.notEqual(withHold, without, "the waypoint changed nothing");
  assert.ok(withHold.includes("777"), `route does not pass through the waypoint: ${withHold}`);
});
