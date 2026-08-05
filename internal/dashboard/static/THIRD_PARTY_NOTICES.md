# Third-party notices

`codemirror.bundle.js` is a locally built bundle (esbuild, `--bundle
--minify --format=iife`) of the following MIT-licensed packages, vendored
here so the dashboard needs no network access at runtime — no CDN, no
build step in `go:embed`, no external dependency in the deployed binary:

- `@codemirror/state`
- `@codemirror/view`
- `@codemirror/commands`
- `@codemirror/language`
- `@codemirror/lang-yaml`
- `@lezer/highlight`, `@lezer/yaml`, `@lezer/common`, `@lezer/lr` (transitive)

All are © their respective authors, MIT License. See
https://codemirror.net and https://lezer.codemirror.net for source and
full license text.

To rebuild the bundle: see `entry.js`-equivalent source kept alongside the
project's build notes (not part of the Go module) — bundle with esbuild
(`--bundle --minify --format=iife --platform=browser --target=es2020`)
against the pinned versions above and replace this file.
