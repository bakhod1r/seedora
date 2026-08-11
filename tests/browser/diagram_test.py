"""End-to-end tests for the page, in a real browser.

What tests/ui/ cannot reach: the diagram is drawn by measuring cards the
browser has laid out, and the drag gestures are pointer events against those
measurements. Neither exists without a box model, so neither can be checked by
loading app.js into a vm.

This is the only part of Seedora that needs a toolchain, and it is kept out of
the way of the one that does not: Playwright is installed by the person running
these, `go build` never sees it, and the binary still ships with no runtime.
Run them with `make test-browser`.
"""

import json
import os
import sqlite3
import subprocess
import time
import urllib.error
import urllib.request

import pytest
from playwright.sync_api import expect, sync_playwright

HOST = "127.0.0.1"
PORT = int(os.environ.get("SEEDORA_TEST_PORT", "7801"))
BASE = f"http://{HOST}:{PORT}"
REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


@pytest.fixture(scope="session")
def server():
    """A real seedora, built and run against the large demo schema.

    The binary is built rather than `go run`, so a compile error is a clear
    failure here instead of a timeout waiting for a port.
    """
    binary = os.path.join(REPO, "tmp", "seedora-browser-test")
    subprocess.run(
        ["go", "build", "-o", binary, "./cmd/seedora"],
        cwd=REPO, check=True,
    )

    # Built here from the schema rather than reused from tmp/, because one of
    # these tests seeds. A database left with rows in it collides on the first
    # unique column the next time round and fails for a reason that has nothing
    # to do with the page — which is exactly what `make demo-big` would hand
    # over, since it does not rebuild a file that already exists.
    db = os.path.join(REPO, "tmp", "browser-test.db")
    os.makedirs(os.path.dirname(db), exist_ok=True)
    if os.path.exists(db):
        os.remove(db)
    with open(os.path.join(REPO, "testdata", "demo_large.sql")) as f:
        schema = f.read()
    conn = sqlite3.connect(db)
    conn.executescript(schema)
    conn.close()

    config = os.path.join(REPO, "tmp", "browser-test.yaml")
    if os.path.exists(config):
        os.remove(config)

    proc = subprocess.Popen(
        [binary, "--dsn", db, "--port", str(PORT), "--config", config],
        cwd=REPO,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        # The UI opens a browser tab on start, which is not wanted here.
        env={**os.environ, "BROWSER": "true", "DISPLAY": ""},
    )

    for _ in range(100):
        try:
            urllib.request.urlopen(f"{BASE}/api/state", timeout=1)
            break
        except (urllib.error.URLError, ConnectionError):
            if proc.poll() is not None:
                raise RuntimeError(f"server exited: {proc.stdout.read().decode()}")
            time.sleep(0.1)
    else:
        proc.kill()
        raise RuntimeError("server never came up")

    yield BASE
    proc.terminate()
    proc.wait(timeout=10)


@pytest.fixture(scope="session")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch(headless=True)
        yield b
        b.close()


@pytest.fixture
def page(browser, server):
    """A page with console errors and failed requests collected.

    Both are assertions in their own right: the script has no error handling
    around most of what it does, so an exception leaves the page half drawn and
    otherwise silent.
    """
    context = browser.new_context(viewport={"width": 1600, "height": 1000})
    p = context.new_page()
    p.errors = []
    p.failures = []
    p.on("console", lambda m: p.errors.append(m.text) if m.type == "error" else None)
    p.on("pageerror", lambda e: p.errors.append(str(e)))
    p.on("requestfailed", lambda r: p.failures.append(f"{r.method} {r.url}"))

    p.goto(server, wait_until="networkidle")
    p.wait_for_selector("section.table", timeout=15000)
    yield p

    context.close()


def cards(page):
    """Every table card's name and its box, as the router sees them."""
    return page.evaluate("""() => [...document.querySelectorAll('section.table')].map(c => {
        const r = c.getBoundingClientRect();
        return {
            name: c.dataset.table,
            left: r.left, top: r.top, right: r.right, bottom: r.bottom,
            width: r.width, height: r.height,
        };
    })""")


def test_the_page_loads_without_errors(page):
    assert page.errors == [], f"console errors: {page.errors}"
    assert page.failures == [], f"failed requests: {page.failures}"


def test_every_table_is_drawn(page, server):
    """The diagram shows the schema, not some of it."""
    state = json.loads(urllib.request.urlopen(f"{server}/api/state").read())
    expected = {t["name"] for t in state["schema"]["tables"]}

    drawn = {c["name"] for c in cards(page)}
    assert drawn == expected, f"missing from the diagram: {expected - drawn}"


def test_no_two_cards_overlap(page):
    """What the layout relaxation exists to guarantee, measured on real boxes.

    tests/ui/ checks this against heights it was handed. Here the heights are
    the ones the browser computed from the actual content, which is the only
    place a card that is taller than the layout assumed shows up.
    """
    boxes = cards(page)
    overlapping = []
    for i, a in enumerate(boxes):
        for b in boxes[i + 1:]:
            if (a["left"] < b["right"] and a["right"] > b["left"]
                    and a["top"] < b["bottom"] and a["bottom"] > b["top"]):
                overlapping.append(f"{a['name']}/{b['name']}")
    assert overlapping == [], f"overlapping cards: {overlapping}"


def test_every_relationship_is_drawn_as_an_edge(page, server):
    """One line per foreign key, and every line has a path."""
    state = json.loads(urllib.request.urlopen(f"{server}/api/state").read())
    keys = sum(
        1
        for t in state["schema"]["tables"]
        for c in t["columns"]
        if c.get("fk")
    )

    paths = page.evaluate(
        """() => [...document.querySelectorAll('#edges path')]
            .map(p => p.getAttribute('d')).filter(d => d && d.length > 0).length"""
    )
    assert paths >= keys, f"{keys} foreign keys but only {paths} edges drawn"


def test_no_edge_passes_under_a_card(page):
    """The router's claim, checked against what was actually rendered.

    Two things this has to get right or it reports crossings that are not
    there. Only `path.edge` is a route — `edge-hit` is its invisible grab area
    and `edge-foot` is the crow's foot, which is drawn on the card's edge on
    purpose. And the comparison happens in the page's own unscaled coordinates,
    via obstacles(), because a path's points are in SVG user space while
    getBoundingClientRect is in the viewport, and the two differ by the
    diagram's origin, its scroll, and the zoom.

    The path is sampled along its length rather than parsed: the corners are
    rounded, so the control points are not on the route, and a point on the
    curve is what a reader actually sees.
    """
    crossings = page.evaluate("""() => {
        const boxes = obstacles();
        const bad = [];
        for (const path of document.querySelectorAll('#edges path.edge')) {
            const total = path.getTotalLength();
            if (!total) continue;
            // The ends sit on the cards the edge belongs to, so a margin at
            // each end is skipped rather than counted as a collision.
            for (let at = total * 0.06; at <= total * 0.94; at += Math.max(total / 300, 1)) {
                const pt = path.getPointAtLength(at);
                for (const b of boxes) {
                    if (pt.x > b.left + 2 && pt.x < b.right - 2 &&
                        pt.y > b.top + 2 && pt.y < b.bottom - 2) {
                        bad.push(`${path.dataset.key || 'an edge'} crosses ${b.table}`);
                        at = total;
                        break;
                    }
                }
            }
        }
        return [...new Set(bad)];
    }""")
    assert crossings == [], f"{len(crossings)} edge(s) cross a card: {crossings[:5]}"


def test_dragging_a_card_moves_it_and_keeps_it(page):
    """A drag is the gesture the layout has to respect, and it is remembered."""
    first = cards(page)[0]
    name = first["name"]
    handle = page.locator(f'section.table[data-table="{name}"] .table-head').first

    box = handle.bounding_box()
    page.mouse.move(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)
    page.mouse.down()
    page.mouse.move(box["x"] + box["width"] / 2 + 220, box["y"] + box["height"] / 2 + 160, steps=12)
    page.mouse.up()
    page.wait_for_timeout(300)

    moved = next(c for c in cards(page) if c["name"] == name)
    assert abs(moved["left"] - first["left"]) > 100, "the card did not move"

    # A dragged card is pinned: reloading must not put it back.
    page.reload(wait_until="networkidle")
    page.wait_for_selector("section.table")
    page.wait_for_timeout(500)
    after = next(c for c in cards(page) if c["name"] == name)
    assert abs(after["left"] - moved["left"]) < 40, "the drag was forgotten on reload"
    assert page.errors == [], f"console errors during the drag: {page.errors}"


def test_folding_a_table_shrinks_it(page):
    """Folding is state the page keeps and redraws the edges around."""
    name = cards(page)[0]["name"]
    before = next(c for c in cards(page) if c["name"] == name)

    page.locator(f'section.table[data-table="{name}"] .table-head .fold').first.click()
    page.wait_for_timeout(400)

    after = next(c for c in cards(page) if c["name"] == name)
    assert after["height"] < before["height"], "the card did not fold"
    assert page.errors == [], f"console errors while folding: {page.errors}"


def test_selecting_a_column_opens_the_inspector(page):
    """The mapping editor is the product; this is the click that reaches it."""
    page.locator("section.table .col").first.click()
    page.wait_for_timeout(300)

    hidden = page.evaluate("() => document.getElementById('inspector').hidden")
    assert not hidden, "the inspector did not open"
    assert page.errors == [], f"console errors: {page.errors}"


def test_zoom_keeps_the_diagram_on_screen(page):
    """Zoom is applied as a transform, and a wrong origin sends it off-canvas."""
    page.keyboard.press("f")
    page.wait_for_timeout(400)

    onscreen = page.evaluate("""() => {
        const boxes = [...document.querySelectorAll('section.table')]
            .map(c => c.getBoundingClientRect());
        return boxes.filter(r => r.right > 0 && r.bottom > 0).length;
    }""")
    assert onscreen > 0, "zoom-to-fit left nothing visible"
    assert page.errors == [], f"console errors while zooming: {page.errors}"


def test_a_seeding_run_reports_progress_and_finishes(page):
    """The whole product in one gesture: press s, run, watch the SSE stream."""
    page.keyboard.press("s")
    page.wait_for_selector("#seed-dialog[open]", timeout=5000)

    # The dialog lists a row-count field per table; the bulk field fills them
    # all, which is the gesture somebody seeding twenty-three tables uses.
    page.locator("#seed-bulk").fill("25")
    page.locator("#seed-bulk-apply").click()
    # Truncate first, so the test says the same thing however many times it is
    # run against the same database.
    page.locator("#seed-truncate").check()
    page.locator("#seed-go").click()

    # The run reports through SSE, and finishing is what the toast says.
    page.wait_for_function(
        """() => {
            const t = document.body.innerText;
            return /rows? in |Seeded|complete/i.test(t);
        }""",
        timeout=30000,
    )
    assert page.errors == [], f"console errors during the run: {page.errors}"


def test_the_page_is_usable_at_a_laptop_width(browser, server):
    """1280x800 is the smallest screen this is used on; it must not clip."""
    context = browser.new_context(viewport={"width": 1280, "height": 800})
    p = context.new_page()
    errors = []
    p.on("pageerror", lambda e: errors.append(str(e)))
    p.goto(server, wait_until="networkidle")
    p.wait_for_selector("section.table", timeout=15000)

    overflow = p.evaluate(
        "() => document.documentElement.scrollWidth > window.innerWidth + 4"
    )
    assert not overflow, "the page scrolls sideways at 1280px"
    assert errors == [], f"errors at 1280px: {errors}"
    context.close()


def test_undo_reverses_an_edit(page):
    """The gesture end to end: change a row count, take it back via the
    button, and confirm the server (not just the page) has the old value.

    tests/ui/ proves the stack; this proves the button reaches it and the
    page shows the result, which is the half that needs a browser.
    """
    rows = page.locator("section.table input[type='number']").first
    original = rows.input_value()

    rows.fill("4321")
    rows.dispatch_event("change")

    undo_button = page.locator("#btn-undo")
    expect(undo_button).to_be_enabled()

    undo_button.click()
    expect(rows).to_have_value(original)
    expect(page.locator("#btn-redo")).to_be_enabled()
    assert page.errors == [], f"console errors: {page.errors}"

    # Undo goes through the server, not a local revert: a reload must still
    # show the restored value, the same way test_dragging_a_card_moves_it_
    # and_keeps_it proves a drag survives one.
    page.reload(wait_until="networkidle")
    page.wait_for_selector("section.table")
    rows_after_reload = page.locator("section.table input[type='number']").first
    expect(rows_after_reload).to_have_value(original)


def test_undo_reverses_an_edit_via_keystroke(page):
    """Cmd-Z / Ctrl-Z is the gesture that needs a real browser: `keydown`
    handling is invisible to tests/ui/'s vm harness. This drives the
    keystroke, not the button, and proves redo's keystroke too.
    """
    rows = page.locator("section.table input[type='number']").first
    original = rows.input_value()

    rows.fill("5150")
    rows.dispatch_event("change")

    undo_button = page.locator("#btn-undo")
    expect(undo_button).to_be_enabled()

    # The handler deliberately ignores the keystroke while a text field has
    # focus, so blur the row-count input before pressing it.
    page.locator("body").click()
    # Control+z works on both platforms: the handler checks
    # `e.metaKey || e.ctrlKey`.
    page.keyboard.press("Control+z")
    expect(rows).to_have_value(original)

    redo_button = page.locator("#btn-redo")
    expect(redo_button).to_be_enabled()
    page.keyboard.press("Control+Shift+z")
    expect(rows).to_have_value("5150")

    assert page.errors == [], f"console errors: {page.errors}"


def test_undo_is_disabled_with_nothing_to_undo(page):
    """A fresh page has no history, and the control says so rather than
    offering an action that would do nothing."""
    assert page.locator("#btn-undo").is_disabled()
    assert page.locator("#btn-redo").is_disabled()
