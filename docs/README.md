# The demo

This directory is published to GitHub Pages. **It is generated — do not edit it
by hand.**

```sh
make pages
python3 -m http.server -d docs 8080
```

`tools/pages` copies `internal/ui/assets/*` verbatim and records what the API
returns by running Seedora against `examples/ecommerce/schema.sql`. The page is
therefore the real UI, and the data in it is real output rather than a fixture
somebody wrote to look plausible.

`demo.js` is the only file that is not generated. It intercepts `fetch` and
answers from the recording: read-only calls get the real data, and anything that
would write to a database says so and does nothing.

The workflow in `.github/workflows/pages.yml` rebuilds this on every push to
`main`, which is what keeps the demo from drifting away from the product.
