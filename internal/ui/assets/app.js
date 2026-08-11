// Seedora — mapping editor.
//
// The page holds the plan, the server holds the connection. Every edit updates
// the local plan and pushes the whole thing back: it is a few hundred kilobytes
// at most, and a patch protocol would be the only stateful thing in the server.
//
// No framework, no build step. The page is served from a single Go binary, and
// adding a bundler would mean adding a toolchain to a tool whose selling point
// is not having one.

"use strict";

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};

const app = {
  state: null,
  plan: null,
  schema: null,
  generators: [],
  byId: new Map(),
  sel: null,     // {table, column}
  running: false,
  lastRun: null, // {dry, byTable, at} — what the last run wrote
  layout: {},    // table name -> {x, y}
  pinned: {},    // table name -> true when a person put it there by hand
  hues: {},      // table name -> hue index
  picker: [],    // the generator list as currently filtered
  pickerAt: -1,  // keyboard cursor into it
  // Schema edits waiting to be applied. Drafts are tables that do not exist
  // yet; pending holds the changes to tables that do. Both are client-side
  // until Apply, so nothing touches the database until the SQL has been read.
  zoom: 1,       // diagram scale, persisted per database
  focus: null,   // table whose relationships are lit
  flow: null,    // key of the edge being followed, if any
  waypoints: {}, // edge key -> {x, y} a line was dragged through
  collapsed: {}, // table name -> true when its columns are folded away
  rendered: new Set(), // tables already on screen, so only new ones animate in
  drafts: [],    // [{table, columns: [...]}]
  pending: [],   // ddl.Change values for existing tables
  editing: new Set(), // tables showing their edit controls
};

const fmt = (n) => Number(n).toLocaleString();

// ---------------------------------------------------------------- transport

async function api(method, path, body) {
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: body ? { "Content-Type": "application/json" } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    // fetch throws for a network-level failure, and the browser's own words for
    // it are "Failed to fetch", which says nothing about what happened. In this
    // page it means one thing: nothing is listening. That is nearly always a
    // stopped binary or a page opened from disk rather than from the server.
    throw new Error(
      "Seedora is not answering — the server it was serving this page from has stopped. " +
      "Start it again and reload."
    );
  }
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText);
    err.problems = (data && data.problems) || [];
    throw err;
  }
  return data;
}

let busyDepth = 0;
function busy(on, text) {
  busyDepth = Math.max(0, busyDepth + (on ? 1 : -1));
  $("busy").hidden = busyDepth === 0;
  if (text) $("busy-text").textContent = text;
}

let toastTimer;
function toast(msg, kind) {
  const box = $("toast");
  $("toast-text").textContent = msg;
  box.className = "toast" + (kind ? " " + kind : "");
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (box.hidden = true), 5000);
}

// ---------------------------------------------------------------- theme

// The theme follows the system until someone overrides it, and the override is
// remembered. Reading it before first paint would need inline script; the flash
// is one frame and not worth the coupling.
function initTheme() {
  const saved = localStorage.getItem("seedora:theme");
  if (saved) document.documentElement.dataset.theme = saved;
}

$("btn-theme").addEventListener("click", () => {
  const root = document.documentElement;
  const dark = root.dataset.theme
    ? root.dataset.theme === "dark"
    : !window.matchMedia("(prefers-color-scheme: light)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  localStorage.setItem("seedora:theme", root.dataset.theme);
});

// ---------------------------------------------------------------- ask dialog
//
// Stands in for confirm() and prompt(). Returns a promise so calling code reads
// the same as it did before, and the dialog is a real <dialog>, so focus is
// trapped and Escape works without any of it being written here.

const askDialog = $("ask-dialog");
let askResolve = null;

function ask({ title, body, danger, okLabel = "Continue", input = null, alert = null, choices = null }) {
  $("ask-title").textContent = title;
  $("ask-body").textContent = body || "";
  $("ask-body").hidden = !body;

  const alertBox = $("ask-alert");
  alertBox.textContent = alert || "";
  alertBox.hidden = !alert;

  // A question with several answers rather than two. The chosen one is
  // remembered so OK can settle with it, and clicking a choice twice is the
  // same as choosing it and pressing OK — the gesture people try first.
  const choiceBox = $("ask-choices");
  choiceBox.innerHTML = "";
  choiceBox.hidden = !choices;
  let chosen = choices ? choices[0].value : null;
  if (choices) {
    for (const c of choices) {
      const btn = el("button", "ask-choice" + (c.value === chosen ? " on" : ""));
      btn.type = "button";
      btn.appendChild(el("span", "mark", c.value === chosen ? "●" : "○"));
      const body = el("span", "body");
      body.appendChild(el("span", "name", c.label));
      if (c.hint) body.appendChild(el("span", "hint", c.hint));
      btn.appendChild(body);
      btn.addEventListener("click", () => {
        if (chosen === c.value) return settleAsk(c.value);
        chosen = c.value;
        for (const n of choiceBox.children) {
          const on = n === btn;
          n.classList.toggle("on", on);
          n.firstChild.textContent = on ? "●" : "○";
        }
      });
      choiceBox.appendChild(btn);
    }
  }
  askChoice = () => chosen;

  const field = $("ask-field");
  const inp = $("ask-input");
  field.hidden = !input;
  if (input) {
    $("ask-label").textContent = input.label || "";
    inp.type = input.type || "text";
    // Prefilled when the caller has a sensible answer already, so the common
    // case is one keystroke rather than typing a name that was on screen.
    inp.value = input.value || "";
    if (input.value) requestAnimationFrame(() => inp.select());
  }

  const ok = $("ask-ok");
  ok.textContent = okLabel;
  ok.className = "btn " + (danger ? "btn-danger" : "btn-primary");

  askDialog.showModal();
  // Focus the field if there is one, so a password can be typed straight away.
  requestAnimationFrame(() => (input ? inp : ok).focus());

  return new Promise((resolve) => {
    askResolve = resolve;
  });
}

let askChoice = () => null;

function settleAsk(value) {
  const resolve = askResolve;
  askResolve = null;
  askDialog.close();
  if (resolve) resolve(value);
}

$("ask-ok").addEventListener("click", () => {
  if (!$("ask-choices").hidden) return settleAsk(askChoice());
  settleAsk($("ask-field").hidden ? true : $("ask-input").value);
});
$("ask-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    settleAsk($("ask-input").value);
  }
});
// Covers Escape and the Cancel button alike, since both close the dialog.
askDialog.addEventListener("close", () => settleAsk(null));

// ------------------------------------------------------ panel dismissal
//
// The inspector and the preview are panels, not dialogs: they sit over the
// canvas but do not block it, so nothing closes them on their own. A click
// outside is the gesture people try first.

document.addEventListener("pointerdown", (e) => {
  // A click inside a dialog is over the panels, not on the canvas behind them.
  if (document.querySelector("dialog[open]")) return;

  const insp = $("inspector");
  if (!insp.hidden && !e.target.closest("#inspector, .col, .hue-menu")) {
    closeInspector();
  }

  const preview = $("preview");
  if (!preview.hidden && !e.target.closest("#preview, .table-foot")) {
    preview.hidden = true;
  }
});

// ------------------------------------------------------- dialog dismissal
//
// A click on the backdrop closes the dialog. The backdrop is not an element of
// its own, so the test is whether the click landed outside the dialog's box —
// which is also what makes a click on a <select> dropdown, whose coordinates
// can fall outside it, not count as a dismissal.

for (const dialog of document.querySelectorAll("dialog")) {
  dialog.addEventListener("click", (e) => {
    if (e.target !== dialog) return;
    const box = dialog.getBoundingClientRect();
    const inside =
      e.clientX >= box.left && e.clientX <= box.right &&
      e.clientY >= box.top && e.clientY <= box.bottom;
    if (!inside) dialog.close();
  });
}

// ---------------------------------------------------------------- state

function applyState(s) {
  app.state = s;
  app.plan = s.plan;
  app.schema = s.schema;

  // A plan describes one database's tables. Connecting to another makes every
  // snapshot in the history a description of something that is no longer there.
  if (s.target !== historyTarget) {
    historyTarget = s.target;
    resetHistory();
  }

  $("target").hidden = !s.connected;
  if (s.connected) {
    $("target-engine").textContent = s.engine;
    $("target-dsn").textContent = s.target;
  }

  $("connect").hidden = s.connected;
  $("board").hidden = !s.connected;

  for (const id of ["btn-seed", "btn-save", "btn-import", "btn-export",
                    "btn-reset-layout", "btn-palette", "btn-new-table",
                    "btn-fold-all", "btn-history"]) {
    const btn = $(id);
    if (btn) btn.disabled = !s.connected;
  }

  renderProblems(s.problems);
  if (s.connected) renderDiagram();
  if (app.sel) renderInspector();
}

function renderProblems(problems) {
  const box = $("problems");
  box.innerHTML = "";
  if (!problems || !problems.length) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.appendChild(el("strong", null,
    `${problems.length} problem${problems.length === 1 ? "" : "s"} — seeding will fail until these are fixed`));
  for (const p of problems) box.appendChild(el("div", null, p));
}

// pushPlan sends the whole plan and applies the server's reply.
//
// A label makes the edit undoable and is what the toast and the button tooltip
// say. Saving without one — before a run, say — is not an edit and does not
// belong in the history.
async function pushPlan(label) {
  const before = history.base;
  try {
    applyState(await api("PUT", "/api/plan", app.plan));
  } catch (e) {
    toast(e.message, "bad");
    return;
  }
  // Recorded only after the server has taken it. An edit it refused never
  // happened, and offering to undo it would be offering to undo nothing.
  if (label) recordEdit(label, before);
  history.base = snapshotPlan();
  updateHistoryButtons();
}

// undo puts the plan back the way it was before the last recorded edit.
//
// It goes through the server like any other change, because the server holds
// the plan and a local revert would leave the two disagreeing until the next
// save. That also means an undo can fail, and a failed undo has to leave the
// stack exactly as it found it.
async function undo() {
  if (!canUndo()) return;
  const entry = history.past.pop();
  const current = history.base;

  app.plan = structuredClone(entry.plan);
  try {
    applyState(await api("PUT", "/api/plan", app.plan));
  } catch (e) {
    history.past.push(entry);
    app.plan = current;
    toast(e.message, "bad");
    return;
  }

  history.future.push({ label: entry.label, plan: current });
  history.base = snapshotPlan();
  renderDiagram();
  updateHistoryButtons();
  toast(`Undid ${entry.label}`, "good");
}

// redo reapplies what undo took away.
async function redo() {
  if (!canRedo()) return;
  const entry = history.future.pop();
  const current = history.base;

  app.plan = structuredClone(entry.plan);
  try {
    applyState(await api("PUT", "/api/plan", app.plan));
  } catch (e) {
    history.future.push(entry);
    app.plan = current;
    toast(e.message, "bad");
    return;
  }

  history.past.push({ label: entry.label, plan: current });
  history.base = snapshotPlan();
  renderDiagram();
  updateHistoryButtons();
  toast(`Redid ${entry.label}`, "good");
}

// ---- undo
//
// Every plan edit commits through pushPlan, which is what makes undo cheap:
// there is one place that knows an edit has happened and succeeded, so there is
// one place to snapshot from. Individual edits do not have to remember to
// record themselves, and none of them can forget.
//
// What is stored is the whole plan rather than a diff. A plan for a large
// schema is a few hundred kilobytes and an edit touches one column of it, so a
// diff would be smaller — and it would also be a second representation of the
// plan to keep correct, for a saving nobody can perceive.

// UNDO_LIMIT bounds what the stack holds. Twenty-five edits is further back
// than anyone reaches by keystroke, and it stops a long session from keeping
// every plan it ever had.
const UNDO_LIMIT = 25;

// historyTarget is the database the stack belongs to, as the redacted DSN the
// state carries. Comparing it is how a reconnect is noticed.
let historyTarget = null;

const history = {
  past: [],   // [{label, plan}] — oldest first
  future: [], // [{label, plan}] — what undo took away
  // base is the plan as of the last successful commit. It is what gets pushed
  // onto past when the *next* edit commits, which is why an edit does not need
  // to snapshot before mutating.
  base: null,
};

function snapshotPlan() {
  return app.plan ? structuredClone(app.plan) : null;
}

// resetHistory starts over. Connecting to another database makes every stored
// plan meaningless — they describe tables this database may not have.
function resetHistory() {
  history.past = [];
  history.future = [];
  history.base = snapshotPlan();
}

// recordEdit pushes the plan as it was before the edit that just committed.
function recordEdit(label, before) {
  if (!before) return;
  history.past.push({ label, plan: before });
  if (history.past.length > UNDO_LIMIT) history.past.shift();
  // Editing after undoing abandons the branch that was undone. Keeping it would
  // mean a redo that reapplies a change on top of a plan it was never made
  // against.
  history.future = [];
}

function canUndo() { return history.past.length > 0; }
function canRedo() { return history.future.length > 0; }

function undoLabel() {
  return canUndo() ? history.past[history.past.length - 1].label : null;
}

function redoLabel() {
  return canRedo() ? history.future[history.future.length - 1].label : null;
}

// updateHistoryButtons reflects the stack in the toolbar. Defined here because
// pushPlan calls it; the buttons themselves arrive with the bindings.
function updateHistoryButtons() {
  const undoBtn = $("btn-undo");
  const redoBtn = $("btn-redo");
  if (!undoBtn || !redoBtn) return;

  undoBtn.disabled = !canUndo();
  redoBtn.disabled = !canRedo();
  undoBtn.title = canUndo() ? `Undo ${undoLabel()}` : "Nothing to undo";
  redoBtn.title = canRedo() ? `Redo ${redoLabel()}` : "Nothing to redo";
}

// ---------------------------------------------------------------- diagram

function renderDiagram() {
  const host = $("tables");
  host.innerHTML = "";

  loadLayout();
  loadPinned();
  loadHues();
  loadZoom();
  loadCollapsed();
  loadWaypoints();

  // The diagram is rebuilt on nearly every edit, so only cards that were not
  // there a moment ago animate in. Everything else appears where it was.
  const known = app.rendered;
  app.rendered = new Set();
  let index = 0;
  const mount = (card, name) => {
    app.rendered.add(name);
    if (!known.has(name)) {
      card.classList.add("entering");
      card.style.setProperty("--i", index++);
      card.addEventListener("animationend", () => card.classList.remove("entering"),
        { once: true });
    }
    host.appendChild(card);
  };

  for (const t of app.schema.tables) {
    if (!app.plan.tables[t.name]) continue;
    mount(tableCard(t, app.plan.tables[t.name]), t.name);
  }
  for (const d of app.drafts) mount(draftCard(d), d.table);
  syncChangeBar();

  // Positions are only known once the cards are measured: a card's height
  // depends on how many columns its table has.
  requestAnimationFrame(() => {
    placeCards();
    applyFocus();
    drawEdges();
  });
}

function tableCard(t, tp) {
  const card = el("section", "table" + (tp.skip ? " excluded" : ""));
  card.dataset.table = t.name;
  card.dataset.hue = hueOf(t.name);

  // Name takes the slack, the two numbers sit at the right. No spacer: the name
  // itself is the flexible item, which keeps the header from reordering when a
  // table has a very short or very long name.
  const collapsed = !!app.collapsed[t.name];
  if (collapsed) card.classList.add("collapsed");

  const head = el("div", "table-head");

  // Folding a table away leaves its header, and its header is what the edges
  // attach to when there is no column row to point at. Twenty expanded cards
  // is more schema than fits on a screen at a readable size.
  const fold = el("button", "fold");
  fold.type = "button";
  fold.textContent = collapsed ? "▸" : "▾";
  fold.title = collapsed ? `Show ${t.name}'s columns` : `Fold ${t.name} away`;
  fold.setAttribute("aria-expanded", collapsed ? "false" : "true");
  fold.addEventListener("click", (e) => {
    e.stopPropagation();
    toggleCollapse(t.name);
  });
  head.appendChild(fold);

  head.appendChild(el("span", "table-name", t.name));

  const count = el("span", "table-count", t.existing_rows ? fmt(t.existing_rows) : "empty");
  count.title = t.existing_rows
    ? `${fmt(t.existing_rows)} rows already in this table`
    : "This table is empty";
  head.appendChild(count);

  const swatch = el("button", "swatch");
  swatch.type = "button";
  swatch.title = "Change this table's colour";
  swatch.setAttribute("aria-label", `Colour for ${t.name}`);
  swatch.addEventListener("click", (e) => {
    e.stopPropagation();
    openHueMenu(t.name, swatch);
  });
  head.insertBefore(swatch, head.firstChild);

  const rows = el("input", "rows-input");
  rows.type = "number";
  rows.min = "0";
  rows.value = tp.rows;
  rows.title = "Rows to generate";
  rows.setAttribute("aria-label", `Rows to generate for ${t.name}`);
  rows.addEventListener("change", () => {
    tp.rows = Math.max(0, parseInt(rows.value, 10) || 0);
    pushPlan(`row count for ${t.name}`);
  });
  head.appendChild(rows);
  // Clicking the header lights this table and everything it is joined to. The
  // header, not the whole card, because a click on a column row means something
  // else already.
  head.addEventListener("click", (e) => {
    if (e.target.closest("input, button")) return;
    focusTable(t.name);
  });
  card.appendChild(head);
  makeDraggable(card, head, t.name);

  makeRelationTarget(card, t);

  const editing = app.editing.has(t.name);
  if (droppingTable(t.name)) card.classList.add("dropping");
  if (collapsed) {
    // The column count stands in for the columns themselves, so a folded card
    // still says how much is behind it.
    const foot = el("div", "table-foot");
    foot.appendChild(el("span", "field-hint",
      `${t.columns.length} column${t.columns.length === 1 ? "" : "s"}`));
    card.appendChild(foot);
    return card;
  }

  for (const c of orderedColumns(t, tp)) {
    const cp = tp.columns[c.name];
    if (!cp) continue;
    const row = columnRow(t, c, cp);
    if (droppingColumn(t.name, c.name)) row.classList.add("dropping");
    if (editing) row.appendChild(dropColumnButton(t, c));
    makeColumnSortable(row, t, tp);
    card.appendChild(row);
  }
  // Columns added but not yet applied sit where they will end up, editable, so
  // the card shows the table as it is about to be rather than as it is.
  for (const ch of app.pending) {
    if (ch.kind !== "add_column" || ch.table !== t.name) continue;
    card.appendChild(columnEditor(ch.columns[0], () => {
      app.pending.splice(app.pending.indexOf(ch), 1);
      renderDiagram();
    }));
  }

  const foot = el("div", "table-foot");
  const preview = el("button", "btn btn-quiet", "Preview");
  preview.addEventListener("click", () => previewTable(t.name));
  foot.appendChild(preview);

  const edit = el("button", "btn btn-quiet", editing ? "Done" : "Edit");
  edit.title = "Add or drop columns on this table";
  edit.addEventListener("click", () => {
    if (editing) app.editing.delete(t.name);
    else app.editing.add(t.name);
    renderDiagram();
  });
  foot.appendChild(edit);

  if (editing) {
    const add = el("button", "btn btn-quiet", "+ column");
    add.addEventListener("click", () => {
      app.pending.push({
        kind: "add_column", table: t.name,
        columns: [{ name: "", type: defaultType(), nullable: true }],
      });
      renderDiagram();
    });
    foot.appendChild(add);

    const drop = el("button", "btn btn-quiet", droppingTable(t.name) ? "Keep table" : "Drop table");
    drop.classList.add("danger-quiet");
    drop.addEventListener("click", () => toggleDropTable(t.name));
    foot.appendChild(drop);
  }

  const guesses = Object.values(tp.columns).filter((x) => x.confidence === "low").length;
  if (guesses > 0) foot.appendChild(el("span", "field-hint", `${guesses} to check`));

  foot.appendChild(el("div", "spacer"));

  // What the last run wrote to this table, kept on the card until the next run.
  // A toast that vanishes in five seconds is no way to report what was written.
  const wrote = app.lastRun && app.lastRun.byTable[t.name];
  if (wrote) {
    const badge = el("span", "wrote" + (app.lastRun.dry ? " dry" : ""),
      (app.lastRun.dry ? "" : "+") + fmt(wrote));
    badge.title = app.lastRun.dry
      ? `${fmt(wrote)} rows generated and discarded — dry run`
      : `${fmt(wrote)} rows written by the last run`;
    foot.appendChild(badge);
  }

  card.appendChild(foot);
  return card;
}

// orderedColumns is the table's columns in the order the plan holds, which is
// the order they were dragged into. Anything the plan does not mention follows
// in catalog order, so a column added to the database appears rather than
// vanishing.
function orderedColumns(t, tp) {
  const byName = new Map(t.columns.map((c) => [c.name, c]));
  const out = [];
  const seen = new Set();
  for (const name of tp.order || []) {
    const c = byName.get(name);
    if (c && !seen.has(name)) {
      out.push(c);
      seen.add(name);
    }
  }
  for (const c of t.columns) if (!seen.has(c.name)) out.push(c);
  return out;
}

// makeColumnSortable lets a column row be dragged into a new position within
// its own card.
//
// The HTML drag-and-drop API rather than pointer events: it gives the drag
// image, the cursor, and the escape-to-cancel for free, and a column row is a
// small enough target that reimplementing those would be most of the work for
// none of the behaviour.
function makeColumnSortable(row, t, tp) {
  row.draggable = true;

  row.addEventListener("dragstart", (e) => {
    dragging = { table: t.name, column: row.dataset.column };
    row.classList.add("dragging-col");
    e.dataTransfer.effectAllowed = "all";
    // Firefox starts no drag at all without payload, even one nothing reads.
    e.dataTransfer.setData("text/plain", row.dataset.column);
  });

  row.addEventListener("dragend", () => {
    dragging = null;
    for (const n of document.querySelectorAll(".drop-above, .drop-below, .drop-link")) {
      n.classList.remove("drop-above", "drop-below", "drop-link");
    }
    row.classList.remove("dragging-col");
  });

  row.addEventListener("dragover", (e) => {
    if (!dragging) return;

    // Within its own table a drag reorders. Onto another table's column it
    // draws a relationship: the child column is pointed at the one it was
    // dropped on. Both are the same gesture because both are the same question
    // — where does this column belong.
    if (dragging.table !== t.name) {
      e.preventDefault();
      e.dataTransfer.dropEffect = "link";
      row.classList.add("drop-link");
      return;
    }
    if (dragging.column === row.dataset.column) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";

    const box = row.getBoundingClientRect();
    const above = e.clientY < box.top + box.height / 2;
    row.classList.toggle("drop-above", above);
    row.classList.toggle("drop-below", !above);
  });

  row.addEventListener("dragleave", () => {
    row.classList.remove("drop-above", "drop-below", "drop-link");
  });

  row.addEventListener("drop", (e) => {
    if (!dragging) return;
    e.preventDefault();
    row.classList.remove("drop-link");

    if (dragging.table !== t.name) {
      dropOnColumn(dragging.table, dragging.column, t.name, row.dataset.column);
      return;
    }
    const box = row.getBoundingClientRect();
    const above = e.clientY < box.top + box.height / 2;
    moveColumn(t, tp, dragging.column, row.dataset.column, above);
  });
}

let dragging = null;

const isPK = (table, column) => {
  const t = app.schema.tables.find((x) => x.name === table);
  return !!t && (t.primary_key || []).includes(column);
};

// dropOnColumn decides which end of a dragged relationship is the child.
//
// A foreign key always runs child → parent, and which is which is not the
// direction of the gesture: dragging `users.id` onto `orders.user_id` and
// dragging `orders.user_id` onto `users.id` mean the same relationship, and the
// key belongs on orders both times. A primary key is never itself a foreign
// key, so when one end is a key and the other is not, the answer is the schema's
// rather than the mouse's.
function dropOnColumn(fromTable, fromColumn, toTable, toColumn) {
  if (fromTable === toTable) return;

  let child = { table: fromTable, column: fromColumn };
  let parent = { table: toTable, column: toColumn };
  if (isPK(fromTable, fromColumn) && !isPK(toTable, toColumn)) {
    child = { table: toTable, column: toColumn };
    parent = { table: fromTable, column: fromColumn };
  }
  relateColumns(child.table, child.column, parent.table, parent.column);
}

// makeRelationTarget lets a column be dropped on another table's card rather
// than on one of its columns, which means "this table needs a key pointing at
// mine".
//
// Dropping on a column relates two things that both already exist. Dropping on
// the card is the case where the child column does not exist yet — the ordinary
// way a relationship is added to a schema — so it adds one. That is a real
// schema change and goes through the same SQL dialog as every other one.
function makeRelationTarget(card, t) {
  card.addEventListener("dragover", (e) => {
    if (!dragging || dragging.table === t.name) return;
    // A drop meant for a column row is handled there; this only claims the
    // space between and around them.
    if (e.target.closest(".col")) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "link";
    card.classList.add("drop-link");
  });

  card.addEventListener("dragleave", (e) => {
    if (e.target === card) card.classList.remove("drop-link");
  });

  card.addEventListener("drop", (e) => {
    if (!dragging || dragging.table === t.name) return;
    if (e.target.closest(".col")) return;
    e.preventDefault();
    card.classList.remove("drop-link");
    addRelationColumn(dragging.table, dragging.column, t.name);
  });
}

// addRelationColumn adds the relationship a key dropped on a table describes.
//
// Which relationship is a question, not a default. A foreign key column on the
// child is one-to-many; the same column with a unique constraint is one-to-one;
// many-to-many is neither, and is a third table holding both keys. Those are
// three different schemas, and guessing wrong here is a table somebody has to
// migrate later — so it is asked, once, with what each one means.
async function addRelationColumn(parentTable, parentColumn, childTable) {
  const parent = app.schema.tables.find((x) => x.name === parentTable);
  const source = parent && parent.columns.find((c) => c.name === parentColumn);
  if (!source) return;

  // Pointing at a column that is not unique is legal and gives every child row
  // a parent chosen from duplicates, which is nearly never what was meant. The
  // engine refuses it outright once a real constraint is involved.
  if (!isPK(parentTable, parentColumn) && !source.unique) {
    await ask({
      title: `${parentTable}.${parentColumn} cannot be referenced`,
      body: "A foreign key can only point at a primary key or a unique column. " +
        "Drag the key column itself, or make this one unique first.",
      okLabel: "Close",
    });
    return;
  }

  const child = app.schema.tables.find((x) => x.name === childTable);
  if (!child) return;

  const kind = await ask({
    title: `How are ${parentTable} and ${childTable} related?`,
    body: `Each ${singular(childTable)} points at ${singular(parentTable)} — the question is how many go the other way.`,
    okLabel: "Continue",
    choices: [
      {
        value: "many",
        label: `One ${singular(parentTable)} — many ${childTable}`,
        hint: `A ${fkColumnName(parentTable, parentColumn, child)} column on ${childTable}. The usual case.`,
      },
      {
        value: "one",
        label: `One ${singular(parentTable)} — one ${singular(childTable)}`,
        hint: `The same column, unique, so no two ${childTable} share a ${singular(parentTable)}.`,
      },
      {
        value: "manyToMany",
        label: `Many ${parentTable} — many ${childTable}`,
        hint: `Neither table can hold this: a third table, ${joinTableName(parentTable, childTable)}, holds both keys.`,
      },
    ],
  });
  if (!kind) return;

  if (kind === "manyToMany") return addJoinTable(parentTable, childTable);

  // SQLite cannot add a unique column to a table that exists — the constraint
  // is an index it will not create through ALTER. Saying so before the name is
  // typed beats a validation error after it.
  if (kind === "one" && isSQLite()) {
    await ask({
      title: "SQLite cannot add a unique column to an existing table",
      body: `Add the column as one-to-many and create a unique index on ${childTable} separately, ` +
        "or recreate the table with the column unique.",
      okLabel: "Close",
    });
    return;
  }

  const suggested = fkColumnName(parentTable, parentColumn, child);
  const name = await ask({
    title: `Add ${childTable}.${suggested} → ${parentTable}.${parentColumn}?`,
    body: `A new column on ${childTable}, pointing at ${parentTable}.${parentColumn}` +
      (kind === "one" ? ", unique." : ".") + " Nothing runs until you apply the SQL.",
    okLabel: "Add column",
    input: { label: "Column name", value: suggested },
  });
  if (name === null) return;

  const column = String(name).trim() || suggested;
  if (child.columns.some((c) => c.name === column)) {
    toast(`${childTable} already has a column named ${column}`, "bad");
    return;
  }

  app.pending.push({
    kind: "add_column",
    table: childTable,
    columns: [{
      name: column,
      type: fkType(source),
      // A table that already has rows cannot take a NOT NULL column without a
      // default, and there is no sensible default for a key.
      nullable: true,
      pk: false,
      unique: kind === "one",
      references: `${parentTable}.${parentColumn}`,
    }],
  });
  renderDiagram();
  reviewSchema();
}

// addJoinTable builds the third table a many-to-many needs: one key per side,
// both of them the primary key, so the same pair cannot be recorded twice.
async function addJoinTable(a, b) {
  const ta = app.schema.tables.find((t) => t.name === a);
  const tb = app.schema.tables.find((t) => t.name === b);
  const ka = ta && (ta.primary_key || [])[0];
  const kb = tb && (tb.primary_key || [])[0];
  if (!ka || !kb) {
    await ask({
      title: "Both tables need a single-column primary key",
      body: "A join table is made of one key from each side, and there is nothing to point at otherwise.",
      okLabel: "Close",
    });
    return;
  }

  const suggested = joinTableName(a, b);
  const name = await ask({
    title: `Add ${suggested}?`,
    body: `A new table with ${singular(a)}_${ka} and ${singular(b)}_${kb}, ` +
      "both of them its primary key so the same pair cannot appear twice.",
    okLabel: "Add table",
    input: { label: "Table name", value: suggested },
  });
  if (name === null) return;

  const table = String(name).trim() || suggested;
  if (app.schema.tables.some((t) => t.name === table) ||
      app.drafts.some((d) => d.table === table)) {
    toast(`${table} already exists`, "bad");
    return;
  }

  const colA = ta.columns.find((c) => c.name === ka);
  const colB = tb.columns.find((c) => c.name === kb);
  app.drafts.push({
    table,
    columns: [
      { name: `${singular(a)}_${ka}`, type: fkType(colA), pk: true, nullable: false,
        references: `${a}.${ka}` },
      { name: `${singular(b)}_${kb}`, type: fkType(colB), pk: true, nullable: false,
        references: `${b}.${kb}` },
    ],
  });
  if (!app.layout[table]) app.layout[table] = draftSpot();
  saveLayout();
  renderDiagram();
  reviewSchema();
}

// singular is the English guess, and it is only ever a suggested name the user
// can overwrite in the dialog.
const singular = (name) => name.replace(/ies$/, "y").replace(/([^s])s$/, "$1");

// joinTableName is the convention both Rails and Django settle on: the two
// table names, alphabetical, joined by an underscore.
const joinTableName = (a, b) => [a, b].sort().join("_");

// fkType is the parent key's type with the auto-assignment taken off: a child
// column must hold the same values, and must not generate its own.
function fkType(source) {
  const native = (source.native || source.type || "").trim();
  const bare = native.replace(/\s*(auto_increment|generated\s+(by\s+default|always)\s+as\s+identity)/gi, "").trim();
  const lower = bare.toLowerCase();
  if (lower === "bigserial") return "bigint";
  if (lower === "serial") return "integer";
  if (lower === "smallserial") return "smallint";
  return bare || "integer";
}

// fkColumnName proposes `user_id` for `users.id`, and falls back to something
// unambiguous when that name is taken.
function fkColumnName(parentTable, parentColumn, child) {
  let name = `${singular(parentTable)}_${parentColumn}`;
  if (!child.columns.some((c) => c.name === name)) return name;
  name = `${parentTable}_${parentColumn}`;
  let n = 2;
  while (child.columns.some((c) => c.name === name)) name = `${parentTable}_${parentColumn}_${n++}`;
  return name;
}

// relateColumns points a child column at a parent column, which is what
// dragging one onto the other means.
//
// This is a mapping change, not a schema change: the column is filled from the
// parent's keys when the run happens. The database's own foreign key, if it
// wants one, is a separate decision made in the schema editor — a diagram that
// silently issued ALTER TABLE because something was dragged would be a trap.
async function relateColumns(childTable, childColumn, parentTable, parentColumn) {
  if (childTable === parentTable && childColumn === parentColumn) return;

  const tp = app.plan.tables[childTable];
  const cp = tp && tp.columns[childColumn];
  if (!cp) return;

  const parent = app.schema.tables.find((t) => t.name === parentTable);
  const target = parent && parent.columns.find((c) => c.name === parentColumn);
  if (!target) return;

  // Drawing to a column that is not unique produces rows that point at
  // whichever duplicate the draw happened to land on. That is legal and
  // sometimes wanted, so it is a question rather than a refusal.
  const pk = (parent.primary_key || []).includes(parentColumn);
  if (!pk && !target.unique) {
    const ok = await ask({
      title: `Point ${childTable}.${childColumn} at ${parentTable}.${parentColumn}?`,
      body: `${parentColumn} is neither a primary key nor unique, so the values drawn from it repeat. ` +
        "That is fine for a lookup and wrong for a key.",
      okLabel: "Relate anyway",
    });
    if (!ok) return;
  }

  cp.generator = "foreign_key";
  cp.references = `${parentTable}.${parentColumn}`;
  cp.skip = false;
  cp.confidence = "manual";
  cp.why = `drawn in the diagram — points at ${parentTable}.${parentColumn}`;

  await pushPlan(`${childTable}.${childColumn} → ${parentTable}.${parentColumn}`);
  // Light up what was just joined, because the whole point of drawing it was to
  // see it.
  setFocus(childTable);
  // Which relationship this is, in the same words the drop-on-a-card dialog
  // uses: a unique child column is one-to-one, anything else is one-to-many.
  const one = cp.unique || isPK(childTable, childColumn) ||
    ((app.schema.tables.find((t) => t.name === childTable) || {}).columns || [])
      .some((c) => c.name === childColumn && c.unique);
  toast(`${childTable}.${childColumn} now points at ${parentTable}.${parentColumn}` +
    ` · one ${singular(parentTable)} — ${one ? "one " + singular(childTable) : "many " + childTable}`, "good");

  await offerConstraint(childTable, childColumn, parentTable, parentColumn, target);
}

// offerConstraint asks whether the database should enforce the relationship
// that was just drawn.
//
// Drawing one is a mapping change and stays one: a diagram that issued ALTER
// TABLE because something was dragged would be a trap. But the reason to draw
// it is usually that the schema is missing the key, so the question is worth
// asking once — and the answer goes through the SQL dialog like every other
// schema change.
async function offerConstraint(childTable, childColumn, parentTable, parentColumn, target) {
  if (isSQLite()) return; // No ALTER TABLE ADD CONSTRAINT there at all.

  const child = app.schema.tables.find((t) => t.name === childTable);
  const col = child && child.columns.find((c) => c.name === childColumn);
  if (!col || col.fk) return; // The database already enforces it.

  const parentIsKey = isPK(parentTable, parentColumn) || (target && target.unique);
  if (!parentIsKey) return; // The engine would refuse the constraint.

  const ok = await ask({
    title: "Add the foreign key to the database too?",
    body: `The mapping now fills ${childTable}.${childColumn} from ` +
      `${parentTable}.${parentColumn}. The database does not enforce it — ` +
      "adding the constraint makes it a real relationship, and existing rows " +
      "that break it will make the statement fail.",
    okLabel: "Add constraint",
  });
  if (!ok) return;

  app.pending.push({
    kind: "add_foreign_key",
    table: childTable,
    column: childColumn,
    references: `${parentTable}.${parentColumn}`,
  });
  renderDiagram();
  reviewSchema();
}

function moveColumn(t, tp, column, target, above) {
  // The stored order can be short or stale, so it is rebuilt from what is on
  // screen before anything is moved: reordering a list that does not match the
  // card is how a column ends up somewhere nobody dropped it.
  const order = orderedColumns(t, tp).map((c) => c.name);
  const from = order.indexOf(column);
  if (from < 0) return;
  order.splice(from, 1);

  let at = order.indexOf(target);
  if (at < 0) return;
  if (!above) at++;
  order.splice(at, 0, column);

  tp.order = order;
  renderDiagram();
  pushPlan(`column order in ${t.name}`);
}

function columnRow(t, c, cp) {
  const row = el("div", "col" + (cp.skip ? " skipped" : ""));
  row.id = anchorId(t.name, c.name);
  row.dataset.table = t.name;
  row.dataset.column = c.name;

  const isPK = (t.primary_key || []).includes(c.name);
  const key = el("span", "col-key" + (isPK ? " pk" : c.fk ? " fk" : ""),
    isPK ? "◆" : c.fk ? "→" : "");
  key.title = isPK ? "Primary key" : c.fk ? `references ${c.fk.table}.${c.fk.column}` : "";
  row.appendChild(key);

  const name = el("span", "col-name");
  name.append(c.name);
  if (!c.nullable) {
    const nn = el("span", "nn", "NN");
    nn.title = "NOT NULL";
    name.appendChild(nn);
  }
  name.title = c.native || c.type;
  row.appendChild(name);

  const g = app.byId.get(cp.generator);
  const badge = el("span", "gen" +
    (cp.confidence === "low" ? " guess" : "") +
    (g && g.group === "structure" ? " structure" : ""), badgeText(cp));
  badge.title = cp.why || "";
  row.appendChild(badge);

  row.addEventListener("click", () => select(t.name, c.name));
  return row;
}

// badgeText is what the column row shows: the generator, plus the one option
// that changes what it means.
function badgeText(cp) {
  if (cp.skip) return "database";
  if (cp.generator === "foreign_key" && cp.references) return "→ " + cp.references;
  const g = app.byId.get(cp.generator);
  return (g ? g.label : cp.generator) + (cp.unique ? " · unique" : "");
}

const anchorId = (t, c) => `col-${slug(t)}--${slug(c)}`;
const slug = (s) => s.replace(/[^a-zA-Z0-9_-]/g, "_");

// ---------------------------------------------------------------- colour
//
// Each table carries a hue. It is not decoration: the same colour marks the
// card, its rail, and every foreign key pointing at it, so a relationship can be
// traced across the canvas without reading a label.
//
// The default is derived from the table's name, so two people opening the same
// schema see the same colours without either having chosen them. Picking one
// overrides it, per database, in the same local storage as the layout — colour
// is presentation, and seedora.yaml is configuration.

const HUES = 8;

const hueKey = () =>
  "seedora:hues:" + (app.state && app.state.target ? app.state.target : "?");

function loadHues() {
  try {
    app.hues = JSON.parse(localStorage.getItem(hueKey())) || {};
  } catch {
    app.hues = {};
  }
}

function saveHues() {
  try {
    localStorage.setItem(hueKey(), JSON.stringify(app.hues));
  } catch {
    // Storage being unavailable costs the chosen colours and nothing else.
  }
}

// hueOf is the table's colour: the one that was picked, or one derived from the
// name so it is stable across sessions and machines.
function hueOf(name) {
  if (app.hues[name] != null) return app.hues[name];
  // FNV-1a, so adjacent names like `order` and `orders` land far apart rather
  // than on the same colour.
  let h = 2166136261;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return Math.abs(h) % HUES;
}

const hueMenu = $("hue-menu");
let hueMenuFor = null;

function openHueMenu(table, anchor) {
  hueMenuFor = table;
  const current = hueOf(table);

  const grid = $("hue-grid");
  grid.innerHTML = "";
  for (let i = 0; i < HUES; i++) {
    const dot = el("button", "hue-dot");
    dot.type = "button";
    dot.dataset.hue = i;
    dot.setAttribute("role", "radio");
    dot.setAttribute("aria-checked", i === current ? "true" : "false");
    dot.setAttribute("aria-label", `Colour ${i + 1}`);
    dot.addEventListener("click", () => {
      app.hues[table] = i;
      saveHues();
      const card = cardOf(table);
      if (card) card.dataset.hue = i;
      drawEdges();
      hueMenu.hidePopover();
    });
    grid.appendChild(dot);
  }

  // Positioned against the swatch. The popover lives in the top layer, so the
  // coordinates are viewport coordinates and no ancestor can clip it.
  hueMenu.showPopover();
  const box = anchor.getBoundingClientRect();
  const menu = hueMenu.getBoundingClientRect();
  const left = Math.min(box.left, window.innerWidth - menu.width - 8);
  const top = box.bottom + menu.height > window.innerHeight
    ? box.top - menu.height - 6
    : box.bottom + 6;
  hueMenu.style.left = Math.max(8, left) + "px";
  hueMenu.style.top = Math.max(8, top) + "px";
}

// ---------------------------------------------------------------- layout
//
// Cards are absolutely positioned so they can be dragged. Where they start is
// computed from the foreign-key graph: a table sits one column right of the
// furthest table it depends on, so parents are on the left and the arrows mostly
// point one way. That is the arrangement someone would reach for by hand, which
// means the first thing they see needs no rearranging.
//
// Positions are per-database and live in localStorage, not in seedora.yaml.
// Layout is not seeding configuration, and putting it in the committed file
// would mean every teammate's window size showed up as a diff.

const CARD_W = 300;
const GAP_X = 92;
const GAP_Y = 26;

const layoutKey = () =>
  "seedora:layout:" + (app.state && app.state.target ? app.state.target : "?");

function loadLayout() {
  try {
    app.layout = JSON.parse(localStorage.getItem(layoutKey())) || {};
  } catch {
    app.layout = {};
  }
}

function saveLayout() {
  try {
    localStorage.setItem(layoutKey(), JSON.stringify(app.layout));
    localStorage.setItem(pinnedKey(), JSON.stringify(app.pinned));
  } catch {
    // A full or disabled storage costs the saved arrangement and nothing else.
  }
}

// A position is remembered for every card, but only a dragged card is pinned.
// The distinction is what folding needs: a folded card is short and an unfolded
// one is tall, so a position computed at one height is wrong at the other. Every
// unpinned card is therefore laid out again whenever the heights change, and a
// card someone placed by hand stays where they put it.
const pinnedKey = () =>
  "seedora:pinned:" + (app.state && app.state.target ? app.state.target : "?");

function loadPinned() {
  try {
    app.pinned = JSON.parse(localStorage.getItem(pinnedKey())) || {};
  } catch {
    app.pinned = {};
  }
}

// depthOf ranks a table by how far downstream of its parents it is. A cycle
// cannot be ranked, so the walk bails at a depth no real schema reaches rather
// than looping.
function depthOf(name, seen = new Set()) {
  if (seen.has(name) || seen.size > 64) return 0;
  seen.add(name);

  const t = app.schema.tables.find((x) => x.name === name);
  const tp = app.plan.tables[name];
  if (!t || !tp) return 0;

  let deepest = -1;
  for (const c of t.columns) {
    const parent = parentOf(t, c, tp.columns[c.name]);
    if (!parent || parent === name) continue;
    deepest = Math.max(deepest, depthOf(parent, new Set(seen)));
  }
  return deepest + 1;
}

// parentOf returns the table a column points at, or null.
function parentOf(t, c, cp) {
  if (!cp || cp.skip || cp.generator !== "foreign_key") return null;
  const ref = cp.references || (c.fk ? `${c.fk.table}.${c.fk.column}` : "");
  if (!ref) return null;
  const name = ref.slice(0, ref.lastIndexOf("."));
  return app.plan.tables[name] ? name : null;
}

// relatedGroups splits the tables into sets that reference each other, largest
// first: the main schema is what someone opened the page to look at, and it
// belongs at the top. Two schemas that share no keys are two pictures, and
// interleaving them helps nobody.
function relatedGroups() {
  const parent = new Map();
  const find = (a) => {
    while (parent.get(a) !== a) {
      parent.set(a, parent.get(parent.get(a)));
      a = parent.get(a);
    }
    return a;
  };
  const union = (a, b) => {
    const ra = find(a);
    const rb = find(b);
    if (ra !== rb) parent.set(ra, rb);
  };

  const names = [];
  for (const t of app.schema.tables) {
    if (!app.plan.tables[t.name]) continue;
    parent.set(t.name, t.name);
    names.push(t.name);
  }
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;
    for (const c of t.columns) {
      const p = parentOf(t, c, tp.columns[c.name]);
      if (p && parent.has(p)) union(t.name, p);
    }
  }

  const groups = new Map();
  for (const name of names) {
    const root = find(name);
    if (!groups.has(root)) groups.set(root, []);
    groups.get(root).push(name);
  }
  return [...groups.values()].sort((a, b) => b.length - a.length || a[0].localeCompare(b[0]));
}

// orderColumn sorts one column of cards by where the tables they point at ended
// up in the column before it — the barycentre rule every layered graph drawing
// uses, and the cheapest thing that measurably reduces crossings.
function orderColumn(names, previous) {
  const rank = new Map(previous.map((n, i) => [n, i]));

  const weight = (name) => {
    const t = app.schema.tables.find((x) => x.name === name);
    const tp = app.plan.tables[name];
    if (!t || !tp) return Number.MAX_SAFE_INTEGER;

    let sum = 0;
    let count = 0;
    for (const c of t.columns) {
      const p = parentOf(t, c, tp.columns[c.name]);
      if (p && rank.has(p)) {
        sum += rank.get(p);
        count++;
      }
    }
    // A table with no parent in the previous column has nothing pulling it, so
    // it goes to the bottom rather than to an arbitrary middle.
    return count === 0 ? Number.MAX_SAFE_INTEGER : sum / count;
  };

  return [...names].sort((a, b) => {
    const wa = weight(a);
    const wb = weight(b);
    // Alphabetical among equals, so the same schema always lays out the same.
    return wa === wb ? a.localeCompare(b) : wa - wb;
  });
}

// autoLayout places the cards nobody has dragged yet.
//
// Columns are assigned from the foreign keys — a table sits one column right of
// the furthest table it depends on — and that part is not negotiable: parents on
// the left is what makes the arrows readable.
//
// The vertical position is where the work is. Stacking each column in order
// puts a table at whatever height its turn came up at, so two tables joined by
// a key can end up a screen apart with the line dragged across everything in
// between. Instead each card is pulled towards the average height of the tables
// it is joined to, over and over, with overlaps pushed apart after every pass.
// Related cards end up level with each other and next to each other, which is
// the arrangement someone would reach for by hand.
function autoLayout() {
  let offsetY = 0;

  for (const group of relatedGroups()) {
    const nodes = buildNodes(group);
    if (nodes.size === 0) continue;

    relax(nodes);

    // The group is lifted to sit under the one before it, so two unrelated
    // schemas read as two schemas.
    let top = Infinity;
    let bottom = -Infinity;
    for (const n of nodes.values()) {
      top = Math.min(top, n.y);
      bottom = Math.max(bottom, n.y + n.h);
    }
    for (const n of nodes.values()) {
      // Recomputed rather than kept, because the heights it was computed from
      // change every time a card is folded or unfolded.
      if (!n.fixed) {
        app.layout[n.name] = { x: n.x, y: Math.max(0, n.y - top + offsetY) };
      }
    }
    offsetY += bottom - top + GAP_Y * 3;
  }
}

// buildNodes turns one group of tables into the boxes the relaxation moves:
// a column from the dependency depth, a height measured off the card, and the
// neighbours that will pull on it.
function buildNodes(group) {
  const nodes = new Map();

  for (const name of group) {
    const card = cardOf(name);
    nodes.set(name, {
      name,
      depth: depthOf(name),
      x: 0,
      y: 0,
      h: card ? card.offsetHeight : 200,
      // A card someone has dragged, or a draft they dropped on the canvas, is
      // an anchor: the relaxation arranges the rest around it rather than
      // undoing the decision.
      fixed: !!app.pinned[name] || app.drafts.some((d) => d.table === name),
      links: new Set(),
    });
  }

  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp || !nodes.has(t.name)) continue;
    for (const c of t.columns) {
      const p = parentOf(t, c, tp.columns[c.name]);
      if (!p || p === t.name || !nodes.has(p)) continue;
      nodes.get(t.name).links.add(p);
      nodes.get(p).links.add(t.name);
    }
  }

  for (const n of nodes.values()) {
    n.x = n.depth * (CARD_W + GAP_X);
    if (n.fixed && app.layout[n.name]) {
      n.x = app.layout[n.name].x;
      n.y = app.layout[n.name].y;
    }
  }

  // A first pass in column order, so the relaxation starts from something
  // sensible rather than from every card at zero.
  for (const [, names] of columnsOf(nodes)) {
    let y = 0;
    for (const name of names) {
      const n = nodes.get(name);
      if (!n.fixed) n.y = y;
      y = n.y + n.h + GAP_Y;
    }
  }
  return nodes;
}

// columnsOf groups the nodes by column, each column ordered by the tables its
// members point at in the column before — the barycentre rule, which is what
// keeps the edges between two columns from crossing each other.
function columnsOf(nodes) {
  const columns = new Map();
  for (const n of nodes.values()) {
    if (!columns.has(n.depth)) columns.set(n.depth, []);
    columns.get(n.depth).push(n.name);
  }

  const depths = [...columns.keys()].sort((a, b) => a - b);
  let previous = [];
  const out = [];
  for (const d of depths) {
    const ordered = orderColumn(columns.get(d), previous);
    previous = ordered;
    out.push([d, ordered]);
  }
  return out;
}

// relax pulls each card towards the average height of everything it is joined
// to, then separates whatever that pushed into an overlap. Repeating the pair
// converges quickly: twenty rounds is well past the point where the arrangement
// stops changing on a schema of this size, and it is a few hundred numbers.
function relax(nodes) {
  const columns = columnsOf(nodes);

  for (let round = 0; round < 24; round++) {
    // Alternating direction, so a pull travels both up and down the graph
    // rather than only away from the roots.
    const order = round % 2 === 0 ? columns : [...columns].reverse();

    for (const [, names] of order) {
      for (const name of names) {
        const n = nodes.get(name);
        if (n.fixed || n.links.size === 0) continue;

        let sum = 0;
        let count = 0;
        for (const other of n.links) {
          const o = nodes.get(other);
          if (!o) continue;
          // Centres, not tops: two cards of different heights are level when
          // their middles are, which is also where the edges leave from.
          sum += o.y + o.h / 2;
          count++;
        }
        if (count === 0) continue;

        const want = sum / count - n.h / 2;
        // Moved part of the way rather than all of it. Jumping straight to the
        // average makes neighbours chase each other and the layout oscillate.
        n.y += (want - n.y) * 0.6;
      }

      separate(names.map((x) => nodes.get(x)));
    }
  }

  // One last separation pass per column, because the final pull can leave two
  // cards touching and an overlap is worse than an imperfect height.
  for (const [, names] of columns) separate(names.map((x) => nodes.get(x)));
}

// separate pushes a column's cards apart until none of them overlap, keeping
// the order they are in and moving them as little as possible.
function separate(column) {
  const items = [...column].sort((a, b) => a.y - b.y);

  // Downwards: every card sits at least a gap below the one above it.
  for (let i = 1; i < items.length; i++) {
    const above = items[i - 1];
    const here = items[i];
    const floor = above.y + above.h + GAP_Y;
    if (here.y < floor && !here.fixed) here.y = floor;
  }

  // Upwards: an anchored card can push the run below it down, which leaves a
  // hole above. This closes it without disturbing the order.
  for (let i = items.length - 2; i >= 0; i--) {
    const below = items[i + 1];
    const here = items[i];
    const ceiling = below.y - GAP_Y - here.h;
    if (here.y > ceiling && !here.fixed) here.y = ceiling;
  }
}

// placeCards writes the layout onto the DOM and sizes the canvas to fit, so the
// page scrolls to the furthest card rather than clipping it.
function placeCards() {
  autoLayout();
  let maxX = 0;
  let maxY = 0;
  const names = app.schema.tables.map((t) => t.name).concat(app.drafts.map((d) => d.table));
  for (const name of names) {
    const card = cardOf(name);
    const pos = app.layout[name];
    if (!card || !pos) continue;
    card.style.left = pos.x + "px";
    card.style.top = pos.y + "px";
    maxX = Math.max(maxX, pos.x + card.offsetWidth);
    maxY = Math.max(maxY, pos.y + card.offsetHeight);
  }
  const tables = $("tables");
  tables.style.width = maxX + "px";
  tables.style.height = maxY + "px";
  sizeCanvas();
}

const cardOf = (name) => $("tables").querySelector(`[data-table="${CSS.escape(name)}"]`);

// makeDraggable turns a card's header into a drag handle.
//
// Pointer events rather than mouse events, so a trackpad, a touchscreen, and a
// pen all work from one path. The pointer is captured on the header, which keeps
// a fast drag from escaping the element and stranding the card mid-move.
function makeDraggable(card, head, name) {
  let startX = 0, startY = 0, originX = 0, originY = 0, dragging = false;

  head.addEventListener("pointerdown", (e) => {
    if (e.button !== 0 || e.target.closest("input, button")) return;
    dragging = true;
    startX = e.clientX;
    startY = e.clientY;
    const pos = app.layout[name] || { x: card.offsetLeft, y: card.offsetTop };
    originX = pos.x;
    originY = pos.y;
    head.setPointerCapture(e.pointerId);
    card.classList.add("dragging");
    e.preventDefault();
  });

  head.addEventListener("pointermove", (e) => {
    if (!dragging) return;
    // Negative coordinates would put a card above or left of the canvas, where
    // there is no scrollbar to reach it.
    // Pointer deltas are screen pixels; the layout is in diagram coordinates,
    // so a zoomed-out card must not run away from the cursor.
    const x = Math.max(0, originX + fromScreen(e.clientX - startX));
    const y = Math.max(0, originY + fromScreen(e.clientY - startY));
    app.layout[name] = { x, y };
    app.pinned[name] = true;
    card.style.left = x + "px";
    card.style.top = y + "px";
    drawEdges();
  });

  const end = (e) => {
    if (!dragging) return;
    dragging = false;
    card.classList.remove("dragging");
    if (head.hasPointerCapture(e.pointerId)) head.releasePointerCapture(e.pointerId);
    saveLayout();
    placeCards();
    drawEdges();
  };
  head.addEventListener("pointerup", end);
  head.addEventListener("pointercancel", end);
}

function resetLayout() {
  rearrange(() => {
    app.layout = {};
    app.pinned = {};
    saveLayout();
    placeCards();
    drawEdges();
  });
  toast("Diagram laid out from the foreign keys");
}

// ---- animated rearrangement
//
// Fold, unfold, and tidy up all do the same thing to the canvas: they move
// cards and change their heights, by rebuilding the diagram from scratch. A
// rebuild is instantaneous, which reads as a jump cut.
//
// So the change is measured rather than tweened: positions are recorded, the
// rebuild happens, and every card is then animated from where it was to where
// it now is. One mechanism covers all three, and it stays correct however the
// layout is computed, because it never predicts the result — it reads it.

const reducedMotion = () =>
  window.matchMedia("(prefers-reduced-motion: reduce)").matches;

// snapshotCards records the layout box of every card. offset* rather than
// getBoundingClientRect, so the numbers are in diagram coordinates and a zoomed
// canvas does not scale the animation with it.
function snapshotCards() {
  const out = new Map();
  for (const card of $("tables").querySelectorAll(".table")) {
    out.set(card.dataset.table, {
      left: card.offsetLeft,
      top: card.offsetTop,
      height: card.offsetHeight,
    });
  }
  return out;
}

function rearrange(mutate) {
  if (reducedMotion()) {
    mutate();
    return;
  }

  const before = snapshotCards();
  mutate();

  // renderDiagram places the cards in its own animation frame; this one is
  // queued after it, so the layout being measured is the finished one.
  requestAnimationFrame(() => requestAnimationFrame(() => {
    let moving = false;
    for (const card of $("tables").querySelectorAll(".table")) {
      const was = before.get(card.dataset.table);
      if (!was) continue;

      const dx = was.left - card.offsetLeft;
      const dy = was.top - card.offsetTop;
      const dh = was.height - card.offsetHeight;
      if (!dx && !dy && !dh) continue;

      moving = true;
      if (dx || dy) {
        card.animate(
          [{ transform: `translate(${dx}px, ${dy}px)` }, { transform: "none" }],
          { duration: 620, easing: "cubic-bezier(0.16, 1, 0.3, 1)" },
        );
      }
      if (dh) {
        // The height is animated on the card itself rather than faked with a
        // scale, because a scaled card takes its text with it and the whole
        // point of folding is to read the name while it happens.
        const to = card.offsetHeight;
        const previous = card.style.overflow;
        card.style.overflow = "hidden";
        const run = card.animate(
          [{ height: `${was.height}px` }, { height: `${to}px` }],
          { duration: 520, easing: "cubic-bezier(0.16, 1, 0.3, 1)" },
        );
        run.finished.then(() => { card.style.overflow = previous; }, () => {});
      }
    }
    // The lines are redrawn every frame for the length of the move, so they
    // stay attached to the cards instead of snapping across at the end.
    if (moving) trackEdges(680);
    else drawEdges();
  }));
}

// trackEdges redraws the edges each frame for a fixed span, which is what keeps
// them fastened to cards that are animating. It is bounded by time rather than
// by an animation event because several animations of different lengths are
// running at once.
function trackEdges(ms) {
  const until = performance.now() + ms;
  const step = () => {
    drawEdges();
    if (performance.now() < until) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}

// ---- collapse

const collapseKey = () =>
  "seedora:collapsed:" + (app.state && app.state.target ? app.state.target : "?");

function loadCollapsed() {
  try {
    app.collapsed = JSON.parse(localStorage.getItem(collapseKey())) || {};
  } catch {
    app.collapsed = {};
  }
}

function saveCollapsed() {
  try {
    localStorage.setItem(collapseKey(), JSON.stringify(app.collapsed));
  } catch {
    // A full or disabled storage costs the remembered folds and nothing else.
  }
}

function toggleCollapse(name) {
  rearrange(() => {
    if (app.collapsed[name]) delete app.collapsed[name];
    else app.collapsed[name] = true;
    saveCollapsed();
    renderDiagram();
  });
}

function setAllCollapsed(on) {
  rearrange(() => {
    app.collapsed = {};
    if (on) for (const t of app.schema.tables) app.collapsed[t.name] = true;
    saveCollapsed();
    renderDiagram();
  });
}

bind("btn-fold-all", "click", () => {
  // The button does whichever of the two is not already true of most cards.
  const folded = Object.keys(app.collapsed).length;
  setAllCollapsed(folded < app.schema.tables.length);
});

// ---- cardinality
//
// A foreign key column only ever says "many of me point at one of you". Which
// of the four shapes that is depends on two more facts the catalog already
// carries: whether the child's key is unique, and whether the child is nothing
// but a pair of foreign keys.

// joinTables are the tables that exist only to pair two others: two foreign
// keys, and nothing else the user fills in. Both of their edges are drawn as
// many-to-many, because that is what the pair of them means.
function joinTables() {
  const out = new Set();
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;

    let fks = 0;
    let others = 0;
    for (const c of t.columns) {
      const cp = tp.columns[c.name];
      if (!cp) continue;
      // A key the database assigns itself is not payload; it is plumbing.
      if (cp.skip || cp.generator === "default") continue;
      if (parentOf(t, c, cp)) fks++;
      else others++;
    }
    if (fks === 2 && others === 0) out.add(t.name);
  }
  return out;
}

// cardinalityOf describes one edge from the parent's side first, the way the
// relationship is usually said out loud: one user has many orders.
function cardinalityOf(t, c, cp, joins) {
  const pk = t.primary_key || [];
  // A unique foreign key can only ever point at one parent row, and a key that
  // is the table's whole primary key is unique whether or not it is indexed as
  // such.
  const unique = !!c.unique || !!cp.unique || (pk.length === 1 && pk[0] === c.name);

  if (joins.has(t.name)) return { label: "M:N", ends: ["N", "M"] };
  if (unique) return { label: "1:1", ends: ["1", "1"] };
  // Written parent-first, the way it is said out loud: one user has many
  // orders. The end markers give the other reading, many orders to one user.
  return { label: "1:N", ends: ["N", "1"] };
}

// ---------------------------------------------------------------- focus
//
// Clicking a table lights it, everything it joins to, and the edges between.
// Twenty cards on a canvas is past the point where a line can be followed by
// eye, and the question being asked is nearly always "what is this table tied
// to" rather than "where does this one line go".

// neighboursOf returns every table joined to this one, in either direction.
// A foreign key is a two-way fact even though the column only exists on one
// side, and lighting only the children would answer half the question.
function neighboursOf(name) {
  const out = new Set();
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;
    for (const c of t.columns) {
      const parent = parentOf(t, c, tp.columns[c.name]);
      if (!parent) continue;
      if (t.name === name) out.add(parent);
      if (parent === name) out.add(t.name);
    }
  }
  out.delete(name);
  return out;
}

function focusTable(name) {
  // A second click on the same table clears it, so the canvas can be put back
  // to normal without hunting for an empty patch to click.
  app.focus = app.focus === name ? null : name;
  applyFocus();
  drawEdges();
}

function applyFocus() {
  const host = $("tables");
  host.classList.toggle("focusing", app.focus !== null);
  const near = app.focus ? neighboursOf(app.focus) : new Set();

  for (const card of host.querySelectorAll(".table")) {
    const name = card.dataset.table;
    card.classList.toggle("focused", name === app.focus);
    card.classList.toggle("linked", near.has(name));
  }
}

// litEdge reports whether an edge should be drawn as a live one. The selected
// column lights its own reference; a focused table lights every edge it is an
// end of.
function litEdge(child, parent) {
  if (app.focus && (child === app.focus || parent === app.focus)) return true;
  const sel = app.sel ? app.sel.table : null;
  return sel !== null && (child === sel || parent === sel);
}

// ---------------------------------------------------------------- history
//
// Neither Postgres nor SQLite records the DDL that shaped a database: the
// catalog says what the schema is, never how it got that way. So the history
// shown here comes from two places, and the difference is worth being explicit
// about — a migration tool's own bookkeeping table, and the changes Seedora
// applied itself, which no migration tool has any reason to know about.

const SOURCE_LABEL = {
  seedora: "this window",
  "golang-migrate": "golang-migrate",
  flyway: "Flyway",
  goose: "goose",
  alembic: "Alembic",
  atlas: "Atlas",
  django: "Django",
  knex: "Knex",
  rails: "Rails",
};

bind("btn-history", "click", async () => {
  const list = $("history-list");
  list.innerHTML = "";
  $("history-error").hidden = true;
  $("history-note").textContent = "Reading…";
  $("history-dialog").showModal();

  try {
    const res = await api("GET", "/api/history");
    renderHistory(res.entries || []);
  } catch (e) {
    $("history-note").textContent = "";
    const box = $("history-error");
    box.textContent = e.message;
    box.hidden = false;
  }
});

function renderHistory(entries) {
  const list = $("history-list");
  list.innerHTML = "";

  if (entries.length === 0) {
    $("history-note").textContent =
      "Nothing to show. A database keeps no record of its own schema changes, " +
      "so this is empty until a migration tool leaves one behind or a change is " +
      "applied from this window.";
    return;
  }

  const sources = [...new Set(entries.map((e) => e.source))]
    .map((s) => SOURCE_LABEL[s] || s);
  $("history-note").textContent =
    `${entries.length} entr${entries.length === 1 ? "y" : "ies"}, newest first · from ${sources.join(", ")}`;

  for (const e of entries) {
    const row = el("div", "history-item");

    const head = el("div", "history-head");
    head.appendChild(el("span", "history-source", SOURCE_LABEL[e.source] || e.source));
    head.appendChild(el("span", "history-version", e.version || ""));
    head.appendChild(el("div", "spacer"));
    // A tool that records no timestamp gets no timestamp. Inventing one, or
    // showing the row's position as if it were a date, would be a lie about
    // the only thing this dialog is for.
    head.appendChild(el("span", "history-when", e.applied_at ? when(e.applied_at) : "no date recorded"));
    row.appendChild(head);

    if (e.name) row.appendChild(el("div", "history-name", e.name));

    // A false `applied` means the tool marked it failed or dirty. An absent one
    // means the tool records no outcome, which is not the same as success.
    if (e.applied === false) {
      row.appendChild(el("div", "history-bad", "recorded as not applied"));
    }

    if (e.statements && e.statements.length) {
      const sql = el("details", "history-sql");
      const summary = el("summary", null,
        `${e.statements.length} statement${e.statements.length === 1 ? "" : "s"}`);
      sql.appendChild(summary);
      sql.appendChild(el("pre", "sql", e.statements.map((x) => x + ";").join("\n\n")));
      row.appendChild(sql);
    }

    list.appendChild(row);
  }
}

// when formats a timestamp as something readable at a glance, which for a
// history means relative for anything recent and a date for anything older.
function when(iso) {
  const at = new Date(iso);
  if (isNaN(at)) return iso;

  const seconds = (Date.now() - at.getTime()) / 1000;
  if (seconds < 60) return "just now";
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
  if (seconds < 7 * 86400) return `${Math.floor(seconds / 86400)} days ago`;
  return at.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

// ---------------------------------------------------------------- settings
//
// Display choices, kept in this browser. They change how the diagram is drawn
// and nothing about what a run writes, which is why they are not in
// seedora.yaml — that file is committed, and how someone likes their arrows
// drawn is not a property of the schema.

const SETTINGS_KEY = "seedora:settings";

const NOTATION = [
  { id: "crow", label: "Crow's foot",
    hint: "A fork at the many end and a bar at the one end — the notation most schema tools draw." },
  { id: "text", label: "Labels",
    hint: "The cardinality written on the line: 1:1, 1:N, M:N. Needs no legend." },
  { id: "none", label: "Plain lines",
    hint: "No markers. The diagram shows what is joined to what and nothing else." },
];

const FLOW_MODES = [
  { id: "click", label: "On click",
    hint: "A line animates while you follow it, and so does every line of a table you have lit up." },
  { id: "always", label: "Always", hint: "Every relationship animates. Busy, but it shows direction at a glance." },
  { id: "off", label: "Off", hint: "No animation." },
];

const settings = {
  notation: "crow",
  flow: "click",
};

function loadSettings() {
  let saved = {};
  try {
    saved = JSON.parse(localStorage.getItem(SETTINGS_KEY)) || {};
  } catch {
    saved = {};
  }
  if (NOTATION.some((n) => n.id === saved.notation)) settings.notation = saved.notation;
  if (FLOW_MODES.some((f) => f.id === saved.flow)) settings.flow = saved.flow;
}

function saveSettings() {
  try {
    localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings));
  } catch {
    // A full or disabled storage costs the remembered choice and nothing else.
  }
}

// choiceGroup renders one setting as a row of buttons rather than a <select>:
// there are three options, they are all worth seeing, and a dropdown would hide
// two of them behind a click.
function choiceGroup(host, options, current, onPick) {
  host.innerHTML = "";
  for (const opt of options) {
    const b = el("button", "choice-item" + (opt.id === current ? " on" : ""), opt.label);
    b.type = "button";
    b.role = "radio";
    b.setAttribute("aria-checked", opt.id === current ? "true" : "false");
    b.addEventListener("click", () => onPick(opt.id));
    host.appendChild(b);
  }
}

function renderSettings() {
  const host = $("notation-choice");
  if (!host) return;
  choiceGroup(host, NOTATION, settings.notation, (id) => {
    settings.notation = id;
    saveSettings();
    renderSettings();
    drawEdges();
  });
  $("notation-hint").textContent =
    (NOTATION.find((n) => n.id === settings.notation) || {}).hint || "";

  const flowHost = $("flow-choice");
  if (!flowHost) return;
  choiceGroup(flowHost, FLOW_MODES, settings.flow, (id) => {
    settings.flow = id;
    saveSettings();
    renderSettings();
    drawEdges();
  });
  $("flow-hint").textContent =
    (FLOW_MODES.find((f) => f.id === settings.flow) || {}).hint || "";
}

bind("btn-settings", "click", () => {
  renderSettings();
  $("settings-dialog").showModal();
});

// ---------------------------------------------------------------- zoom
//
// The diagram is scaled with one transform on its wrapper rather than by
// recomputing the layout: card positions, drag, and the edge geometry all stay
// in one coordinate system, and the browser does the scaling on the compositor.
//
// The cost is that every screen measurement has to be divided by the scale
// before it is used as a diagram coordinate, which is what fromScreen exists
// for. Getting that wrong is how edges end up detached from their cards.

const ZOOM_MIN = 0.3;
const ZOOM_MAX = 2;
const ZOOM_STEP = 0.1;

const zoomKey = () =>
  "seedora:zoom:" + (app.state && app.state.target ? app.state.target : "?");

// fromScreen converts a length in screen pixels to one in diagram coordinates.
const fromScreen = (px) => px / app.zoom;

function loadZoom() {
  const saved = parseFloat(localStorage.getItem(zoomKey()));
  app.zoom = clampZoom(isNaN(saved) ? 1 : saved);
  applyZoom();
}

const clampZoom = (z) => Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, z));

function applyZoom() {
  const diagram = $("diagram");
  diagram.style.transform = app.zoom === 1 ? "" : `scale(${app.zoom})`;
  const label = $("zoom-level");
  if (label) label.textContent = Math.round(app.zoom * 100) + "%";
  sizeCanvas();
}

// A transform scales what is painted, not the space it occupies, so the scroll
// area has to be sized by hand. Without this, zooming in puts the far cards
// past the end of the scrollbar, where they cannot be reached.
function sizeCanvas() {
  const tables = $("tables");
  const diagram = $("diagram");
  const pad = 2 * parseFloat(getComputedStyle(diagram).paddingLeft || 0);
  const w = tables.offsetWidth + pad;
  const h = tables.offsetHeight + pad;
  if (!w || !h) return;
  diagram.style.width = w * app.zoom + "px";
  diagram.style.height = h * app.zoom + "px";
}

// setZoom keeps a point on the canvas under the cursor while the scale changes.
// Zooming about the top-left instead would walk the diagram out from under
// whoever is reading it, which is the whole reason a zoom control feels bad.
function setZoom(next, anchor, glide = false) {
  const board = $("board");
  const diagram = $("diagram");
  diagram.classList.toggle("zooming", glide);
  const before = app.zoom;
  app.zoom = clampZoom(next);
  if (app.zoom === before) return;

  const rect = board.getBoundingClientRect();
  const ax = anchor ? anchor.x - rect.left : rect.width / 2;
  const ay = anchor ? anchor.y - rect.top : rect.height / 2;
  // Where the anchored point sits in diagram coordinates, before and after.
  const dx = (board.scrollLeft + ax) / before;
  const dy = (board.scrollTop + ay) / before;

  applyZoom();
  board.scrollLeft = dx * app.zoom - ax;
  board.scrollTop = dy * app.zoom - ay;

  try {
    localStorage.setItem(zoomKey(), String(app.zoom));
  } catch {
    // A full or disabled storage costs the remembered zoom and nothing else.
  }
  drawEdges();
}

// The rail, the keyboard, and Fit step; the wheel does not. A transition on a
// continuous gesture trails the pointer by a frame and reads as lag.
const zoomBy = (delta, anchor) => setZoom(app.zoom + delta, anchor, true);

// zoomToFit picks the scale that puts the whole diagram on screen. With twenty
// tables the useful first view is all of them, not the top-left corner of one.
function zoomToFit() {
  const board = $("board");
  const tables = $("tables");
  const w = tables.offsetWidth;
  const h = tables.offsetHeight;
  if (!w || !h) return;

  // The padding on .diagram is inside the scaled box, so it scales too; the
  // margin here is what keeps the outermost cards off the edge.
  const fit = Math.min((board.clientWidth - 48) / w, (board.clientHeight - 48) / h, 1);
  setZoom(fit, null, true);
  board.scrollLeft = 0;
  board.scrollTop = 0;
}

// Ctrl or ⌘ with the wheel is the zoom gesture every canvas uses, and it is
// also what a trackpad pinch reports. Without preventDefault the browser zooms
// the whole page instead, which leaves the cards the same size relative to the
// text and achieves nothing.
$("board").addEventListener("wheel", (e) => {
  if (!e.ctrlKey && !e.metaKey) return;
  e.preventDefault();

  // A wheel notch and a trackpad pinch report wildly different deltas — tens of
  // pixels against fractions of one — so the step is taken from the magnitude
  // and then capped. Using a fixed step per event makes a pinch, which fires
  // continuously, cross the whole zoom range in a flick.
  const step = Math.min(0.06, Math.abs(e.deltaY) * 0.0016);
  if (step === 0) return;
  // Scaling multiplicatively keeps a step the same size to the eye at 40% as
  // at 180%; adding a constant does not.
  const factor = e.deltaY < 0 ? 1 + step : 1 - step;
  setZoom(app.zoom * factor, { x: e.clientX, y: e.clientY });
}, { passive: false });

bind("btn-zoom-in", "click", () => zoomBy(ZOOM_STEP));
bind("btn-zoom-out", "click", () => zoomBy(-ZOOM_STEP));
bind("btn-zoom-fit", "click", zoomToFit);
bind("zoom-level", "click", () => setZoom(1, null, true));

// ---------------------------------------------------------------- edges

const SVG_NS = "http://www.w3.org/2000/svg";

// drawEdges links every foreign key to the column it points at. The lines are
// what make this a schema diagram rather than a list of tables.
function drawEdges() {
  const svg = $("edges");
  const host = $("diagram");
  svg.textContent = "";
  svg.classList.toggle("focusing", app.focus !== null);

  // The SVG lives inside the scaled wrapper, so its own coordinates are
  // unscaled: every measurement taken off the screen is divided back down.
  const base = host.getBoundingClientRect();
  const w = fromScreen(base.width);
  const h = fromScreen(base.height);
  svg.setAttribute("viewBox", `0 0 ${w} ${h}`);
  svg.setAttribute("width", w);
  svg.setAttribute("height", h);

  const joins = joinTables();
  const boxes = obstacles();

  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;
    for (const c of t.columns) {
      const cp = tp.columns[c.name];
      const parent = parentOf(t, c, cp);
      if (!parent) continue;

      const ref = cp.references || `${c.fk.table}.${c.fk.column}`;
      const from = document.getElementById(anchorId(t.name, c.name)) || headOf(t.name);
      const to = document.getElementById(anchorId(parent, ref.slice(ref.lastIndexOf(".") + 1)))
        || headOf(parent);
      if (!from || !to) continue;

      // An edge is identified by the column it leaves and the column it
      // arrives at, which is the only pair that is unique when one table
      // references another twice.
      const key = `${t.name}.${c.name}→${ref}`;
      // A focused table animates everything it is joined to, not just glows:
      // the question a click on a card asks is which way the data moves through
      // it, and a still line answers half of that.
      const touchesFocus = app.focus !== null &&
        (t.name === app.focus || parent === app.focus);
      const flowing = settings.flow === "always"
        ? true
        : settings.flow === "click" && (app.flow === key || touchesFocus);
      const highlight = flowing || litEdge(t.name, parent);
      // The edge takes the colour of the table it points at, so everything
      // feeding one table reads as a group.
      const draw = parent === t.name ? selfEdge : edge;
      for (const node of draw(from.getBoundingClientRect(), to.getBoundingClientRect(),
                              base, highlight, hueOf(parent),
                              { key, flowing, boxes, from: t.name, to: parent,
                                ...cardinalityOf(t, c, cp, joins) })) {
        svg.appendChild(node);
      }
    }
  }
}

// headOf is where an edge attaches when the column it belongs to is folded
// away. A relationship that vanishes when a card is folded would make folding
// cost the thing the diagram is for.
function headOf(name) {
  const card = cardOf(name);
  return card ? card.querySelector(".table-head") : null;
}

// ---- routing
//
// A line drawn straight from a child column to its parent crosses whatever
// happens to be between them, and on a schema this size that is usually three
// other cards. Passing under a card hides the line exactly where someone is
// trying to follow it.
//
// So a blocked line is routed around instead: the cards in its way are found,
// and the line is bent over or under the whole group of them, whichever side is
// nearer. It is not a general-purpose router — no grid, no search — because the
// cases that matter here are one detour deep, and a maze solver would move the
// lines around every time a card was dragged a pixel.

const CLEAR = 18; // how far a detour keeps from the cards it goes around

// A line can be dragged out of the way. The automatic route is right often
// enough to be the default and wrong often enough that overriding it has to be
// possible — and once someone has arranged a diagram, the arrangement is worth
// as much as the layout of the cards, so it is remembered the same way.
const waypointKey = () =>
  "seedora:edges:" + (app.state && app.state.target ? app.state.target : "?");

function loadWaypoints() {
  try {
    app.waypoints = JSON.parse(localStorage.getItem(waypointKey())) || {};
  } catch {
    app.waypoints = {};
  }
}

function saveWaypoints() {
  try {
    localStorage.setItem(waypointKey(), JSON.stringify(app.waypoints));
  } catch {
    // A full or disabled storage costs the arrangement and nothing else.
  }
}

function clearWaypoint(key) {
  delete app.waypoints[key];
  saveWaypoints();
  drawEdges();
}

// obstacles lists the card boxes in diagram coordinates, which is the space the
// edge geometry is in.
function obstacles() {
  const host = $("tables");
  const ox = host.offsetLeft;
  const oy = host.offsetTop;

  const out = [];
  for (const card of host.querySelectorAll(".table")) {
    out.push({
      table: card.dataset.table,
      left: ox + card.offsetLeft,
      top: oy + card.offsetTop,
      right: ox + card.offsetLeft + card.offsetWidth,
      bottom: oy + card.offsetTop + card.offsetHeight,
    });
  }
  return out;
}

// segmentHitsBox reports whether an axis-aligned segment passes through a card.
// Every segment a route is made of is horizontal or vertical, so this is four
// comparisons rather than a line-clipping algorithm.
function segmentHitsBox(x1, y1, x2, y2, b) {
  // A hair of inset, so a line running exactly along a card's edge counts as
  // touching it rather than crossing it.
  const left = b.left + 1;
  const right = b.right - 1;
  const top = b.top + 1;
  const bottom = b.bottom - 1;

  return Math.min(x1, x2) < right && Math.max(x1, x2) > left &&
    Math.min(y1, y2) < bottom && Math.max(y1, y2) > top;
}

// isClear reports whether a whole route misses every card except the two it
// belongs to. Those two are excluded because the route starts and ends on their
// edges, which would otherwise count as a collision with itself.
function isClear(points, boxes, from, to) {
  for (let i = 1; i < points.length; i++) {
    const a = points[i - 1];
    const c = points[i];
    for (const b of boxes) {
      if (b.table === from || b.table === to) continue;
      if (segmentHitsBox(a.x, a.y, c.x, c.y, b)) return false;
    }
  }
  return true;
}

// lanes are the vertical corridors between the cards — the gaps a line can run
// down without crossing anything. They are already there in the layout, because
// the cards are laid out in columns, and they are what someone routing a cable
// by hand would use.
function lanes(boxes, from, to) {
  const edges = [];
  for (const b of boxes) {
    if (b.table === from || b.table === to) continue;
    edges.push(b.left, b.right);
  }
  edges.sort((a, b) => a - b);

  const out = [];
  for (let i = 1; i < edges.length; i++) {
    if (edges[i] - edges[i - 1] > 40) out.push((edges[i - 1] + edges[i]) / 2);
  }
  return out;
}

// routeAround returns the path for one edge.
//
// Right angles, not curves: a diagonal across a canvas of rectangles has no
// relationship to anything else on it, while a line that leaves a card
// horizontally, turns, and arrives horizontally reads as a connection between
// two boxes. The corners are rounded so it does not look like wiring.
//
// Routes are proposed and then tested rather than computed. Whether a line
// crosses a card is a question with an exact answer, and a router that reasons
// about it instead of checking gets it right most of the time — which on a
// canvas of twenty-three cards means several lines under several cards.
function routeAround(x1, y1, x2, y2, boxes, from, to, key, out1 = 1, out2 = -1) {
  // A hand-placed waypoint wins over anything computed: it was placed to say
  // "not there", and recomputing over the top of it would be an argument.
  const held = app.waypoints[key];
  if (held) return throughWaypoint(x1, y1, x2, y2, held.x, held.y);

  return roundedPath(routePoints(x1, y1, x2, y2, boxes, from, to, out1, out2), 12);
}

// routePoints is routeAround's decision, before it becomes a path string.
//
// It is separate so a test can ask the question the router exists to answer —
// does this route cross a card — which cannot be read back out of the rounded
// path, since the corners no longer lie on the corners.
function routePoints(x1, y1, x2, y2, boxes, from, to, out1 = 1, out2 = -1) {
  const stub = 24;
  const ax = x1 + out1 * stub;   // out of the side the child was left by
  const bx = x2 + out2 * stub;   // into the side the parent is entered on
  const start = { x: x1, y: y1 };
  const finish = { x: x2, y: y2 };

  // The routes to try, best first.
  const options = [];

  // Level and unobstructed: one straight line, no turns at all.
  if (Math.abs(y1 - y2) < 2) options.push([start, finish]);

  // Down the middle, then down each corridor between the cards, nearest to the
  // middle first. When the parent is to the left, the corridors are what let
  // the line go back on itself without passing under anything.
  const middle = (ax + bx) / 2;
  const corridors = [middle, ...lanes(boxes, from, to)
    .sort((a, b) => Math.abs(a - middle) - Math.abs(b - middle))];

  for (const lane of corridors) {
    options.push([
      start,
      { x: ax, y: y1 },
      { x: lane, y: y1 },
      { x: lane, y: y2 },
      { x: bx, y: y2 },
      finish,
    ]);
  }

  // Failing all of those, over the top of everything or under the bottom of it.
  // One of these always works: there is nothing above the highest card.
  let top = Infinity;
  let bottom = -Infinity;
  for (const b of boxes) {
    if (b.table === from || b.table === to) continue;
    top = Math.min(top, b.top);
    bottom = Math.max(bottom, b.bottom);
  }
  if (top !== Infinity) {
    const overOrUnder = Math.abs((y1 + y2) / 2 - top) <= Math.abs((y1 + y2) / 2 - bottom)
      ? [top - CLEAR, bottom + CLEAR]
      : [bottom + CLEAR, top - CLEAR];
    for (const wayY of overOrUnder) {
      options.push([
        start,
        { x: ax, y: y1 },
        { x: ax, y: wayY },
        { x: bx, y: wayY },
        { x: bx, y: y2 },
        finish,
      ]);
    }
  }

  for (const points of options) {
    if (isClear(points, boxes, from, to)) return points;
  }
  // Everything was blocked, which means the cards are stacked on top of each
  // other. The last route is no worse than the rest.
  return options[options.length - 1] || [start, finish];
}

// throughWaypoint is the detour itself: out of each card, across at the
// waypoint's height, and in again.
function throughWaypoint(x1, y1, x2, y2, wx, wy) {
  const stub = 22;
  const ax = x1 + stub;
  const bx = x2 - stub;
  return roundedPath([
    { x: x1, y: y1 },
    { x: ax, y: y1 },
    { x: ax, y: wy },
    { x: wx, y: wy },
    { x: bx, y: wy },
    { x: bx, y: y2 },
    { x: x2, y: y2 },
  ], 12);
}

// roundedPath draws a polyline with each corner replaced by a quadratic of the
// given radius, shortened when a segment is too short to carry it.
function roundedPath(points, radius) {
  const pts = points.filter((p, i) =>
    i === 0 || Math.abs(p.x - points[i - 1].x) > 0.5 || Math.abs(p.y - points[i - 1].y) > 0.5);
  if (pts.length < 3) {
    return `M ${pts[0].x} ${pts[0].y} L ${pts[pts.length - 1].x} ${pts[pts.length - 1].y}`;
  }

  let d = `M ${pts[0].x} ${pts[0].y}`;
  for (let i = 1; i < pts.length - 1; i++) {
    const prev = pts[i - 1];
    const here = pts[i];
    const next = pts[i + 1];

    const r = Math.min(
      radius,
      dist(prev, here) / 2,
      dist(here, next) / 2,
    );
    const from = along(here, prev, r);
    const to = along(here, next, r);

    d += ` L ${from.x} ${from.y} Q ${here.x} ${here.y}, ${to.x} ${to.y}`;
  }
  const last = pts[pts.length - 1];
  return d + ` L ${last.x} ${last.y}`;
}

const dist = (a, b) => Math.hypot(b.x - a.x, b.y - a.y);

// along is the point r away from `from`, in the direction of `to`.
function along(from, to, r) {
  const len = dist(from, to) || 1;
  return {
    x: from.x + ((to.x - from.x) / len) * r,
    y: from.y + ((to.y - from.y) / len) * r,
  };
}

// selfEdge draws a table's reference to itself.
//
// The ordinary edge leaves the side of the child that faces the parent — with
// one card that is both sides at once, so the line wraps around the card and
// reads as two unrelated stubs. A self join gets a closed loop clear of the
// card's right edge instead: one shape, obviously beginning and ending on the
// same table.
// followEdge lights one relationship and runs the flow along it, so a single
// line can be traced through a canvas where a dozen of them cross. A second
// click on the same edge puts it back.
function followEdge(key) {
  app.flow = app.flow === key ? null : key;
  drawEdges();
}

// decorate turns a rendered path into something clickable and, when it is the
// one being followed, animated.
//
// The stroke of a 1.5px line is a hard target for a pointer, so a wide
// transparent copy of the same path sits underneath and takes the clicks.
// makeEdgeDraggable lets a line be pulled somewhere else. The point it is
// dragged to becomes the waypoint its route runs through.
//
// The same pointer does two things, told apart by distance: a press that does
// not travel is a click and opens the menu, a press that travels is a drag.
// Four pixels is the usual threshold and is well below what a hand does by
// accident.
function makeEdgeDraggable(hit, key) {
  let start = null;
  let moved = false;

  hit.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    start = { x: e.clientX, y: e.clientY };
    moved = false;
    hit.setPointerCapture(e.pointerId);
    e.stopPropagation();
    e.preventDefault();
  });

  hit.addEventListener("pointermove", (e) => {
    if (!start) return;
    if (!moved && Math.hypot(e.clientX - start.x, e.clientY - start.y) < 4) return;
    moved = true;

    // Screen to diagram coordinates: the same conversion the edge geometry
    // uses, or the line lands where the pointer is not.
    const base = $("diagram").getBoundingClientRect();
    app.waypoints[key] = {
      x: fromScreen(e.clientX - base.left),
      y: fromScreen(e.clientY - base.top),
    };
    drawEdges();
  });

  const finish = (e) => {
    if (!start) return;
    if (hit.hasPointerCapture(e.pointerId)) hit.releasePointerCapture(e.pointerId);
    start = null;
    if (moved) {
      saveWaypoints();
      return;
    }
    // A press that went nowhere: a line is a small target, so the click opens
    // the menu rather than spending the only gesture it has on one action.
    openContextMenu(e.clientX, e.clientY, edgeMenu(key));
  };
  hit.addEventListener("pointerup", finish);
  hit.addEventListener("pointercancel", finish);
}

// labelAt writes the cardinality on the line. A label rather than a crow's
// foot: the notation needs a legend, the words do not, and this diagram is read
// by people who are about to seed the tables rather than draw them.
function labelAt(x, y, text, lit) {
  const label = document.createElementNS(SVG_NS, "text");
  label.setAttribute("class", "edge-label" + (lit ? " lit" : ""));
  label.setAttribute("x", x);
  label.setAttribute("y", y);
  label.setAttribute("text-anchor", "middle");
  label.setAttribute("dominant-baseline", "central");
  label.textContent = text;
  return label;
}

// midOf is the point halfway along a cubic, which is where a label sits without
// landing on either card.
const midOf = (p0, p1, p2, p3) => (p0 + 3 * p1 + 3 * p2 + p3) / 8;

// midpointOf asks the path itself where its middle is. An SVG path element
// answers that even before it is in the document, which is cheaper and more
// honest than re-deriving the curve the router just built.
function midpointOf(path, x1, y1, x2, y2) {
  try {
    const len = path.getTotalLength();
    if (len > 0) return path.getPointAtLength(len / 2);
  } catch {
    // Not every environment implements path measurement; the straight midpoint
    // is close enough to keep the label on the line.
  }
  return { x: (x1 + x2) / 2, y: (y1 + y2) / 2 };
}

// marks draws whichever end notation is switched on. The geometry is the same
// either way — a point, a direction away from the card, and whether that end is
// the many side — so the two notations are one branch rather than two drawing
// paths that can drift apart.
function marks(a, b, opts) {
  if (settings.notation === "none") return [];

  if (settings.notation === "text") {
    return [
      labelAt(a.x + a.dir * 13, a.y - 9, opts.ends[0], opts.lit),
      labelAt(b.x + b.dir * 13, b.y - 9, opts.ends[1], opts.lit),
      // The middle label is the relationship itself — 1:N, 1:1, M:N — so it is
      // also the control that changes it.
      clickable(labelAt(opts.mx, opts.my, opts.label, opts.lit), opts.key),
    ];
  }
  return [clickable(foot(a, opts), opts.key), clickable(foot(b, opts), opts.key)];
}

// clickable turns a cardinality marker into the control for the relationship it
// describes: clicking 1:N asks what it should be instead.
//
// The marker is the obvious thing to press — it is the picture of the answer —
// and the alternative was a menu item nobody would find. The line's own menu
// still carries it, for the notation that draws no markers at all.
function clickable(node, key) {
  if (!key) return node;
  node.classList.add("edge-mark");
  node.addEventListener("click", (e) => {
    e.stopPropagation();
    changeCardinality(key);
  });
  return node;
}

// foot is one crow's-foot terminator: a three-pronged fork for many, a single
// bar for one. It is drawn away from the card so it does not sit under the
// card's own border.
function foot(end, opts) {
  const { x, y, dir, many } = end;
  const reach = 11;
  const spread = 4.5;

  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("class", "edge-foot" + (opts.lit ? " lit" : ""));
  path.setAttribute("stroke", `var(--h${opts.hue})`);
  path.setAttribute("d", many
    // Three prongs from a point out along the line back to the card's edge,
    // which is the fork that reads as "many of these".
    ? `M ${x + dir * reach} ${y} L ${x} ${y - spread} ` +
      `M ${x + dir * reach} ${y} L ${x} ${y} ` +
      `M ${x + dir * reach} ${y} L ${x} ${y + spread}`
    // A single bar across the line: exactly one.
    : `M ${x + dir * 7} ${y - spread} L ${x + dir * 7} ${y + spread}`);
  return path;
}

function decorate(path, d, opts) {
  const nodes = [];

  const hit = document.createElementNS(SVG_NS, "path");
  hit.setAttribute("class", "edge-hit");
  hit.setAttribute("d", d);
  hit.dataset.edge = opts.key;
  makeEdgeDraggable(hit, opts.key);
  nodes.push(hit);
  nodes.push(path);

  if (!opts.flowing) return nodes;

  path.classList.add("flow");
  // The travelling dot is what makes the direction readable: the dash pattern
  // alone says "live", not "this way".
  if (!window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    const comet = document.createElementNS(SVG_NS, "circle");
    comet.setAttribute("class", "edge-comet");
    comet.setAttribute("r", 3);
    comet.setAttribute("fill", path.getAttribute("stroke"));

    const motion = document.createElementNS(SVG_NS, "animateMotion");
    motion.setAttribute("dur", "2.6s");
    motion.setAttribute("repeatCount", "indefinite");
    motion.setAttribute("path", d);
    comet.appendChild(motion);
    nodes.push(comet);
  }
  return nodes;
}

function selfEdge(a, b, base, lit, hue, opts) {
  const x = fromScreen(Math.max(a.right, b.right) - base.left);
  const y1 = fromScreen(a.top + a.height / 2 - base.top);
  const y2 = fromScreen(b.top + b.height / 2 - base.top);

  // The loop widens with the gap between the two rows, so a reference across a
  // tall card does not collapse into a flat line against its edge.
  const out = 22 + Math.min(26, Math.abs(y2 - y1) * 0.4);

  const d = roundedPath([
    { x, y: y1 },
    { x: x + out, y: y1 },
    { x: x + out, y: y2 },
    { x, y: y2 },
  ], 10);

  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("class", "edge self" + (lit ? " lit" : ""));
  path.setAttribute("stroke", `var(--h${hue})`);
  path.setAttribute("d", d);

  const dot = document.createElementNS(SVG_NS, "circle");
  dot.setAttribute("class", "edge-dot");
  dot.setAttribute("fill", `var(--h${hue})`);
  dot.setAttribute("cx", x);
  dot.setAttribute("cy", y2);
  dot.setAttribute("r", lit ? 3.5 : 2.5);

  const mx = midOf(x, x + out, x + out, x);
  const my = midOf(y1, y1, y2, y2);

  const nodes = [...decorate(path, d, opts), dot];
  nodes.push(...marks(
    { x, y: y1, dir: 1, many: opts.ends[0] !== "1" },
    { x, y: y2, dir: 1, many: opts.ends[1] !== "1" },
    { mx, my, hue, lit, ends: opts.ends, label: opts.label, key: opts.key },
  ));
  return nodes;
}

function edge(a, b, base, lit, hue, opts) {
  // Out of the child and into the parent on the sides that face each other.
  //
  // This used to be fixed — always the child's right into the parent's left —
  // so that every arrow meant the same thing without being read twice. It cost
  // a doubling-back whenever the parent sat to the left, and that route runs
  // back underneath both cards, where the edge layer sits below them: what is
  // left visible is a stub at each end and a stray segment in the gap, which
  // reads as a broken line rather than a long one. Direction is already said
  // twice over — by the dot at the parent's end and by the crow's foot — so the
  // sides are free to be chosen for legibility.
  const flip = (b.left + b.right) / 2 < (a.left + a.right) / 2;

  const x1 = fromScreen((flip ? a.left : a.right) - base.left);
  const y1 = fromScreen(a.top + a.height / 2 - base.top);
  const x2 = fromScreen((flip ? b.right : b.left) - base.left);
  const y2 = fromScreen(b.top + b.height / 2 - base.top);
  // Which way each stub leaves its card: away from it, whichever side it left.
  const out1 = flip ? -1 : 1;
  const out2 = -out1;
  const d = routeAround(x1, y1, x2, y2, opts.boxes, opts.from, opts.to, opts.key, out1, out2);

  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("class", "edge" + (lit ? " lit" : ""));
  path.setAttribute("stroke", `var(--h${hue})`);
  path.setAttribute("d", d);

  // A dot at the parent end says which way the reference points, without the
  // visual weight of an arrowhead on every line.
  const dot = document.createElementNS(SVG_NS, "circle");
  dot.setAttribute("class", "edge-dot");
  dot.setAttribute("fill", `var(--h${hue})`);
  dot.setAttribute("cx", x2);
  dot.setAttribute("cy", y2);
  dot.setAttribute("r", lit ? 3.5 : 2.5);

  // Halfway along the path as drawn, not along the line it would have taken:
  // a routed edge's label belongs on the detour, not floating over the card the
  // detour went around.
  const mid = midpointOf(path, x1, y1, x2, y2);
  const mx = mid.x;
  const my = mid.y;

  // Which end is which side of the relationship: the child's end carries the
  // many, the parent's the one. Drawn once, read both ways.
  const out = [...decorate(path, d, opts), dot];
  out.push(...marks(
    // The markers point away from the card they sit on, whichever side the
    // line left it by.
    { x: x1, y: y1, dir: out1, many: opts.ends[0] !== "1" },
    { x: x2, y: y2, dir: out2, many: opts.ends[1] !== "1" },
    { mx, my, hue, lit, ends: opts.ends, label: opts.label, key: opts.key },
  ));
  return out;
}

let resizeTimer;
window.addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => app.schema && drawEdges(), 80);
});

// ---------------------------------------------------------------- inspector

function select(table, column) {
  app.sel = { table, column };
  for (const n of document.querySelectorAll(".col.selected")) n.classList.remove("selected");
  const row = document.getElementById(anchorId(table, column));
  if (row) row.classList.add("selected");
  renderInspector();
  drawEdges();
}

function closeInspector() {
  app.sel = null;
  $("inspector").hidden = true;
  $("preview").classList.remove("with-inspector");
  for (const n of document.querySelectorAll(".col.selected")) n.classList.remove("selected");
  drawEdges();
}

function current() {
  if (!app.sel || !app.schema) return null;
  const t = app.schema.tables.find((x) => x.name === app.sel.table);
  const c = t && t.columns.find((x) => x.name === app.sel.column);
  const tp = app.plan.tables[app.sel.table];
  const cp = tp && tp.columns[app.sel.column];
  return t && c && cp ? { t, c, cp } : null;
}

function renderInspector() {
  const cur = current();
  const panel = $("inspector");
  if (!cur) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;
  $("preview").classList.add("with-inspector");

  const { t, c, cp } = cur;
  $("insp-table").textContent = t.name;
  $("insp-column").textContent = c.name;

  const tags = $("insp-tags");
  tags.innerHTML = "";
  tags.appendChild(el("span", "tag", c.native || c.type));
  tags.appendChild(el("span", "tag", c.nullable ? "nullable" : "not null"));
  if ((t.primary_key || []).includes(c.name)) tags.appendChild(el("span", "tag key", "primary key"));
  if (c.unique) tags.appendChild(el("span", "tag", "unique"));
  if (c.fk) tags.appendChild(el("span", "tag fk", `→ ${c.fk.table}.${c.fk.column}`));
  if (c.has_default) tags.appendChild(el("span", "tag", "has default"));

  const why = $("insp-why");
  why.textContent = cp.why || "";
  why.className = "note" + (cp.confidence === "low" ? " guess" : "");
  why.hidden = !cp.why;

  renderPicker(c, cp);
  renderOptions(c, cp);

  $("opt-unique").checked = !!cp.unique;
  $("opt-skip").checked = !!cp.skip;
  $("null-field").hidden = !c.nullable;
  const pct = Math.round((cp.null_rate || 0) * 100);
  $("opt-null").value = pct;
  $("null-value").textContent = pct + "%";
}

// ---------------------------------------------------------------- picker
//
// A listbox built by hand rather than a native <select>. Native gives no way to
// group, badge, or explain an option, and this is the one decision the page
// exists to support: it has to be searchable and arrow-key navigable.

function renderPicker(c, cp) {
  const list = $("gen-list");
  const filter = $("gen-filter").value.trim().toLowerCase();
  const cls = classOf(c.type);

  const fits = (g) => !g.classes || g.classes.includes(cls);
  const matches = (g) =>
    !filter || g.label.toLowerCase().includes(filter) || g.id.includes(filter);

  const found = app.generators.filter(matches);
  // Type-appropriate first, everything else after. Nothing is hidden: a
  // deliberate mismatch is a legitimate choice, not a mistake to prevent.
  const ordered = [...found.filter(fits), ...found.filter((g) => !fits(g))];

  app.picker = ordered;
  app.pickerAt = ordered.findIndex((g) => g.id === cp.generator);

  list.innerHTML = "";
  if (ordered.length === 0) {
    list.appendChild(el("div", "picker-empty", "No generator matches that."));
    return;
  }

  // Grouped and folded. The catalogue is long enough that scrolling past four
  // groups to reach the fifth is the normal case, and a filter only helps
  // someone who already knows the name of what they want.
  const counts = new Map();
  for (const g of ordered) counts.set(groupLabel(g, fits), (counts.get(groupLabel(g, fits)) || 0) + 1);

  let group = null;
  let folded = false;
  for (let i = 0; i < ordered.length; i++) {
    const g = ordered[i];
    const label = groupLabel(g, fits);
    if (label !== group) {
      group = label;
      // A search is a request to see everything that matches, so a filtered
      // list is never folded.
      folded = !filter && isFolded(label, g.id === cp.generator);
      list.appendChild(groupHeader(label, counts.get(label), folded, c, cp));
    }
    if (folded) continue;

    const opt = el("button", "picker-option");
    opt.type = "button";
    opt.setAttribute("role", "option");
    opt.setAttribute("aria-selected", g.id === cp.generator ? "true" : "false");
    opt.dataset.index = i;
    opt.append(el("span", null, g.label), el("span", "id", g.id));
    opt.addEventListener("click", () => chooseGenerator(g.id));
    list.appendChild(opt);
  }

  // A generator a hand-written config named but the catalogue does not list must
  // still be selectable, or opening the inspector would silently change it.
  if (!app.byId.has(cp.generator)) {
    const opt = el("button", "picker-option");
    opt.type = "button";
    opt.setAttribute("role", "option");
    opt.setAttribute("aria-selected", "true");
    opt.append(el("span", null, cp.generator), el("span", "id", "custom"));
    list.insertBefore(opt, list.firstChild);
  }

  scrollPickerIntoView();
}

// Which groups are folded is remembered, because the answer is a habit: the
// person who never uses the finance generators never uses them.
const groupLabel = (g, fits) => (fits(g) ? "" : "other · ") + g.group;

const FOLD_KEY = "seedora:picker-folded";
let foldedGroups = null;

function loadFolded() {
  if (foldedGroups) return foldedGroups;
  try {
    foldedGroups = new Set(JSON.parse(localStorage.getItem(FOLD_KEY)) || []);
  } catch {
    foldedGroups = new Set();
  }
  return foldedGroups;
}

// isFolded keeps a group open when it holds the generator that is currently
// chosen: folding away the answer to "what is this column set to" would be
// worse than the scrolling it saves.
function isFolded(label, holdsCurrent) {
  return !holdsCurrent && loadFolded().has(label);
}

function toggleGroup(label, c, cp) {
  const set = loadFolded();
  if (set.has(label)) set.delete(label);
  else set.add(label);
  try {
    localStorage.setItem(FOLD_KEY, JSON.stringify([...set]));
  } catch {
    // A full or disabled storage costs the remembered folds and nothing else.
  }
  renderPicker(c, cp);
}

function groupHeader(label, count, folded, c, cp) {
  const head = el("button", "picker-group" + (folded ? " folded" : ""));
  head.type = "button";
  head.setAttribute("aria-expanded", folded ? "false" : "true");
  head.append(
    el("span", "picker-caret", folded ? "▸" : "▾"),
    el("span", "picker-group-name", label),
    el("span", "picker-group-count", String(count || 0)),
  );
  head.addEventListener("click", () => toggleGroup(label, c, cp));
  return head;
}

function scrollPickerIntoView() {
  const active = $("gen-list").querySelector('[aria-selected="true"], .active');
  if (active) active.scrollIntoView({ block: "nearest" });
}

function movePicker(delta) {
  if (!app.picker.length) return;
  const next = Math.min(app.picker.length - 1, Math.max(0, app.pickerAt + delta));
  app.pickerAt = next;
  const list = $("gen-list");
  for (const n of list.querySelectorAll(".picker-option.active")) n.classList.remove("active");
  const opt = list.querySelector(`[data-index="${next}"]`);
  if (opt) {
    opt.classList.add("active");
    opt.scrollIntoView({ block: "nearest" });
  }
}

function chooseGenerator(id) {
  const cur = current();
  if (!cur) return;
  cur.cp.generator = id;
  // Options belonging to the previous generator would be carried over and mean
  // something different, so they go.
  for (const k of ["min", "max", "values", "weights", "true_weight", "const",
                   "references", "pattern"]) {
    delete cur.cp[k];
  }
  if (id === "foreign_key" && cur.c.fk) {
    cur.cp.references = `${cur.c.fk.table}.${cur.c.fk.column}`;
  }
  cur.cp.skip = id === "default";
  commit();
}

$("gen-filter").addEventListener("input", () => {
  const cur = current();
  if (cur) renderPicker(cur.c, cur.cp);
});

$("gen-filter").addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { e.preventDefault(); movePicker(1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); movePicker(-1); }
  else if (e.key === "Enter") {
    e.preventDefault();
    const g = app.picker[app.pickerAt];
    if (g) chooseGenerator(g.id);
  }
});

// classOf mirrors the server's type classification closely enough to order the
// picker. The server is authoritative; being wrong here only changes ordering.
function classOf(type) {
  const t = String(type).toLowerCase().replace(/\(.*/, "");
  if (/uuid|uniqueidentifier/.test(t)) return "uuid";
  if (/bool|bit/.test(t)) return "bool";
  if (/int|serial|year/.test(t)) return "int";
  if (/real|double|float|numeric|decimal|money/.test(t)) return "float";
  if (/date|time/.test(t)) return "time";
  if (/json/.test(t)) return "json";
  if (/bytea|blob|binary|image/.test(t)) return "bytes";
  if (/char|text|clob|xml|name/.test(t)) return "string";
  return "unknown";
}

// ---------------------------------------------------------------- options

// renderOptions shows only the inputs the chosen generator actually reads.
function renderOptions(c, cp) {
  const host = $("opts");
  host.innerHTML = "";
  const g = app.byId.get(cp.generator);
  const opts = new Set((g && g.options) || []);

  if (opts.has("references")) {
    host.appendChild(textField("References", cp.references || "", "table.column",
      (v) => { cp.references = v; commit(); }));
  }
  if (opts.has("const")) {
    host.appendChild(textField("Value", cp.const == null ? "" : String(cp.const),
      "written to every row", (v) => { cp.const = v; commit(); }));
  }
  if (opts.has("min") || opts.has("max")) {
    const grid = el("div", "field-row");
    if (opts.has("min")) grid.appendChild(textField("Min", cp.min ?? "", "", (v) => { cp.min = num(v); commit(); }));
    if (opts.has("max")) grid.appendChild(textField("Max", cp.max ?? "", "", (v) => { cp.max = num(v); commit(); }));
    host.appendChild(grid);
  }
  if (opts.has("values")) {
    host.appendChild(textField("Values", (cp.values || []).join(", "), "comma separated",
      (v) => {
        cp.values = v.split(",").map((s) => s.trim()).filter(Boolean);
        commit();
      }));
  }
  if (opts.has("weights")) {
    host.appendChild(textField("Weights", (cp.weights || []).join(", "), "one per value",
      (v) => {
        const w = v.split(",").map((s) => parseFloat(s.trim())).filter((n) => !isNaN(n));
        cp.weights = w.length ? w : undefined;
        commit();
      }));
  }
  if (opts.has("true_weight")) {
    const pct = Math.round((cp.true_weight ?? 0.5) * 100);
    const field = el("div", "field");
    field.appendChild(el("span", "field-label", "Share true"));
    const row = el("div", "slider-row");
    const range = el("input");
    range.type = "range";
    range.min = "0";
    range.max = "100";
    range.value = pct;
    const out = el("span", "slider-value", pct + "%");
    range.addEventListener("input", () => (out.textContent = range.value + "%"));
    range.addEventListener("change", () => {
      cp.true_weight = parseInt(range.value, 10) / 100;
      commit();
    });
    row.append(range, out);
    field.appendChild(row);
    host.appendChild(field);
  }
  if (opts.has("locale")) {
    host.appendChild(localeField(cp));
  }
}

// LOCALES are the ones Synth ships. A list rather than a text field: a locale
// typed from memory is either right or silently ignored, and there is no way to
// tell which from looking at the field.
const LOCALES = [
  ["", "Follow the run's locale"],
  ["en_US", "English — United States"],
  ["en_GB", "English — United Kingdom"],
  ["uz_UZ", "Uzbek — Uzbekistan"],
  ["ru_RU", "Russian — Russia"],
  ["de_DE", "German — Germany"],
  ["fr_FR", "French — France"],
  ["es_ES", "Spanish — Spain"],
  ["it_IT", "Italian — Italy"],
  ["pt_BR", "Portuguese — Brazil"],
  ["tr_TR", "Turkish — Türkiye"],
  ["pl_PL", "Polish — Poland"],
  ["nl_NL", "Dutch — Netherlands"],
  ["ja_JP", "Japanese — Japan"],
  ["zh_CN", "Chinese — China"],
  ["ko_KR", "Korean — Korea"],
  ["ar_SA", "Arabic — Saudi Arabia"],
  ["hi_IN", "Hindi — India"],
];

function localeField(cp) {
  const field = el("div", "field");
  field.appendChild(el("span", "field-label", "Locale"));

  const select = el("select", "select");
  select.setAttribute("aria-label", "Locale for this column");

  const current = cp.locale || "";
  let known = false;
  for (const [id, label] of LOCALES) {
    const opt = el("option", null, id ? `${label} · ${id}` : label);
    opt.value = id;
    select.appendChild(opt);
    if (id === current) known = true;
  }
  // A locale from a hand-written config that this list does not carry stays
  // selectable, or opening the inspector would quietly change it.
  if (current && !known) {
    const opt = el("option", null, current + " · from the config");
    opt.value = current;
    select.appendChild(opt);
  }
  select.value = current;
  select.addEventListener("change", () => {
    cp.locale = select.value || undefined;
    commit();
  });

  field.appendChild(select);
  field.appendChild(el("p", "field-hint",
    "Only this column. The run's locale covers everything else."));
  return field;
}

function textField(label, value, placeholder, onChange) {
  const wrap = el("label", "field");
  wrap.appendChild(el("span", "field-label", label));
  const input = el("input");
  input.type = "text";
  input.value = value === null || value === undefined ? "" : value;
  input.placeholder = placeholder || "";
  input.addEventListener("change", () => onChange(input.value));
  wrap.appendChild(input);
  return wrap;
}

const num = (v) => (v === "" ? undefined : isNaN(Number(v)) ? v : Number(v));

// commit marks the column as a human decision, so a re-scan never overwrites it,
// then pushes the plan.
function commit() {
  const cur = current();
  if (cur) cur.cp.confidence = "manual";
  pushPlan(cur ? `${cur.column} settings` : "column settings");
}

$("opt-unique").addEventListener("change", (e) => {
  const cur = current();
  if (cur) { cur.cp.unique = e.target.checked; commit(); }
});
$("opt-skip").addEventListener("change", (e) => {
  const cur = current();
  if (cur) { cur.cp.skip = e.target.checked; commit(); }
});
$("opt-null").addEventListener("input", (e) => {
  $("null-value").textContent = e.target.value + "%";
});
$("opt-null").addEventListener("change", (e) => {
  const cur = current();
  if (cur) { cur.cp.null_rate = parseInt(e.target.value, 10) / 100; commit(); }
});
$("insp-close").addEventListener("click", closeInspector);

// ---------------------------------------------------------------- preview

let previewing = null;

async function previewTable(table, nonce = 0) {
  previewing = table;
  busy(true, "Generating a preview…");
  try {
    const res = await api("POST", "/api/preview", { table, rows: 10, nonce });
    renderPreview(table, res.columns || [], res.rows || []);
  } catch (e) {
    toast(e.message, "bad");
  } finally {
    busy(false);
  }
}

function renderPreview(table, cols, rows) {
  $("preview").hidden = false;
  $("preview-title").textContent = table;
  $("preview-note").textContent = `${rows.length} sample rows · nothing written`;

  const t = $("preview-table");
  t.innerHTML = "";

  const head = t.createTHead().insertRow();
  for (const c of cols) {
    const th = document.createElement("th");
    th.textContent = c;
    head.appendChild(th);
  }

  const body = t.createTBody();
  for (const r of rows) {
    const tr = body.insertRow();
    for (const c of cols) {
      const td = tr.insertCell();
      const v = r[c];
      if (v === null || v === undefined) {
        td.textContent = "NULL";
        td.className = "null";
      } else {
        td.textContent = typeof v === "object" ? JSON.stringify(v) : String(v);
      }
    }
  }
}

// Regenerate asks for a different draw; without a nonce the fixed preview seed
// would hand back the exact same rows and the button would look dead.
$("preview-refresh").addEventListener("click", () => {
  if (previewing) previewTable(previewing, Date.now());
});
$("preview-close").addEventListener("click", () => ($("preview").hidden = true));


// ---------------------------------------------------------- context menu
//
// Right-clicking the canvas opens the application's menu rather than the
// browser's, which over a diagram has nothing to offer. Edges also open it on
// an ordinary click: a line has no other left-click meaning, and it is a hard
// enough target that making it do two things would waste the hit.

const ctxMenu = $("ctx-menu");

function closeContextMenu() {
  ctxMenu.hidden = true;
  ctxMenu.innerHTML = "";
}

// openContextMenu places the menu at the pointer and nudges it back inside the
// window if it would hang off an edge.
function openContextMenu(x, y, items) {
  ctxMenu.innerHTML = "";
  for (const item of items) {
    if (item === "-") {
      ctxMenu.appendChild(el("div", "ctx-rule"));
      continue;
    }
    const b = el("button", "ctx-item" + (item.danger ? " danger" : ""), item.label);
    b.type = "button";
    b.role = "menuitem";
    b.addEventListener("click", () => {
      closeContextMenu();
      item.run();
    });
    ctxMenu.appendChild(b);
  }
  ctxMenu.hidden = false;

  const box = ctxMenu.getBoundingClientRect();
  const left = Math.min(x, window.innerWidth - box.width - 8);
  const top = Math.min(y, window.innerHeight - box.height - 8);
  ctxMenu.style.left = Math.max(8, left) + "px";
  ctxMenu.style.top = Math.max(8, top) + "px";
  ctxMenu.querySelector(".ctx-item")?.focus();
}

// The three menus, one per thing that can be under the pointer.

// changeCardinality asks what a relationship that already exists should be, and
// makes it that.
//
// One-to-many and one-to-one differ by one fact: whether the child's key may
// repeat. That is a mapping change first — it decides whether the seeder gives
// each parent one child or many — and a schema change only if the user wants
// the database to enforce it, which is the second question.
//
// Many-to-many is not offered here. It is not a property of this edge: it is a
// third table, and turning one relationship into one is a different gesture —
// drag the key onto the other table's card.
async function changeCardinality(key) {
  const [childRef, parentRef] = key.split("→");
  const childTable = childRef.slice(0, childRef.lastIndexOf("."));
  const childColumn = childRef.slice(childRef.lastIndexOf(".") + 1);
  const parentTable = parentRef.slice(0, parentRef.lastIndexOf("."));
  const parentColumn = parentRef.slice(parentRef.lastIndexOf(".") + 1);

  const tp = app.plan.tables[childTable];
  const cp = tp && tp.columns[childColumn];
  if (!cp) return;

  const child = app.schema.tables.find((t) => t.name === childTable);
  const col = child && child.columns.find((c) => c.name === childColumn);
  const pk = (child && child.primary_key) || [];
  // A key the database already constrains cannot be loosened from here: that
  // means dropping an index this tool did not create and cannot name.
  const fixed = !!(col && col.unique) || (pk.length === 1 && pk[0] === childColumn);

  const now = cp.unique || fixed ? "one" : "many";
  const kind = await ask({
    title: `${parentTable} → ${childTable}`,
    body: `Every ${singular(childTable)} points at one ${singular(parentTable)}. ` +
      `The question is how many ${childTable} may point at the same one.`,
    okLabel: "Apply",
    choices: [
      {
        value: "many",
        label: `One ${singular(parentTable)} — many ${childTable}`,
        hint: fixed
          ? `The database enforces uniqueness on ${childColumn}; drop that index to allow it.`
          : `${childColumn} repeats. Rows are spread over the parents.`,
      },
      {
        value: "one",
        label: `One ${singular(parentTable)} — one ${singular(childTable)}`,
        hint: `${childColumn} is used once per parent. Seeding is capped at the number of ${parentTable}.`,
      },
    ],
  });
  if (!kind || kind === now) return;
  if (kind === "many" && fixed) {
    toast(`${childTable}.${childColumn} is unique in the database — that is what makes it one-to-one`, "bad");
    return;
  }

  cp.unique = kind === "one";
  cp.confidence = "manual";
  cp.why = kind === "one"
    ? `one-to-one — each ${singular(parentTable)} is used once`
    : `foreign key to ${parentTable}.${parentColumn}`;
  await pushPlan(`${parentTable} → ${childTable} cardinality`);
  renderDiagram();
  toast(`${parentTable} → ${childTable} is now ` +
    (kind === "one" ? "one-to-one" : "one-to-many"), "good");

  // The mapping now says one-to-one; the database still allows two. Offering
  // the index is the same offer made when a relationship is first drawn, and
  // it goes through the SQL dialog like everything else.
  if (kind === "one" && !fixed) await offerUniqueIndex(childTable, childColumn);
}

async function offerUniqueIndex(table, column) {
  const t = app.schema.tables.find((x) => x.name === table);
  const rows = (t && t.existing_rows) || 0;
  const ok = await ask({
    title: "Have the database enforce it?",
    body: `A unique index on ${table}.${column} makes one-to-one a rule rather than a habit.` +
      (rows > 0
        ? ` ${table} already has ${fmt(rows)} rows, and the index will fail if any two of them share a ${column}.`
        : ""),
    okLabel: "Add index",
  });
  if (!ok) return;

  app.pending.push({ kind: "add_unique", table, column });
  renderDiagram();
  reviewSchema();
}

function edgeMenu(key) {
  const [child, parent] = key.split("→");
  const childTable = child.slice(0, child.lastIndexOf("."));
  const parentTable = parent.slice(0, parent.lastIndexOf("."));
  const items = [
    { label: "Change what this relationship is…", run: () => changeCardinality(key) },
    { label: app.flow === key ? "Stop following" : "Follow this relationship",
      run: () => followEdge(key) },
  ];
  if (app.waypoints[key]) {
    items.push({ label: "Put this line back", run: () => clearWaypoint(key) });
  }
  return [
    ...items,
    "-",
    { label: `Light up ${childTable}`, run: () => setFocus(childTable) },
    { label: `Light up ${parentTable}`, run: () => setFocus(parentTable) },
    { label: `Preview ${childTable}`, run: () => previewTable(childTable) },
  ];
}

function tableMenu(name) {
  const folded = !!app.collapsed[name];
  return [
    { label: app.focus === name ? "Stop lighting it up" : "Light up its relationships",
      run: () => focusTable(name) },
    { label: "Preview rows", run: () => previewTable(name) },
    "-",
    { label: folded ? "Unfold" : "Fold away", run: () => toggleCollapse(name) },
    { label: app.editing.has(name) ? "Stop editing" : "Edit columns", run: () => {
      if (app.editing.has(name)) app.editing.delete(name);
      else app.editing.add(name);
      renderDiagram();
    } },
    "-",
    { label: droppingTable(name) ? "Keep this table" : "Drop this table",
      danger: !droppingTable(name), run: () => toggleDropTable(name) },
  ];
}

function canvasMenu() {
  return [
    { label: "New table", run: newDraft },
    "-",
    { label: "Fit the diagram", run: zoomToFit },
    { label: "Back to 100%", run: () => setZoom(1, null, true) },
    { label: "Tidy up", run: resetLayout },
    { label: "Straighten every line", run: () => {
      app.waypoints = {};
      saveWaypoints();
      drawEdges();
    } },
    "-",
    { label: "Fold every table", run: () => setAllCollapsed(true) },
    { label: "Unfold every table", run: () => setAllCollapsed(false) },
  ];
}

// setFocus is focusTable without the toggle, for the menu items that name the
// table they light up: picking "Light up users" should always light up users.
function setFocus(name) {
  app.focus = name;
  applyFocus();
  drawEdges();
}

$("board").addEventListener("contextmenu", (e) => {
  const hit = e.target.closest(".edge-hit");
  const card = e.target.closest(".table");
  // A draft has no menu of its own: its controls are all on the card.
  if (card && card.classList.contains("draft")) return;

  e.preventDefault();
  if (hit) openContextMenu(e.clientX, e.clientY, edgeMenu(hit.dataset.edge));
  else if (card) openContextMenu(e.clientX, e.clientY, tableMenu(card.dataset.table));
  else openContextMenu(e.clientX, e.clientY, canvasMenu());
});

// Anything else dismisses it, including a scroll: a menu anchored to a point on
// a canvas that has moved is pointing at the wrong thing.
document.addEventListener("pointerdown", (e) => {
  if (!ctxMenu.hidden && !e.target.closest("#ctx-menu")) closeContextMenu();
}, true);
$("board").addEventListener("scroll", closeContextMenu);
window.addEventListener("blur", closeContextMenu);

// ---------------------------------------------------------- schema editor
//
// Tables can be sketched in the diagram and created for real. The scope is
// deliberately narrow — create a table, add a column, drop a column, drop a
// table — because that is what sketching a schema to seed against needs, and
// everything past it is a migration tool.
//
// Nothing here touches the database. Edits accumulate on the client, the server
// renders them to SQL, and that SQL is shown before anything runs.

const defaultType = () => {
  const types = (app.state && app.state.types) || [];
  return types[0] || "text";
};

const isSQLite = () => app.state && app.state.dialect === "sqlite";
const isMySQL = () => app.state && app.state.dialect === "mysql";

// The auto-assigning key is spelled differently on every engine, and a key that
// does not fill itself is the first thing to go wrong in a table sketched here.
function defaultKeyType() {
  if (isSQLite()) return "INTEGER";
  if (isMySQL()) return "BIGINT AUTO_INCREMENT";
  return "bigserial";
}

function schemaChanges() {
  const creates = app.drafts.map((d) => ({
    kind: "create_table", table: d.table, columns: d.columns,
  }));
  return creates.concat(app.pending);
}

function syncChangeBar() {
  const n = schemaChanges().length;
  const btn = $("btn-changes");
  if (!btn) return;
  btn.hidden = n === 0;
  btn.textContent = n === 1 ? "1 change" : `${n} changes`;
  btn.classList.toggle("btn-primary", n > 0);
}

const droppingTable = (name) =>
  app.pending.some((c) => c.kind === "drop_table" && c.table === name);

const droppingColumn = (table, column) =>
  app.pending.some((c) => c.kind === "drop_column" && c.table === table && c.column === column);

function toggleDropTable(name) {
  const at = app.pending.findIndex((c) => c.kind === "drop_table" && c.table === name);
  if (at >= 0) {
    app.pending.splice(at, 1);
  } else {
    // Dropping the table makes every other edit to it moot, so they go with it.
    app.pending = app.pending.filter((c) => c.table !== name);
    app.pending.push({ kind: "drop_table", table: name });
  }
  renderDiagram();
}

function dropColumnButton(t, c) {
  const dropping = droppingColumn(t.name, c.name);
  const btn = el("button", "col-drop" + (dropping ? " on" : ""), dropping ? "↺" : "×");
  btn.type = "button";
  btn.title = dropping ? `Keep ${c.name}` : `Drop ${c.name}`;
  btn.setAttribute("aria-label", btn.title);
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    const at = app.pending.findIndex(
      (x) => x.kind === "drop_column" && x.table === t.name && x.column === c.name);
    if (at >= 0) app.pending.splice(at, 1);
    else app.pending.push({ kind: "drop_column", table: t.name, column: c.name });
    renderDiagram();
  });
  return btn;
}

// ---- drafts

function draftName() {
  const taken = (name) =>
    app.schema.tables.some((t) => t.name === name) ||
    app.drafts.some((d) => d.table === name);
  let name = "new_table";
  for (let i = 2; taken(name); i++) name = `new_table_${i}`;
  return name;
}

// draftSpot puts a new card in empty space to the right of everything else.
// A draft with no position lands at 0,0, underneath whatever card is already
// there, which looks like nothing happened.
function draftSpot() {
  let x = 0;
  for (const pos of Object.values(app.layout)) x = Math.max(x, pos.x);
  const used = Object.values(app.layout).filter((p) => p.x === x).length;
  return { x: x + CARD_W + GAP_X, y: used * 40 };
}

function newDraft() {
  const draft = {
    table: draftName(),
    // Every table wants a key, and the auto-assigning spelling differs by
    // engine: INTEGER PRIMARY KEY is what makes a SQLite column a rowid alias.
    columns: [{
      name: "id",
      type: defaultKeyType(),
      pk: true,
      nullable: false,
    }],
  };
  app.drafts.push(draft);
  app.layout[draft.table] = draftSpot();
  saveLayout();
  renderDiagram();
}

function draftCard(d) {
  const card = el("section", "table draft");
  card.dataset.table = d.table;
  card.dataset.hue = hueOf(d.table);

  const head = el("div", "table-head");
  const name = el("input", "draft-name");
  name.value = d.table;
  name.spellcheck = false;
  name.setAttribute("aria-label", "Table name");
  name.addEventListener("change", () => {
    const from = d.table;
    const to = name.value.trim();
    if (!to || to === from) {
      name.value = d.table;
      return;
    }
    // The layout is keyed by name, so a rename carries the position with it or
    // the card jumps back to the auto-placed spot.
    d.table = to;
    app.layout[to] = app.layout[from] || draftSpot();
    delete app.layout[from];
    if (app.pinned[from]) { app.pinned[to] = true; delete app.pinned[from]; }
    saveLayout();
    renderDiagram();
  });
  head.appendChild(name);
  head.appendChild(el("span", "table-count", "draft"));
  card.appendChild(head);
  makeDraggable(card, head, d.table);

  for (const col of d.columns) {
    card.appendChild(columnEditor(col, () => {
      d.columns.splice(d.columns.indexOf(col), 1);
      renderDiagram();
    }));
  }

  const foot = el("div", "table-foot");
  const add = el("button", "btn btn-quiet", "+ column");
  add.addEventListener("click", () => {
    d.columns.push({ name: "", type: defaultType(), nullable: true });
    renderDiagram();
  });
  foot.appendChild(add);
  foot.appendChild(el("div", "spacer"));

  const remove = el("button", "btn btn-quiet danger-quiet", "Discard");
  remove.addEventListener("click", () => {
    app.drafts.splice(app.drafts.indexOf(d), 1);
    delete app.layout[d.table];
    delete app.pinned[d.table];
    saveLayout();
    renderDiagram();
  });
  foot.appendChild(remove);
  card.appendChild(foot);
  return card;
}

// columnEditor is one editable column, used both for a draft table's columns
// and for a column being added to an existing one. They are the same thing at
// this point: a name, a type, and the three flags that change what it means.
function columnEditor(col, onRemove) {
  const row = el("div", "col col-edit");

  const name = el("input", "col-edit-name");
  name.value = col.name;
  name.placeholder = "column";
  name.spellcheck = false;
  name.setAttribute("aria-label", "Column name");
  name.addEventListener("input", () => {
    col.name = name.value.trim();
    syncChangeBar();
  });
  row.appendChild(name);

  const type = el("select", "col-edit-type");
  type.setAttribute("aria-label", "Column type");
  const types = (app.state && app.state.types) || [];
  // A type the engine's short list does not carry is still shown, so a column
  // that came from somewhere else does not silently change type.
  for (const t of types.includes(col.type) ? types : [col.type, ...types]) {
    const opt = el("option", null, t);
    opt.value = t;
    type.appendChild(opt);
  }
  type.value = col.type;
  type.addEventListener("change", () => (col.type = type.value));
  row.appendChild(type);

  const flags = el("div", "col-flags");
  flags.appendChild(flag("PK", "Primary key", !!col.pk, (on) => {
    col.pk = on;
    if (on) col.nullable = false;
    renderDiagram();
  }));
  flags.appendChild(flag("NULL", "Nullable", !!col.nullable, (on) => {
    col.nullable = on;
    if (on) col.pk = false;
    renderDiagram();
  }));
  flags.appendChild(flag("U", "Unique", !!col.unique, (on) => (col.unique = on)));
  row.appendChild(flags);

  const ref = el("input", "col-edit-ref");
  ref.value = col.references || "";
  ref.placeholder = "references table.column";
  ref.spellcheck = false;
  ref.setAttribute("aria-label", "Foreign key");
  ref.addEventListener("input", () => (col.references = ref.value.trim()));
  row.appendChild(ref);

  const remove = el("button", "col-drop", "×");
  remove.type = "button";
  remove.title = "Remove this column";
  remove.addEventListener("click", onRemove);
  row.appendChild(remove);

  return row;
}

function flag(label, title, on, onToggle) {
  const b = el("button", "flag" + (on ? " on" : ""), label);
  b.type = "button";
  b.title = title;
  b.setAttribute("aria-pressed", on ? "true" : "false");
  b.addEventListener("click", () => onToggle(!on));
  return b;
}

// ---- review and apply

const schemaDialog = $("schema-dialog");

// The page and the script ship together, but a browser can hold an old copy of
// one and a new copy of the other. A missing element must cost that one control
// and not the whole script: an uncaught TypeError up here would stop boot and
// leave the diagram empty, which looks like the database disappeared.
function bind(id, event, fn) {
  const node = $(id);
  if (!node) {
    console.warn(`seedora: #${id} is missing — reload the page to pick up the current markup`);
    return;
  }
  node.addEventListener(event, fn);
}

async function reviewSchema() {
  const changes = schemaChanges();
  if (!changes.length) return;

  $("schema-sql").textContent = "";
  showSchemaError(null);
  $("schema-note").textContent = "";
  $("schema-apply").disabled = true;
  schemaDialog.showModal();

  try {
    const res = await api("POST", "/api/schema/plan", { changes });
    const sql = res.sql || [];
    // Semicolons are for reading. Each statement is sent on its own.
    $("schema-sql").textContent = sql.map((s) => s + ";").join("\n\n");
    // MySQL commits the open transaction before every DDL statement, so the
    // promise the other engines make here is one it cannot keep. Saying so is
    // the difference between a surprise and a decision.
    $("schema-note").textContent = isMySQL()
      ? `${sql.length} statement${sql.length === 1 ? "" : "s"}, run in order. ` +
        "MySQL commits each one as it runs, so a failure part way leaves the earlier ones applied. " +
        "Nothing has been applied yet."
      : `${sql.length} statement${sql.length === 1 ? "" : "s"}, run in one transaction. ` +
        "Nothing has been applied yet.";
    $("schema-apply").disabled = false;
  } catch (e) {
    showSchemaError(e.message, e.problems);
  }
}

function showSchemaError(msg, problems) {
  const box = $("schema-error");
  box.innerHTML = "";
  if (!msg) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.appendChild(el("strong", null, msg));
  for (const p of problems || []) box.appendChild(el("div", null, p));
}

bind("schema-apply", "click", async () => {
  const changes = schemaChanges();
  busy(true, "Applying schema changes…");
  try {
    const st = await api("POST", "/api/schema/apply", { changes });
    // Applied changes are no longer pending, and the fresh state carries the
    // tables they created.
    app.drafts = [];
    app.pending = [];
    app.editing.clear();
    schemaDialog.close();
    applyState(st);
    toast("Schema updated", "good");
  } catch (e) {
    showSchemaError(e.message, e.problems);
  } finally {
    busy(false);
  }
});

bind("btn-new-table", "click", newDraft);
bind("btn-changes", "click", reviewSchema);

// ---------------------------------------------------------------- palette
//
// ⌘K jumps to a table or a column. On a schema with two hundred tables, hunting
// the canvas is the slowest thing in the tool.

const palette = $("palette-dialog");
let paletteItems = [];
let paletteAt = 0;

function openPalette() {
  if (!app.schema) return;
  $("palette-input").value = "";
  renderPalette("");
  palette.showModal();
  requestAnimationFrame(() => $("palette-input").focus());
}

function paletteCandidates() {
  const out = [];
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;
    out.push({ table: t.name, column: null, label: t.name, where: "table" });
    for (const c of t.columns) {
      if (tp.columns[c.name]) {
        out.push({ table: t.name, column: c.name, label: `${t.name}.${c.name}`, where: c.native || c.type });
      }
    }
  }
  return out;
}

function renderPalette(query) {
  const q = query.trim().toLowerCase();
  const all = paletteCandidates();
  const hits = q
    ? all
        .map((x) => ({ x, score: score(x.label.toLowerCase(), q) }))
        .filter((r) => r.score > 0)
        .sort((a, b) => b.score - a.score)
        .slice(0, 60)
        .map((r) => r.x)
    : all.slice(0, 60);

  paletteItems = hits;
  paletteAt = 0;

  const list = $("palette-list");
  list.innerHTML = "";
  if (!hits.length) {
    list.appendChild(el("div", "picker-empty", "Nothing matches that."));
    return;
  }
  hits.forEach((h, i) => {
    const opt = el("button", "palette-option" + (i === 0 ? " active" : ""));
    opt.type = "button";
    opt.setAttribute("role", "option");
    opt.dataset.index = i;
    opt.append(el("span", "what", h.label), el("span", "where", h.where));
    opt.addEventListener("click", () => choosePalette(i));
    list.appendChild(opt);
  });
}

// score prefers a prefix, then a whole-substring match, then a subsequence — the
// order that makes typing "ord.st" find orders.status without ranking noise
// above it.
function score(hay, needle) {
  if (hay.startsWith(needle)) return 1000 - hay.length;
  const at = hay.indexOf(needle);
  if (at >= 0) return 500 - at;
  let i = 0;
  for (const ch of hay) if (ch === needle[i]) i++;
  return i === needle.length ? 100 - hay.length : 0;
}

function movePalette(delta) {
  if (!paletteItems.length) return;
  paletteAt = (paletteAt + delta + paletteItems.length) % paletteItems.length;
  const list = $("palette-list");
  for (const n of list.querySelectorAll(".active")) n.classList.remove("active");
  const opt = list.querySelector(`[data-index="${paletteAt}"]`);
  if (opt) {
    opt.classList.add("active");
    opt.scrollIntoView({ block: "nearest" });
  }
}

function choosePalette(i) {
  const hit = paletteItems[i];
  palette.close();
  if (!hit) return;

  const card = cardOf(hit.table);
  if (card) card.scrollIntoView({ block: "center", inline: "center", behavior: "smooth" });
  if (hit.column) select(hit.table, hit.column);
}

$("palette-input").addEventListener("input", (e) => renderPalette(e.target.value));
$("palette-input").addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { e.preventDefault(); movePalette(1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); movePalette(-1); }
  else if (e.key === "Enter") { e.preventDefault(); choosePalette(paletteAt); }
});
$("btn-palette").addEventListener("click", openPalette);

// ---------------------------------------------------------------- seed dialog
//
// Pressing Seed opens this rather than running immediately. Seeding writes to a
// real database, and the two things a person wants to check first — which tables
// are included and how many rows each gets — are otherwise spread across every
// card on the canvas.

const seedDialog = $("seed-dialog");

function openSeedDialog() {
  renderSeedRows();
  $("seed-value").value = $("run-seed").value;
  $("seed-truncate").checked = $("run-truncate").checked;
  seedDialog.showModal();
}

function renderSeedRows() {
  const body = $("seed-rows");
  body.innerHTML = "";

  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp) continue;

    const tr = body.insertRow();

    const tick = tr.insertCell();
    tick.className = "tick";
    const on = el("input");
    on.type = "checkbox";
    on.checked = !tp.skip;
    on.setAttribute("aria-label", `Include ${t.name}`);
    on.addEventListener("change", () => {
      tp.skip = !on.checked;
      tr.classList.toggle("off", !on.checked);
      updateSeedTotal();
    });
    tick.appendChild(on);

    const name = tr.insertCell();
    name.className = "name";
    name.textContent = t.name;

    const existing = tr.insertCell();
    existing.className = "num dim";
    existing.textContent = t.existing_rows ? fmt(t.existing_rows) : "—";

    const rows = tr.insertCell();
    rows.className = "num";
    const input = el("input", "rows-input");
    input.type = "number";
    input.min = "0";
    input.value = tp.rows;
    input.setAttribute("aria-label", `Rows for ${t.name}`);
    input.addEventListener("input", () => {
      tp.rows = Math.max(0, parseInt(input.value, 10) || 0);
      updateSeedTotal();
    });
    rows.appendChild(input);

    tr.classList.toggle("off", !!tp.skip);
  }
  updateSeedTotal();
}

function updateSeedTotal() {
  let total = 0, tables = 0, destroyed = 0;
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp || tp.skip || tp.rows <= 0) continue;
    total += tp.rows;
    tables++;
    destroyed += t.existing_rows || 0;
  }

  $("seed-total").textContent = `${fmt(total)} rows · ${tables} table${tables === 1 ? "" : "s"}`;
  $("seed-intro").textContent = app.state.connected
    ? `Writing to ${app.state.engine} · ${app.state.target}. One transaction, so a failure leaves the database as it is.`
    : "";

  // The truncate label carries the number: "are you sure" without one is a
  // question nobody can answer.
  $("seed-truncate-label").textContent = destroyed > 0
    ? `Truncate first — deletes about ${fmt(destroyed)} existing rows`
    : "Truncate the selected tables first";

  const go = $("seed-go");
  go.disabled = total === 0;
  go.textContent = total === 0 ? "Nothing to seed" : `Seed ${fmt(total)} rows`;

  showOrphanWarning();
}

// showOrphanWarning catches the mistake the table list makes easy: leaving a
// parent unticked while a child that points at it stays in. The child's foreign
// keys would have nothing to draw from and the run fails partway — harmlessly,
// since it is one transaction, but only after doing all the work.
function showOrphanWarning() {
  const missing = [];

  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (!tp || tp.skip || tp.rows <= 0) continue;

    for (const c of t.columns) {
      const parentName = parentOf(t, c, tp.columns[c.name]);
      if (!parentName || parentName === t.name || c.nullable) continue;

      const parent = app.schema.tables.find((x) => x.name === parentName);
      const ptp = app.plan.tables[parentName];
      if (!parent || !ptp) continue;

      // A parent that already holds rows is fine to leave out: the child draws
      // from what is already there.
      const willHaveRows = (!ptp.skip && ptp.rows > 0) || (parent.existing_rows || 0) > 0;
      if (!willHaveRows) missing.push(`${t.name}.${c.name} needs rows in ${parentName}`);
    }
  }

  const box = $("seed-warning");
  box.innerHTML = "";
  if (!missing.length) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.appendChild(el("strong", null, "These reference a parent that will have no rows"));
  const ul = el("ul");
  for (const m of missing) ul.appendChild(el("li", null, m));
  box.appendChild(ul);
}

$("seed-all").addEventListener("change", (e) => {
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (tp) tp.skip = !e.target.checked;
  }
  renderSeedRows();
});

$("seed-bulk-apply").addEventListener("click", () => {
  const n = parseInt($("seed-bulk").value, 10);
  if (isNaN(n) || n < 0) return;
  for (const t of app.schema.tables) {
    const tp = app.plan.tables[t.name];
    if (tp && !tp.skip) tp.rows = n;
  }
  renderSeedRows();
});

$("seed-dry").addEventListener("click", () => startRun(true));

$("seed-go").addEventListener("click", async () => {
  if ($("seed-truncate").checked) {
    let destroyed = 0;
    for (const t of app.schema.tables) {
      const tp = app.plan.tables[t.name];
      if (tp && !tp.skip) destroyed += t.existing_rows || 0;
    }
    if (destroyed > 0) {
      seedDialog.close();
      const go = await ask({
        title: "Truncate before seeding?",
        body: `About ${fmt(destroyed)} existing rows will be deleted. Once the run commits this cannot be undone.`,
        danger: true,
        okLabel: "Truncate and seed",
      });
      if (!go) {
        seedDialog.showModal();
        return;
      }
      startRun(false);
      return;
    }
  }
  startRun(false);
});

// startRun pushes the edited row counts back before running, so the server seeds
// what the dialog shows rather than what it was last told.
async function startRun(dry) {
  seedDialog.close();
  $("run-seed").value = $("seed-value").value;
  $("run-truncate").checked = $("seed-truncate").checked;
  await pushPlan();
  run(dry);
}

// run starts a seeding run and watches it over Server-Sent Events.
//
// The POST returns as soon as the run is under way; everything after that
// arrives on the stream. That is what lets each card show its own progress
// rather than the page freezing on one long request.
async function run(dry) {
  let started;
  try {
    started = await api("POST", "/api/seed", {
      dry_run: dry,
      truncate: $("run-truncate").checked,
      rows: 0,
      seed: readSeedValue(),
    });
  } catch (e) {
    toast(e.message, "bad");
    return;
  }
  watchRun(started.run_id, dry);
}

const readSeedValue = () => {
  const n = parseInt($("run-seed").value, 10);
  return isNaN(n) ? 0 : n;
};

function watchRun(runID, dry) {
  app.running = true;
  setRunningUI(true, dry);

  const source = new EventSource("/api/seed/events");

  source.addEventListener("progress", (e) => {
    const p = JSON.parse(e.data);
    showTableProgress(p.table, p.written, p.total);
  });

  source.addEventListener("done", (e) => {
    source.close();
    finishRun(JSON.parse(e.data), dry);
  });

  source.addEventListener("failed", (e) => {
    source.close();
    app.running = false;
    setRunningUI(false);
    clearTableProgress();
    toast(JSON.parse(e.data).error, "bad");
  });

  source.onerror = () => {
    // EventSource reconnects on its own, and the server replays the run from the
    // start, so a dropped connection is not worth reporting. A run that has
    // already finished closes the stream, which lands here too.
    if (!app.running) source.close();
  };
}

async function finishRun(res, dry) {
  app.running = false;
  setRunningUI(false);

  // What each table got, kept on its card until the next run. A toast that
  // vanishes in five seconds is no way to report what was written.
  app.lastRun = { dry, byTable: {}, at: Date.now() };
  for (const t of res.tables || []) app.lastRun.byTable[t.table] = t.rows;

  const secs = res.duration / 1e9;
  const rate = secs > 0 ? Math.round(res.rows / secs) : 0;
  toast(
    `${dry ? "Validated" : "Seeded"} ${fmt(res.rows)} rows in ${secs.toFixed(2)}s` +
      (rate ? ` · ${fmt(rate)} rows/s` : "") + ` · seed ${res.seed}`,
    "good"
  );

  applyState(await api("GET", "/api/state"));
}

// setRunningUI disables the things that would corrupt a run in flight and turns
// the Seed button into a live status.
function setRunningUI(on, dry) {
  const go = $("btn-seed");
  go.disabled = on;
  go.textContent = on ? (dry ? "Validating…" : "Seeding…") : "Seed…";
  for (const id of ["btn-import", "btn-save"]) $(id).disabled = on;
  $("board").classList.toggle("running", on);
  if (!on) clearTableProgress();
}

// showTableProgress draws the bar on a card. The DOM is touched directly rather
// than re-rendered: at a few hundred events a second, rebuilding the diagram
// each time would drop frames for no gain.
function showTableProgress(table, written, total) {
  const card = cardOf(table);
  if (!card) return;

  let bar = card.querySelector(".progress");
  if (!bar) {
    bar = el("div", "progress");
    bar.appendChild(el("div", "progress-fill"));
    bar.appendChild(el("span", "progress-text"));
    card.querySelector(".table-head").after(bar);
  }
  const pct = total > 0 ? Math.min(100, (written / total) * 100) : 0;
  bar.querySelector(".progress-fill").style.width = pct + "%";
  bar.querySelector(".progress-text").textContent =
    `${fmt(written)} / ${fmt(total)}`;
  card.classList.add("busy");
}

function clearTableProgress() {
  for (const bar of document.querySelectorAll(".progress")) bar.remove();
  for (const card of document.querySelectorAll(".table.busy")) card.classList.remove("busy");
}

$("btn-seed").addEventListener("click", openSeedDialog);
$("btn-undo").addEventListener("click", undo);
$("btn-redo").addEventListener("click", redo);

// ---------------------------------------------------------------- import

const importDialog = $("import-dialog");

$("btn-import").addEventListener("click", () => {
  $("import-error").hidden = true;
  importDialog.showModal();
});

$("btn-browse").addEventListener("click", () => $("import-file").click());
$("import-file").addEventListener("change", async (e) => {
  const file = e.target.files && e.target.files[0];
  if (file) $("import-text").value = await file.text();
});

// Dropping the file straight onto the dialog is the shortest path, so the
// browser's default — navigating away to display the file — has to go.
for (const type of ["dragenter", "dragover"]) {
  $("drop").addEventListener(type, (e) => {
    e.preventDefault();
    $("drop").classList.add("over");
  });
}
for (const type of ["dragleave", "drop"]) {
  $("drop").addEventListener(type, () => $("drop").classList.remove("over"));
}
$("drop").addEventListener("drop", async (e) => {
  e.preventDefault();
  const file = e.dataTransfer.files && e.dataTransfer.files[0];
  if (file) $("import-text").value = await file.text();
});

// looksLikeSQL decides which of the two things the text is. A mapping is YAML
// with known keys at the top; a schema is a file of statements. Sniffing rather
// than asking, because the answer is never in doubt and a format picker on an
// import dialog is a question with one right answer.
function looksLikeSQL(text) {
  const head = text.slice(0, 4000).toLowerCase();
  if (/^\s*(version:|tables:)/m.test(text)) return false;
  return /\bcreate\s+table\b/.test(head);
}

$("btn-do-import").addEventListener("click", async () => {
  const text = $("import-text").value;
  if (!text.trim()) {
    showImportError("Nothing to import — paste a config or a schema, or choose a file.", []);
    return;
  }

  busy(true, "Importing…");
  try {
    if (looksLikeSQL(text)) {
      await importSchema(text);
    } else {
      applyState(await api("POST", "/api/import", {
        yaml: text,
        replace: $("import-replace").checked,
      }));
      importDialog.close();
      $("import-text").value = "";
      toast("Mapping imported", "good");
    }
  } catch (e) {
    showImportError(e.message, e.problems || []);
  } finally {
    busy(false);
  }
});

// importSchema hands a .sql file to the schema editor rather than applying it.
// A file that creates fourteen tables is exactly the case where the SQL should
// be read before it runs, and that dialog already exists.
async function importSchema(text) {
  const res = await api("POST", "/api/import", { yaml: text, format: "sql" });
  const changes = res.changes || [];
  const skipped = res.skipped || [];

  if (!changes.length) {
    showImportError(
      skipped.length
        ? `Nothing to create — this database already has ${skipped.join(", ")}.`
        : "No tables were found in that file.", []);
    return;
  }

  // The parsed tables become drafts, so they can be edited before they are
  // applied and so the diagram shows what is about to appear.
  app.drafts = changes.map((c) => ({ table: c.table, columns: c.columns || [] }));
  for (const d of app.drafts) {
    if (!app.layout[d.table]) app.layout[d.table] = draftSpot();
  }
  saveLayout();

  importDialog.close();
  $("import-text").value = "";
  renderDiagram();
  toast(skipped.length
    ? `${changes.length} table${changes.length === 1 ? "" : "s"} to create — ${skipped.length} already here`
    : `${changes.length} table${changes.length === 1 ? "" : "s"} to create`);
  reviewSchema();
}

// scanMigrations reads the project's migration files server-side and draws the
// tables the database does not have as drafts.
//
// The gap it closes is the ordinary one: the schema lives in the repository as
// .sql files, someone checked out a branch that adds tables, and introspection
// cannot see a table that does not exist yet. Nothing is applied — the missing
// tables arrive as drafts and go through the same review dialog every other
// edit does.
async function scanMigrations(path) {
  try {
    const res = await api("POST", "/api/schema/scan", { path });
    const changes = res.changes || [];
    const existing = res.existing || [];
    const files = (res.files || []).length;

    if (!changes.length) {
      toast(`${path}: ${existing.length} table${existing.length === 1 ? "" : "s"} — the database is up to date`);
      return;
    }

    app.drafts = changes.map((c) => ({ table: c.table, columns: c.columns || [] }));
    for (const d of app.drafts) {
      if (!app.layout[d.table]) app.layout[d.table] = draftSpot();
    }
    saveLayout();
    renderDiagram();
    toast(`${files} migration file${files === 1 ? "" : "s"} read — ` +
      `${changes.length} table${changes.length === 1 ? "" : "s"} the database does not have, ` +
      `${existing.length} already here`);
  } catch (e) {
    toast(`${path}: ${e.message}`, "bad");
  }
}

function showImportError(msg, problems) {
  const box = $("import-error");
  box.innerHTML = "";
  box.hidden = false;
  box.appendChild(el("strong", null, msg));
  if (problems.length) {
    const ul = el("ul");
    for (const p of problems) ul.appendChild(el("li", null, p));
    box.appendChild(ul);
  }
}

// Three exports, three different things, so the button asks which. A plain
// navigation for the download itself: the browser's own handling applies and
// the page never holds the file in memory.
$("btn-export").addEventListener("click", (e) => {
  const box = e.currentTarget.getBoundingClientRect();
  openContextMenu(box.left, box.bottom + 6, [
    { label: "Mapping — seedora.yaml", run: () => download("yaml") },
    { label: "Schema — schema.sql", run: () => download("sql") },
    { label: "Diagram — schema.mmd", run: () => download("mermaid") },
  ]);
});

const download = (format) => { window.location.href = "/api/export?format=" + format; };

// ---------------------------------------------------------------- connect

async function connect(body) {
  $("connect-error").hidden = true;
  busy(true, "Connecting and reading the schema…");
  try {
    applyState(await api("POST", "/api/connect", body));
    loadConnections();
  } catch (e) {
    const box = $("connect-error");
    box.textContent = e.message;
    box.hidden = false;
  } finally {
    busy(false);
  }
}

$("btn-connect").addEventListener("click", () => {
  const dsn = $("dsn").value.trim();
  if (!dsn) return;
  connect({
    dsn,
    remember: $("remember").checked,
    keep_password: $("keep-password").checked,
  });
});

$("dsn").addEventListener("keydown", (e) => e.key === "Enter" && $("btn-connect").click());

// "Store the password" only means anything if the connection is remembered at
// all, so it follows that checkbox rather than sitting there enabled.
$("remember").addEventListener("change", () => {
  const on = $("remember").checked;
  $("keep-password").disabled = !on;
  if (!on) $("keep-password").checked = false;
});

async function loadConnections() {
  let list = [];
  try {
    list = await api("GET", "/api/connections");
  } catch {
    // A store that cannot be read is not worth a message on the connect screen:
    // pasting a DSN still works, which is all this list saves.
    return;
  }
  $("recent").hidden = list.length === 0;

  const ul = $("recent-list");
  ul.innerHTML = "";
  for (const c of list) {
    const li = document.createElement("li");

    const open = el("button", "recent-item");
    open.type = "button";
    open.append(el("strong", null, c.name), el("span", null, c.dsn));
    open.addEventListener("click", () => useConnection(c));

    const forget = el("button", "btn btn-icon", "✕");
    forget.type = "button";
    forget.title = "Forget this connection";
    forget.setAttribute("aria-label", `Forget ${c.name}`);
    forget.addEventListener("click", async (e) => {
      e.stopPropagation();
      await api("POST", "/api/connections/forget", { name: c.name });
      loadConnections();
    });

    li.append(open, forget);
    ul.appendChild(li);
  }
}

async function useConnection(c) {
  if (c.needs_password) {
    // The password was deliberately not stored, so it has to come from the
    // person in front of the screen.
    const pw = await ask({
      title: "Password needed",
      body: `${c.name} was remembered without its password.`,
      okLabel: "Connect",
      input: { label: "Password", type: "password" },
    });
    if (pw === null) return;
    connect({ name: c.name, password: pw, remember: true, keep_password: false });
    return;
  }
  connect({ name: c.name, remember: true, keep_password: true });
}

// ---------------------------------------------------------------- top bar

$("btn-save").addEventListener("click", async () => {
  try {
    const r = await api("POST", "/api/save", {});
    toast(`Saved ${r.path}`, "good");
  } catch (e) {
    toast(e.message, "bad");
  }
});

$("btn-reset-layout").addEventListener("click", resetLayout);

// ---------------------------------------------------------------- keyboard

document.addEventListener("keydown", (e) => {
  const inField = e.target.matches("input, textarea, select");
  const anyDialogOpen = document.querySelector("dialog[open]");

  // ⌘K / Ctrl-K opens the palette from anywhere, which is the convention every
  // tool with one follows.
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    if (!anyDialogOpen) openPalette();
    return;
  }

  // ⌘S saves, because the muscle memory is already there and the browser's own
  // save dialog is useless here.
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
    e.preventDefault();
    if (app.state && app.state.connected) $("btn-save").click();
    return;
  }

  if (anyDialogOpen) return;

  if (e.key === "Escape") {
    if (!ctxMenu.hidden) { closeContextMenu(); return; }
    if (!$("preview").hidden) { $("preview").hidden = true; return; }
    if (app.flow) { followEdge(app.flow); return; }
    if (app.focus) { focusTable(app.focus); return; }
    if (!$("inspector").hidden) closeInspector();
    return;
  }

  // Undo is the one shortcut that keeps its modifier, because it is the one
  // people arrive already knowing. It also works while a field has focus,
  // where a bare letter would be a character someone was typing — but not in a
  // text field, where the browser's own undo is the one they mean.
  const mod = e.metaKey || e.ctrlKey;
  if (mod && e.key.toLowerCase() === "z" && !inField) {
    e.preventDefault();
    if (e.shiftKey) redo(); else undo();
    return;
  }
  if (mod && e.key.toLowerCase() === "y" && !inField) {
    e.preventDefault();
    redo();
    return;
  }

  // Single-letter shortcuts stay out of the way of anything being typed.
  if (inField || e.metaKey || e.ctrlKey || e.altKey) return;
  if (!app.state || !app.state.connected) return;

  if (e.key === "+" || e.key === "=") { e.preventDefault(); zoomBy(ZOOM_STEP); }
  else if (e.key === "-") { e.preventDefault(); zoomBy(-ZOOM_STEP); }
  else if (e.key === "0") { e.preventDefault(); setZoom(1, null, true); }
  else if (e.key === "f") { e.preventDefault(); zoomToFit(); }
  else if (e.key === "c") { e.preventDefault(); $("btn-fold-all").click(); }
  else if (e.key === "s") { e.preventDefault(); openSeedDialog(); }
  else if (e.key === "t") { e.preventDefault(); $("btn-theme").click(); }
  else if (e.key === "l") { e.preventDefault(); resetLayout(); }
});

// ---------------------------------------------------------------- boot

(async function boot() {
  initTheme();
  loadSettings();
  try {
    app.generators = await api("GET", "/api/generators");
    app.byId = new Map(app.generators.map((g) => [g.id, g]));
    applyState(await api("GET", "/api/state"));
    if (!app.state.connected) loadConnections();
    if (app.state.connected && app.state.migrations) scanMigrations(app.state.migrations);
  } catch (e) {
    toast(e.message, "bad");
  }
})();
