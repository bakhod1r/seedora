// Tests for the plan history.
//
// The stack is pure state — no DOM, no network — so it is tested here rather
// than in a browser. What a browser is needed for is the keystroke reaching it,
// which tests/browser/ covers.

import assert from "node:assert/strict";
import test from "node:test";

import { loadApp, reader } from "./harness.mjs";

// A fresh script per test: the history is module-level state, and a test that
// inherited the previous test's stack would pass or fail for the wrong reason.
function fresh() {
  const app = loadApp();
  const read = reader(app);
  const state = read("app");
  state.plan = { tables: { users: { rows: 100, columns: {} } } };
  app.resetHistory();
  return { app, state, history: read("history") };
}

test("a fresh history has nothing to undo or redo", () => {
  const { app } = fresh();
  assert.equal(app.canUndo(), false);
  assert.equal(app.canRedo(), false);
});

test("recording an edit makes it undoable", () => {
  const { app, state } = fresh();
  const before = structuredClone(state.plan);

  state.plan.tables.users.rows = 500;
  app.recordEdit("row count", before);

  assert.equal(app.canUndo(), true);
  assert.equal(app.undoLabel(), "row count");
});

test("the stack keeps only the most recent edits", () => {
  const { app, state } = fresh();
  const limit = reader(app)("UNDO_LIMIT");

  for (let i = 0; i < limit + 10; i++) {
    const before = structuredClone(state.plan);
    state.plan.tables.users.rows = i;
    app.recordEdit(`edit ${i}`, before);
  }

  const history = reader(app)("history");
  assert.equal(history.past.length, limit,
    "the stack grew past its limit and would hold a plan per edit forever");
  // The oldest were dropped, not the newest.
  assert.equal(app.undoLabel(), `edit ${limit + 9}`);
});

test("a snapshot is a copy, not a reference to the live plan", () => {
  const { app, state } = fresh();
  const before = structuredClone(state.plan);
  app.recordEdit("row count", before);

  // Mutating the plan afterwards must not reach into the stack.
  state.plan.tables.users.rows = 9999;
  const history = reader(app)("history");
  assert.equal(history.past[0].plan.tables.users.rows, 100);
});

test("recording an edit discards the redo branch", () => {
  const { app, state } = fresh();
  const history = reader(app)("history");

  history.future.push({ label: "something redoable", plan: {} });
  app.recordEdit("a new edit", structuredClone(state.plan));

  assert.equal(app.canRedo(), false,
    "a new edit after an undo must not leave a redo pointing at an abandoned branch");
});

test("resetHistory clears both stacks", () => {
  const { app, state } = fresh();
  app.recordEdit("one", structuredClone(state.plan));
  app.resetHistory();

  assert.equal(app.canUndo(), false);
  assert.equal(app.canRedo(), false);
});
