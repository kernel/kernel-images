# BlackShield Security Audit Report — `kernel/kernel-images`

- **Scanned Commit:** `629500231850878e78b8c1baff1041f031c4dc99`
- **Actionable Findings:** `23` (`5 Critical`, `18 High`, `24 Medium`, `2 Low`)
- **BlackShield Platform & Continuous Zero-Cost PR Protection:** [https://blackshield.chaplau.com](https://blackshield.chaplau.com)

## Detailed Findings & Remediation Guide

### 1. [CRITICAL] `CVE-2022-37601` — CVE-2022-37601 in loader-utils@1.4.2

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `loader-utils@1.4.2`
- **Details:** Prototype pollution in loader-utils via interpolateName method.
- **Recommended Fix:** Upgrade `loader-utils` to `2.0.4` or newer

### 2. [CRITICAL] `CVE-2023-45133` — CVE-2023-45133 in @babel/traverse@7.20.13

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `@babel/traverse@7.20.13`
- **Details:** Arbitrary code execution when compiling maliciously crafted code in @babel/traverse.
- **Recommended Fix:** Upgrade `@babel/traverse` to `7.23.2` or newer

### 3. [CRITICAL] `sec-unsafe-reflection` — sec-unsafe-reflection: Unsafe Dynamic Reflection from User Input

- **Category:** `sast`
- **Location:** `images/chromium-headful/demos/demo_smooth_typing.py:31`
- **Details:** Using user-supplied class or module names in dynamic reflection enables arbitrary code execution. at images/chromium-headful/demos/demo_smooth_typing.py:31
- **Recommended Fix:** Use a registry pattern with explicit string-to-class mappings instead of dynamic reflection.

### 4. [CRITICAL] `pkcs8-private-key` — PKCS#8 Cryptographic Private Key Detected

- **Category:** `secret`
- **Location:** `server/e2e/test-extension-enterprise/private_key.pem:1`
- **Details:** Potential hardcoded credential or secret detected in `server/e2e/test-extension-enterprise/private_key.pem:1` (rule `pkcs8-private-key`). Raw value redacted (`***REDACTED***`) for safe disclosure.
- **Recommended Fix:** Revoke leaked credential immediately and rotate in secret manager.

### 5. [CRITICAL] `bs-poly-js-vm-rce` — bs-poly-js-vm-rce: Sandbox Escape / Code Injection via vm.runInContext

- **Category:** `sast`
- **Location:** `server/runtime/browser-repl.ts:463`
- **Details:** Sandbox Escape / Code Injection via vm.runInContext (CWE-94 / A03:2021). at server/runtime/browser-repl.ts:463
- **Recommended Fix:** Validate, parameterize, or sanitize untrusted inputs before invoking dangerous sinks.

### 6. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `images/chromium-headful/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 7. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `images/chromium-headful/client/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 8. [HIGH] `CVE-2022-25883` — CVE-2022-25883 in semver@6.3.0

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `semver@6.3.0`
- **Details:** Regular Expression Denial of Service (ReDoS) in semver range parser.
- **Recommended Fix:** Upgrade `semver` to `7.5.2` or newer

### 9. [HIGH] `CVE-2023-26115` — CVE-2023-26115 in word-wrap@1.2.3

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `word-wrap@1.2.3`
- **Details:** Regular Expression Denial of Service (ReDoS) in word-wrap string splitting.
- **Recommended Fix:** Upgrade `word-wrap` to `1.2.4` or newer

### 10. [HIGH] `CVE-2024-21538` — CVE-2024-21538 in cross-spawn@7.0.3

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `cross-spawn@7.0.3`
- **Details:** Regular Expression Denial of Service (ReDoS) in cross-spawn argument escaping.
- **Recommended Fix:** Upgrade `cross-spawn` to `7.0.5` or newer

### 11. [HIGH] `CVE-2024-37890` — CVE-2024-37890 in ws@8.12.0

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `ws@8.12.0`
- **Details:** Denial of Service when handling requests with excess HTTP headers in ws.
- **Recommended Fix:** Upgrade `ws` to `8.17.1` or newer

### 12. [HIGH] `CVE-2024-4068` — CVE-2024-4068 in braces@3.0.2

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `braces@3.0.2`
- **Details:** Uncontrolled resource consumption in braces imbalanced brace expansion.
- **Recommended Fix:** Upgrade `braces` to `3.0.3` or newer

### 13. [HIGH] `CVE-2024-43796` — CVE-2024-43796 in express@4.18.2

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `express@4.18.2`
- **Details:** Open redirect and route tampering via path-to-regexp vulnerability in Express.
- **Recommended Fix:** Upgrade `express` to `4.20.0` or newer

### 14. [HIGH] `CVE-2024-45296` — CVE-2024-45296 in path-to-regexp@0.1.7

- **Category:** `vulnerability`
- **Location:** `images/chromium-headful/client/package-lock.json`
- **Affected Package:** `path-to-regexp@0.1.7`
- **Details:** Backtracking regular expression in path-to-regexp causes Denial of Service.
- **Recommended Fix:** Upgrade `path-to-regexp` to `0.1.10` or newer

### 15. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `images/chromium-headful/xorg-deps/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 16. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `images/chromium-headful/xorg-deps/xf86-input-neko/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 17. [HIGH] `CKV_DOCKER_1` — CKV_DOCKER_1: Container Runs as Root (Missing Non-Root USER)

- **Category:** `misconfiguration`
- **Location:** `images/chromium-headless/image/Dockerfile:1`
- **Details:** Container does not specify a non-root USER, allowing processes to execute with root privileges.
- **Recommended Fix:** Add 'USER <non-root-uid>' instruction before container entrypoint.

### 18. [HIGH] `sec-zip-slip-traversal` — sec-zip-slip-traversal: Zip Slip Directory Traversal on Archive Extraction

- **Category:** `sast`
- **Location:** `server/cmd/api/api/chromium.go:322`
- **Details:** Extracting archive members without validating target file paths enables arbitrary file overwrite. at server/cmd/api/api/chromium.go:322
- **Recommended Fix:** Verify each archive entry's resolved path starts with the intended target directory before extraction.

### 19. [HIGH] `sec-zip-slip-traversal` — sec-zip-slip-traversal: Zip Slip Directory Traversal on Archive Extraction

- **Category:** `sast`
- **Location:** `server/cmd/api/api/fs.go:622`
- **Details:** Extracting archive members without validating target file paths enables arbitrary file overwrite. at server/cmd/api/api/fs.go:622
- **Recommended Fix:** Verify each archive entry's resolved path starts with the intended target directory before extraction.

### 20. [HIGH] `sec-path-traversal` — sec-path-traversal: Path Traversal / Arbitrary File Access

- **Category:** `sast`
- **Location:** `server/cmd/api/api/process.go:329`
- **Details:** Accessing filesystem paths constructed from unvalidated input allows path traversal. at server/cmd/api/api/process.go:329
- **Recommended Fix:** Sanitize user-provided file paths with filepath.Clean or secure_filename before reading.

### 21. [HIGH] `go-cmdi-exec-combined` — go-cmdi-exec-combined: Command Execution via Dynamic exec.Command

- **Category:** `sast`
- **Location:** `server/cmd/wrapper/supervisord.go:114`
- **Details:** Go application security vulnerability or insecure coding pattern. at server/cmd/wrapper/supervisord.go:114
- **Recommended Fix:** Invoke the target binary directly with exec.Command(bin, args...) without sh -c shell evaluation.

### 22. [HIGH] `sec-broken-crypto-cipher` — sec-broken-crypto-cipher: Deprecated or Weak Symmetric Cipher Algorithm

- **Category:** `sast`
- **Location:** `server/lib/policy/policy_registry.go:475`
- **Details:** Using broken cryptographic algorithms (DES, 3DES, RC4, Blowfish, AES-ECB mode) compromises data confidentiality. at server/lib/policy/policy_registry.go:475
- **Recommended Fix:** Use AES-256-GCM or ChaCha20-Poly1305 for symmetric encryption. Avoid ECB mode.

### 23. [HIGH] `sec-zip-slip-traversal` — sec-zip-slip-traversal: Zip Slip Directory Traversal on Archive Extraction

- **Category:** `sast`
- **Location:** `server/lib/ziputil/ziputil.go:95`
- **Details:** Extracting archive members without validating target file paths enables arbitrary file overwrite. at server/lib/ziputil/ziputil.go:95
- **Recommended Fix:** Verify each archive entry's resolved path starts with the intended target directory before extraction.

