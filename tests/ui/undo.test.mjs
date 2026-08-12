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

// The round trip. api() is replaced with one that echoes the plan back the way
// the server does, so the stack can be exercised without a server: what is
// under test is which plan gets sent, not what the server does with it.
function withServer() {
  const { app, state } = fresh();
  const sent = [];
  app.api = async (method, path, body) => {
    sent.push({ method, path, plan: structuredClone(body) });
    return { connected: true, plan: structuredClone(body), schema: { tables: [] } };
  };
  // applyState touches the DOM through element ids the harness stub answers
  // happily, so it needs no further help. If it turns out to reach further than
  // the stub goes — a real box measurement, say — replace it too and assert on
  // what pushPlan sent, which is what these tests are actually about:
  //
  //   app.applyState = (s) => { state.plan = s.plan; state.schema = s.schema; };
  //
  // Do that only if it actually throws. The narrower stub is the better test.
  return { app, state, sent };
}

test("committing an edit records the plan as it was", async () => {
  const { app, state } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");

  assert.equal(app.canUndo(), true);
  assert.equal(app.undoLabel(), "row count");
});

test("an unlabelled commit records nothing", async () => {
  const { app, state } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan();

  assert.equal(app.canUndo(), false,
    "saving before a run is not an edit and must not be undoable");
});

test("undo sends the previous plan and can be redone", async () => {
  const { app, state, sent } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");
  await app.undo();

  assert.equal(sent[sent.length - 1].plan.tables.users.rows, 100,
    "undo did not send the plan as it was before the edit");
  assert.equal(app.canUndo(), false);
  assert.equal(app.canRedo(), true);
  assert.equal(app.redoLabel(), "row count");

  await app.redo();
  assert.equal(sent[sent.length - 1].plan.tables.users.rows, 500);
  assert.equal(app.canUndo(), true);
  assert.equal(app.canRedo(), false);
});

test("several edits undo in reverse order", async () => {
  const { app, state, sent } = withServer();

  for (const n of [200, 300, 400]) {
    state.plan.tables.users.rows = n;
    await app.pushPlan(`set ${n}`);
  }

  await app.undo();
  assert.equal(sent[sent.length - 1].plan.tables.users.rows, 300);
  await app.undo();
  assert.equal(sent[sent.length - 1].plan.tables.users.rows, 200);
  await app.undo();
  assert.equal(sent[sent.length - 1].plan.tables.users.rows, 100);
  assert.equal(app.canUndo(), false);
});

test("a rejected commit records nothing", async () => {
  const { app, state } = fresh();
  app.api = async () => { throw new Error("the server said no"); };

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");

  assert.equal(app.canUndo(), false,
    "an edit the server refused never happened, so there is nothing to undo");
});

test("a rejected undo leaves the stack where it was", async () => {
  const { app, state } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");

  app.api = async () => { throw new Error("the server said no"); };
  await app.undo();

  assert.equal(app.canUndo(), true,
    "the undo failed, so the edit is still there to be undone");
  assert.equal(app.canRedo(), false);
});

// The plan can also be replaced from outside the stack — an import, a schema
// apply, the state refresh after a run all arrive as a whole new plan. Those
// paths go through applyState, so applyState is what has to keep base honest.

test("a plan that arrives outside an edit is not undone away", async () => {
  const { app, state, sent } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");

  // An import: a different plan, same database, no edit of ours involved.
  app.applyState({
    connected: true,
    plan: { tables: { users: { rows: 500, columns: {} }, orders: { rows: 7, columns: {} } } },
    schema: { tables: [] },
  });

  state.plan.tables.users.rows = 600;
  await app.pushPlan("row count again");
  await app.undo();

  const back = sent[sent.length - 1].plan;
  assert.ok(back.tables.orders,
    "the undo pushed a plan from before the import, discarding what it brought in");
  assert.equal(back.tables.users.rows, 500);
});

test("concurrent undos do not race", async () => {
  const { app, state } = withServer();
  const echo = app.api;
  // A server that takes a moment, which is what lets two undos overlap.
  app.api = async (m, p, b) => {
    await new Promise((r) => setTimeout(r, 5));
    return echo(m, p, b);
  };

  for (const n of [200, 300]) {
    state.plan.tables.users.rows = n;
    await app.pushPlan(`set ${n}`);
  }

  // Both fired before either resolves — an autorepeating ⌘Z.
  await Promise.all([app.undo(), app.undo()]);

  const history = reader(app)("history");
  assert.equal(history.past.length, 0);
  assert.equal(history.future.length, 2);
  assert.deepEqual(
    Array.from(history.future, (e) => e.plan.tables.users.rows).sort((a, b) => a - b),
    [200, 300],
    "the two undos read the same base, so a redo step points at the wrong plan");
  assert.equal(state.plan.tables.users.rows, 100);
});

test("a rejected undo leaves a snapshot later edits cannot reach into", async () => {
  const { app, state } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");

  app.api = async () => { throw new Error("the server said no"); };
  await app.undo();

  // The edit that follows a failed undo mutates the plan in place, as every
  // edit in the page does. It must not reach into the stack's own copy.
  state.plan.tables.users.rows = 777;

  const history = reader(app)("history");
  assert.equal(history.base.tables.users.rows, 500,
    "the failed undo handed the plan the base object itself, and an in-place " +
    "edit has now rewritten the snapshot the next undo restores");
});

test("a commit that changed nothing is not undoable", async () => {
  const { app, state } = withServer();

  state.plan.tables.users.rows = 500;
  await app.pushPlan("row count");
  // A `change` event on a field nobody edited: the plan is already what the
  // server holds.
  await app.pushPlan("row count");

  const history = reader(app)("history");
  assert.equal(history.past.length, 1,
    "a no-op commit took a slot on the stack, so one undo appears to do nothing");
});
