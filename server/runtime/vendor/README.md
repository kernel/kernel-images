# Vendored dependencies

## acorn.mjs

- Package: `acorn` v8.18.0 (<https://github.com/acornjs/acorn>)
- License: MIT (see `acorn.LICENSE`)
- Source file: `dist/acorn.mjs` from the published npm package.

Vendored so the browser REPL daemon can parse user code for its
top-level-await rewrite without depending on whatever packages happen to be
installed globally in the image. Node's own REPL uses acorn the same way
(`allowAwaitOutsideFunction` / `allowReturnOutsideFunction`).

To update: `npm pack acorn@<version>` and copy `dist/acorn.mjs` and `LICENSE`
here, then update the version above.
