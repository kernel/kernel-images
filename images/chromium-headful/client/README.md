# Embedded live-view control

The `readOnly=true` query parameter sets the initial input mode (aliases: `readonly`,
`ro`; accepted true values: `1`, `true`, `yes`).

Embedded parents can change the mode without reconnecting:

```js
iframe.contentWindow.postMessage(
  { type: 'KERNEL_SET_READ_ONLY', readOnly: true, requestId: crypto.randomUUID() },
  new URL(iframe.src).origin,
)
```

The viewer accepts messages only from its immediate parent and the exact origin
in `document.referrer`. Parents must allow an origin referrer, for example with
`referrerPolicy="strict-origin-when-cross-origin"`. Missing or opaque referrers
cannot authorize mode changes.

`KERNEL_CONNECTED` includes `capabilities: ['setReadOnly']` on supporting images.
After applying a valid request, the viewer sends `KERNEL_READ_ONLY_CHANGED` with
the same `requestId` and the applied boolean `readOnly` to the parent origin.
Requests without `requestId` remain supported for existing parents.
Parents must validate the iframe window, origin, request ID, and applied mode.

For older images without the capability, change the query parameter and reload.
If an advertised capability fails to acknowledge a request, fall back to a reload
with the desired query mode. The dashboard uses a one-second acknowledgement timeout.

Read-only links stay read-only. When the live-view proxy serves a read-only link it
sets `window.KERNEL_LIVE_VIEW_READ_ONLY = true`; the viewer then ignores the query
parameter and answers unlock requests with `readOnly: true`. The server refuses that
viewer's input regardless.

Read-only mode locks local input and disables implicit hosting. Unlocking restores
the server's configured implicit-hosting setting. Mode changes persist across
transport reconnects; a full page reload uses the URL's initial mode again.
