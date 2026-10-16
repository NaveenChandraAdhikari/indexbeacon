# The idxa web interface

Plain HTML, CSS and ES modules. No framework, no bundler, no build step: the
files under `dist` are the sources, and they are embedded into the binary with
`go:embed`. Changing a colour should not require installing a toolchain.

```
dist/index.html      the shell
dist/app.css         all styles, with light and dark palettes as custom properties
dist/app.js          routing, sidebar, modal, keyboard shortcuts
dist/dom.js          element helpers, number formatting, sparklines
dist/api.js          fetch and server-sent event client
dist/views/          overview, database, findings, form, settings
```

## Testing it

`browser-test.mjs` drives the interface in a headless Chromium over the
DevTools protocol, clicking through every view and saving screenshots. It
catches the things unit tests cannot: a dialog that will not close, a
conditional that renders the word "null", a layout that hides navigation on a
narrow screen. All three of those were real bugs it found.

```
idxa serve --listen 127.0.0.1:8787 &
"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser" \
  --headless=new --remote-debugging-port=9222 --user-data-dir=/tmp/idxa-profile about:blank &
node web/browser-test.mjs
```

Any Chromium works; the path above is just an example.

## Rules the code follows

- **Paint before touching a database.** The shell and sidebar render from
  configuration alone. A server that is down must never leave the page blank.
- **Update in place.** Live tables keep fixed column widths, tabular numerals
  and a stable sort, and rows are patched cell by cell rather than rebuilt, so
  a value going from 9 to 10 does not make the page jump.
- **One stream per view**, closed when the view unmounts and when the tab is
  hidden. An unattended browser tab must not keep a database busy.
- **Text through textContent, never markup.** Values come from a database, so
  a table named after a script tag must render as characters.
- **Empty states say what to do next**, not just that there is nothing there.
