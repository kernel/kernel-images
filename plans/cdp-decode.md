# Plan: Two-layer CDP decode — protocol-faithful types, then monitor projection

## Problem

The cdpmonitor decodes CDP wire events directly into hand-crafted structs (`cdpConsoleArg`, `cdpConsoleParams`, `cdpNetworkRequestParams`, etc.) that keep only the fields the current monitor needs and silently drop the rest.

**Issues:**

1. **Audit difficulty.** The struct names imply fidelity to CDP, but the shapes are projections. A reviewer can't tell which fields were dropped intentionally vs. forgotten without diffing against the PDL by hand.
2. **Silent field loss at the wrong layer.** Adding `frameId` or `initiator.type` later requires widening the decode struct before the projection can even reference it. Decode and projection are conflated.

## Solution

Split decode into two layers, with the boundary enforced by function signatures:

- **Layer 1 (protocol decode):** structs that retain every PDL-declared field. Top-level scalars are typed; complex sub-objects (`StackTrace`, `Initiator`, `ResourceTiming`, etc.) are kept as `json.RawMessage` to preserve them without committing to recursive typing.
- **Layer 2 (monitor projection):** unchanged behaviour. Receives a typed Layer-1 struct (not raw `json.RawMessage`) and projects to `events.Event`.

**Invariant:** every field declared in the PDL for an event we handle appears on its Layer-1 struct. Sub-objects may be `json.RawMessage`, but they are *retained*, not dropped.

**Outcome:** the decode boundary is mechanically auditable against the PDL (one file to diff). Future additions to monitor event payloads are pure Layer-2 changes.

## Architecture

```
Wire → cdpMessage envelope
          ↓ (monitor.go dispatch decodes Params into the right Layer-1 struct)
       Layer 1 struct (cdp_proto.go, mirrors PDL)
          ↓ (passed by value to handler)
       Layer 2 handler (handlers.go, projection logic)
          ↓
       events.Event
```

Decode happens **once**, in `monitor.go`'s `switch msg.Method` dispatch. Handlers receive typed Layer-1 structs, not raw bytes. This makes the Layer 1 → Layer 2 boundary a function signature: a grep for handler call sites is the audit.

**File layout:**

```
server/lib/cdpmonitor/
├── types.go         wire envelope + monitor internals + constants
├── cdp_proto.go     NEW: Layer 1 PDL-faithful CDP types (with PDL pin header)
├── util.go          consoleArgString(cdpRuntimeRemoteObject)
├── monitor.go       dispatch decodes msg.Params into Layer-1 structs before calling handlers
└── handlers.go      Layer 2 — handler signatures take Layer-1 structs
```

## Scope (Layer 1 contents)

**Minimal scope: only events currently consumed by handlers.** Speculative coverage of types we don't use yet (LCP, BindingCalled, full TargetInfo, LayoutShiftAttribution) is the same projection-creep problem in a different shape — it gets added when a handler needs it.

Layer 1 covers exactly:

- **Runtime:** `cdpRuntimeRemoteObject`, `cdpRuntimeConsoleAPICalledParams`, `cdpRuntimeExceptionThrownParams` (with nested `ExceptionDetails`)
- **Network:** `cdpNetworkRequest`, `cdpNetworkResponse`, `cdpNetworkRequestWillBeSentParams`, `cdpNetworkResponseReceivedParams`, `cdpNetworkLoadingFinishedParams`, `cdpNetworkLoadingFailedParams`
- **Page:** `cdpPageFrame`, `cdpPageFrameNavigatedParams`, `cdpPageDomContentEventFiredParams`, `cdpPageLoadEventFiredParams`
- **PerformanceTimeline:** `cdpPerformanceTimelineEvent`, `cdpPerformanceTimelineEventAddedParams` (Layout-shift detail stays as `json.RawMessage` until a handler needs typed access)
- **Target:** `cdpTargetTargetInfo`, `cdpTargetAttachedToTargetParams`, `cdpTargetCreatedParams`, `cdpTargetDestroyedParams`, `cdpTargetDetachedFromTargetParams`

Each struct retains every PDL field for that type at the top level, even fields the current handler ignores.

## Changes

**Modified:**

- `types.go` — remove projection types (`cdpConsoleArg`, `cdpConsoleParams`, `cdpExceptionDetails`, `cdpNetworkRequestParams`, `cdpResponseReceivedParams`, `cdpTargetInfo`, `cdpAttachedToTargetParams`, `cdpTargetCreatedParams`). Keep `cdpMessage`, `cdpError`, `targetInfo`, `networkReqState`, constants.
- `monitor.go` — dispatch (`switch msg.Method`) decodes `msg.Params` into the appropriate Layer-1 struct before calling the handler. Decode error paths logged consistently.
- `handlers.go` — handler signatures change from `(ctx, sess, raw json.RawMessage)` to `(ctx, sess, p cdpRuntimeConsoleAPICalledParams)` etc. Bodies drop their local `json.Unmarshal` calls.
- `util.go` — `consoleArgString` takes `cdpRuntimeRemoteObject` instead of `cdpConsoleArg`.

**Added:**

- `cdp_proto.go` — Layer 1 types (see Scope). Top of file:
  ```go
  // Generated against:
  //   Chromium: <FILL IN — e.g. 130.0.6723.58>
  //   js_protocol.pdl: <FILL IN sha>
  //   browser_protocol.pdl: <FILL IN sha>
  // To re-audit: diff field sets against the PDL files at the pinned revision.
  ```

**Tests:**

- `cdp_proto_test.go` (NEW) — golden-fixture tests. For each Layer-1 struct, a captured real Chrome-emitted JSON frame in `testdata/`. Test decodes into the Layer-1 struct, re-marshals, then compares **normalized full JSON** (recursive key-sorted, whitespace-stripped) against the original frame. A full structural round-trip catches not just dropped top-level fields but also dropped nested fields, silently-coerced types, and field-name mismatches. Any non-equivalence is an audit failure.
- `handlers_test.go` — update call sites to construct Layer-1 structs directly instead of raw JSON where convenient; existing wire-level tests in `cdp_test.go` and `monitor_test.go` continue to exercise the dispatch path end-to-end.

## Design decisions

1. **Decode in dispatch, not handlers.** One unmarshal per event; handler signatures encode the Layer 1 → Layer 2 boundary; grepping handler call sites is the audit. Alternative (decode inside each handler) was rejected because the boundary becomes a convention rather than a signature, and conventions rot.

2. **Minimal Layer-1 scope.** Only events we currently dispatch get Layer-1 types. Adding speculative coverage (LCP, BindingCalled) in this PR would replicate the projection-creep problem we're fixing.

3. **Complex sub-objects as `json.RawMessage`.** `StackTrace`, `Initiator`, `ResourceTiming`, `SecurityDetails`, `ObjectPreview`, layout-shift detail, etc. are *retained but not parsed*. Layer 1 promises field **retention**, not full recursive typing. A future Layer-2 caller can `json.Unmarshal` the raw segment into a typed struct when it needs the fields.

4. **Headers as `json.RawMessage`.** `Network.Headers` is PDL-typed as `object` and emitted as a string→string map, but values can be primitives or arrays in some Chromium builds. Keeping it raw avoids a wrong type assertion and defers the decision to the consumer.

5. **Naming convention.** `cdp` prefix + domain + type name (`cdpRuntimeRemoteObject`, `cdpNetworkRequest`, `cdpPageFrame`). Bare PDL names (`Request`, `Response`, `Frame`) collide with stdlib and event-package types in Go; the prefix disambiguates and keeps types unexported.

6. **PDL pin in source.** Header comment on `cdp_proto.go` records the Chromium and PDL revisions Layer-1 was written against. Without a pin, "auditable against the PDL" is meaningless because the PDL moves.

## Out of scope

- Recursive typing of `StackTrace`, `Initiator`, `ResourceTiming`, etc. (kept as raw JSON until needed).
- Layer-1 coverage for events the monitor doesn't currently handle (LCP, BindingCalled, etc.).
- Full codegen from `.pdl` → Go in this PR. The pin + golden round-trip tests are the audit mechanism for now.

## Follow-up: light codegen from json/js_protocol.json

The DevTools repo ships a JSON form of the protocol (`json/js_protocol.json`, `json/browser_protocol.json`) alongside the `.pdl` sources. A small generator could read these, emit Layer-1 struct skeletons for the domains we care about, and diff against the hand-written types — catching fields that were added upstream since the pin. Worth doing once Layer 1 stabilizes and we have a second or third reason to regenerate (e.g. adding LCP). Not blocking this PR; the golden round-trip test covers drift for the types we already have.
