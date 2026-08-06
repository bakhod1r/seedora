// The demo shim.
//
// This page is the real Seedora UI — the same markup, stylesheet, and script
// the binary serves. What it does not have is a database, so this file answers
// the API from a recording taken by running the tool against the example
// schema. Nothing here reimplements Seedora; every response it returns was
// produced by Seedora.
//
// Two rules kept the demo honest:
//
//   - Anything read-only answers with the real recorded data, so the diagram,
//     the inspector, and the previews behave exactly as they do locally.
//   - Anything that would write says so and does nothing. A demo that pretends
//     to seed a database teaches people something false about a tool whose
//     whole job is writing to one.

(function () {
  "use strict";

  const files = {};
  const load = (name) =>
    fetch(`demo/${name}.json`).then((r) => r.json()).then((d) => (files[name] = d));

  const ready = Promise.all([
    load("state"), load("generators"), load("previews"), load("history"),
  ]);

  // The plan as the page has edited it. Keeping it means a generator picked in
  // the inspector, a row count typed into a card, and a column dragged into a
  // new position all stick for the session, exactly as they would locally.
  let state = null;

  const json = (body, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });

  const refuse = (what) =>
    json({
      error: `${what} needs a database. This is the demo — run Seedora locally to do it for real.`,
    }, 409);

  const real = window.fetch.bind(window);

  window.fetch = async function (input, init) {
    const url = typeof input === "string" ? input : input.url;
    const path = url.replace(/^https?:\/\/[^/]+/, "").split("?")[0];
    if (!path.startsWith("/api/")) return real(input, init);

    await ready;
    if (!state) state = files.state;

    const body = init && init.body ? JSON.parse(init.body) : null;

    switch (path) {
      case "/api/state":
        return json(state);

      case "/api/generators":
        return json(files.generators);

      case "/api/connections":
        return json([]);

      case "/api/history":
        return json(files.history);

      case "/api/plan":
        state = { ...state, plan: body };
        return json(state);

      case "/api/validate":
        return json({ problems: state.problems || [] });

      case "/api/preview": {
        const recorded = files.previews[body && body.table];
        if (!recorded) return json({ error: "No preview was recorded for that table." }, 404);
        if (recorded.error) return json({ error: recorded.error }, 400);

        // Regenerate asks for a different draw. There is one recording, so the
        // rows are rotated: the button visibly does something, and it does not
        // pretend to have generated anything new.
        if (body.nonce) {
          const rows = recorded.rows.slice();
          rows.push(rows.shift());
          return json({ columns: recorded.columns, rows });
        }
        return json(recorded);
      }

      // The schema editor renders its SQL on the server. Answering here would
      // mean shipping a second implementation of the renderer, and a demo whose
      // SQL differs from the product's is worse than one that says so.
      case "/api/schema/plan":
      case "/api/schema/apply":
        return refuse("Applying schema changes");

      // Scanning a migrations directory reads files off the machine Seedora is
      // running on. There is no machine here.
      case "/api/schema/scan":
        return refuse("Reading a migrations directory");

      case "/api/seed":
        return refuse("Seeding");

      case "/api/save":
        return refuse("Saving seedora.yaml");

      case "/api/import":
        return refuse("Importing");

      case "/api/connect":
        return refuse("Connecting");

      default:
        return json({ error: "Not part of the demo." }, 404);
    }
  };

  // The banner belongs to the demo, so its styles live here rather than in the
  // product's stylesheet — that file is copied verbatim on every build and must
  // not carry anything the binary would then serve.
  document.addEventListener("DOMContentLoaded", () => {
    const style = document.createElement("style");
    style.textContent = `
      .demo-banner {
        position: fixed;
        left: 50%;
        bottom: 16px;
        transform: translateX(-50%);
        z-index: 60;
        max-width: min(780px, calc(100vw - 32px));
        padding: 10px 18px;
        border: 1px solid var(--border);
        border-radius: 999px;
        background: color-mix(in srgb, var(--surface) 88%, transparent);
        backdrop-filter: blur(10px) saturate(140%);
        box-shadow: var(--shadow-2);
        color: var(--fg-muted);
        font-size: 12px;
        line-height: 1.5;
        text-align: center;
      }
      .demo-banner strong { color: var(--accent); }
      .demo-banner a { color: var(--link); }
      @media (max-width: 900px) { .demo-banner { display: none; } }
    `;
    document.head.appendChild(style);

    const banner = document.createElement("div");
    banner.className = "demo-banner";
    banner.innerHTML =
      "<strong>Demo</strong> — a real schema and the real UI, with no database behind it. " +
      "Zoom, fold, drag, light up a table's relationships, preview generated rows, change a generator. " +
      "Everything that writes is disabled. " +
      '<a href="https://github.com/bakhod1r/seedora">Run it locally</a> to seed for real.';
    document.body.appendChild(banner);
  });
})();
