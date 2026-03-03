# Disable Passkey Conditional UI in Browser VMs

**Status: PLANNING**

## Background

Passkey "conditional UI" (also called "conditional mediation") integrates WebAuthn passkey authentication into browser form autofill. When a website calls `navigator.credentials.get()` with `mediation: 'conditional'` and the input has `autocomplete="webauthn"`, Chrome shows passkey suggestions in the autofill dropdown. This popup blocks CUA (Computer Use Agent) click actions during managed auth flows (e.g. Amazon login).

### What PR #170 Tried

PR #170 added `--disable-features=WebAuthenticationConditionalUI` to both the headless `wrapper.sh` defaults and the headful `chromium-launcher` hardcoded args. 

### Why It Doesn't Work (Raf's Review)

Raf checked the Chromium v145 source and found:

1. **`WebAuthenticationConditionalUI` no longer exists as a feature flag.** It was present in earlier Chromium versions when conditional mediation was experimental, but has since graduated and been removed. The only trace is in `tools/metrics/histograms/enums.xml` as a historical record.

2. **`IsConditionalMediationAvailable` unconditionally returns `true`** in current Chromium (`content/browser/webauth/authenticator_common_impl.cc`):
   ```cpp
   // Desktop Chrome can always show GPM passkeys through conditional mediation.
   std::move(callback).Run(true);
   ```

3. **No enterprise policy exists** to disable this behavior.

4. **`wrapper.sh` changes don't take effect for pool browsers** — the API sets `CHROMIUM_FLAGS` which overrides the `wrapper.sh` defaults. If we do find a working flag, it should go in the chromium-launcher hardcoded args (which are always applied) and/or wherever the API sets flags.

## Research Findings

### What Won't Work

| Approach | Why It Won't Work |
|----------|-------------------|
| `--disable-features=WebAuthenticationConditionalUI` | Flag removed from Chromium; silently ignored |
| `--disable-features=WebAuthentication` | Disables the entire WebAuthn API, which would break sites that need it for non-conditional flows |
| Enterprise policy for passkey conditional UI | No such policy exists in Chrome's policy templates |
| Chrome user settings | No built-in setting to disable passkey conditional UI |

### Viable Approaches

#### Option A: CDP Virtual Authenticator Environment (Recommended)

**How it works:** The Chrome DevTools Protocol has a `WebAuthn` domain. Calling `WebAuthn.enable()` activates a "virtual authenticator environment" that disconnects the browser from real authenticator discovery and substitutes a virtual one. With no virtual authenticators registered (or one with no credentials), there are no passkeys to show in the autofill dropdown, so the conditional UI never appears.

**Pros:**
- No Chromium source changes needed
- Works with any Chromium version
- Precisely targeted — only affects passkey discovery, doesn't break other WebAuthn flows if a virtual authenticator is added later
- We already have a CDP WebSocket proxy on port 9222 — could be done at startup or via a new API endpoint

**Cons:**
- Must be called per browser-level CDP session (per Chromium process lifetime)
- Need to send the CDP command after Chromium starts and the DevTools WebSocket is available
- If Chromium restarts, the virtual authenticator environment resets and needs to be re-enabled

**Implementation sketch:**
1. After `chromium-launcher` detects DevTools is ready (it already waits for port 9223), send a CDP `WebAuthn.enable` command via WebSocket
2. Alternatively, add it to the API server's upstream-ready flow — when `UpstreamManager` detects a new DevTools URL, send `WebAuthn.enable` on the browser-level WS endpoint
3. Could also expose a `PATCH /chromium/webauthn` or similar API endpoint

#### Option B: Chrome Extension using `webAuthenticationProxy` API

**How it works:** Chrome 115+ has a `chrome.webAuthenticationProxy` extension API. An extension calls `attach()` and then receives all WebAuthn requests as events. The extension can reject them (blocking passkeys entirely) or selectively handle them.

The open-source [Disable-Passkeys](https://github.com/TheConfax/Disable-Passkeys) extension does exactly this.

**Pros:**
- Survives Chromium restarts automatically (extension stays loaded)
- Well-tested approach (the extension has real users)
- Can be selectively enabled/disabled per domain if needed
- Already have extension upload infrastructure (`UploadExtensionsAndRestart`)

**Cons:**
- Requires building/bundling a custom MV3 extension
- Adds complexity to the image or startup flow
- Must be loaded via `--load-extension` or enterprise policy
- Blocks ALL WebAuthn flows (get + create), not just conditional UI, unless custom logic is added
- Extension API is MV3-only

**Implementation sketch:**
1. Create a minimal MV3 extension with `webAuthenticationProxy` permission
2. In the background service worker, call `chrome.webAuthenticationProxy.attach()` and reject all `onGetRequest` events with an error
3. Bundle it into the Docker image at a known path (e.g., `/opt/extensions/disable-passkeys/`)
4. Add it to `--load-extension` in the chromium-launcher hardcoded args

#### Option C: JavaScript Injection via `Page.addScriptToEvaluateOnNewDocument`

**How it works:** Override `PublicKeyCredential.isConditionalMediationAvailable` to return `false` and/or monkey-patch `navigator.credentials.get` to strip `mediation: 'conditional'` from options.

**Pros:**
- No Chromium build changes
- Can be done via CDP from the API server

**Cons:**
- Race condition — the script must be injected before the page's JS runs; `addScriptToEvaluateOnNewDocument` handles this but must be set per-target
- Fragile — websites may detect the override or use different code paths
- Must be re-applied on every new tab/target
- Some sites may break if `isConditionalMediationAvailable` returns `false` unexpectedly

**Implementation sketch:**
1. On each new target, send `Page.addScriptToEvaluateOnNewDocument` with:
   ```javascript
   PublicKeyCredential.isConditionalMediationAvailable = async () => false;
   ```
2. This would need to be done in the CDP proxy or via the API server's target management

#### Option D: Chromium Source Patch

**How it works:** Patch the Chromium source to make `IsConditionalMediationAvailable` return `false`, or gate it behind a new feature flag, then build a custom Chromium package.

**Pros:**
- Definitive fix
- Works everywhere without runtime setup

**Cons:**
- Requires maintaining a custom Chromium build — enormous maintenance burden
- Chromium build takes hours and produces multi-GB artifacts
- Must be re-applied on every Chromium version update
- Currently using distro-packaged Chromium (`apt-get -y install chromium`)

**Verdict:** Not viable unless we're already maintaining custom Chromium builds.

## Recommendation

**Option A (CDP `WebAuthn.enable`)** is the recommended approach:

1. It's the most surgical — it only removes passkey discovery without breaking other functionality
2. It's pure runtime config — no custom builds, no bundled extensions
3. It integrates naturally with the existing architecture (CDP proxy, upstream manager)
4. It's the same mechanism that Chrome DevTools itself uses for WebAuthn testing

**Option B (Extension)** is a solid fallback if Option A proves insufficient, particularly because it survives Chromium restarts without needing re-initialization.

A hybrid approach is also viable: use Option A as the default, and if customers need more granular control (e.g., per-domain passkey whitelisting), offer Option B via the existing extension upload API.

## Implementation Plan (Option A)

### Step 1: Add CDP WebAuthn.enable to Startup Flow

Location: API server or chromium-launcher

When the upstream manager detects a new DevTools WebSocket URL, connect to the browser-level endpoint and send:
```json
{"id": 1, "method": "WebAuthn.enable"}
```

This should happen in the API server's `UpstreamManager` callback (or equivalent), since it already knows when Chromium has restarted and DevTools is ready.

### Step 2: Handle Chromium Restarts

The `WebAuthn.enable` state is per-process. When Chromium is restarted (via `supervisorctl restart chromium`, extension upload, flags update, etc.), the virtual authenticator environment resets. The upstream manager already detects restarts — the `WebAuthn.enable` call should be part of the post-restart initialization.

### Step 3: Make It Configurable (Optional)

Add an env var or API flag (e.g., `DISABLE_PASSKEY_AUTOFILL=true`, defaulting to `true`) so customers can opt out if they actually need passkey conditional UI in their browser VMs.

### Step 4: Test

- Build headless image, navigate to Amazon login, verify no passkey popup
- Build headful image, same test
- Verify that after Chromium restart (e.g., via extension upload), passkeys are still suppressed
- Verify that explicitly registering a virtual authenticator via CDP still works (for customers who want to test passkeys)

### Step 5: Clean Up PR #170

Remove the `WebAuthenticationConditionalUI` references from both `wrapper.sh` and `chromium-launcher/main.go` since they have no effect.
