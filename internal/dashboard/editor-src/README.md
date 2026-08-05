# Rebuilding the vendored editor bundle

`entry.js` is the source for `../static/codemirror.bundle.js`. It's not
part of the Go module or build — the compiled bundle is committed directly
and that's what `go:embed` picks up. This directory exists only so the
bundle can be rebuilt/updated later.

```sh
npm install
npx esbuild entry.js --bundle --minify --format=iife \
  --platform=browser --target=es2020 \
  --outfile=../static/codemirror.bundle.js
```

Exposes `window.HekaEditor.create(parentEl, initialText, opts)` — see
`entry.js` for the returned handle's API (`getValue`, `setValue`,
`setReadOnly`, `gotoLine`, `focus`, `destroy`) and `opts`
(`onChange`, `onSave`, `readOnly`). Colors are pulled from the dashboard's
own CSS custom properties (`style.css`), so it follows the same light/dark
switching without a separate theme.
