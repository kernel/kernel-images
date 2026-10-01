# Vault fill owner protocol (work in progress)

This path is implemented but has not yet completed end-to-end validation. Do not deploy it or enable the API fill gate based on unit tests.

The Chromium launcher remains the browser's parent instead of execing it. It reserves the loopback owner listener before checking for surviving Chromium processes. The next browser is not started and the owner HTTP handler is not served until the previous browser/renderer/zygote/crashpad processes are gone. Inspection errors, unsupported pidfds, permission failures, an occupied DevTools port, and unsuccessful termination fail closed. A successful supervisorctl response is not used as proof of fencing.

The image CDP proxy forwards `kernelVaultFill=1` to this owner without reconnecting upgraded streams or logging command payloads. The API must complete a nonce-bound `kernel.vault-fill.v1` handshake, including the actual instance name, before sending CDP commands. Older images or proxies cannot complete this handshake and receive no secret-bearing command on the new API path.

One operation occupies the owner. Commands are sequential, ID-checked and use one fixed upstream socket. A finish message is accepted only between acknowledged commands; it closes that upstream before releasing admission and makes the old client terminal. Disconnect, cancellation, protocol failure or loss of a command response leaves the generation occupied. Neither Redis key deletion nor a timeout clears it. There is no reconnect, replay or force-unlock endpoint. Lost finish acknowledgment does not make a browser write unknown: the client already acknowledged every command by sending finish.

API/proxy restart does not restart the launcher or clear its state. Launcher restart must fence surviving Chrome processes before admitting a replacement. Parent-death signaling closes the pre-exec orphan window; the startup process census also covers Chrome surviving owner death. This relies on Linux pidfd/proc semantics, not durable Redis or filesystem lease records. The image's immutable Chromium executable and its shipped process helpers are the supported process cohort; out-of-band browser binaries or subprocess launch wrappers are not a supported fenced configuration.

Standby/restore retains the owner's state together with the browser. An active or quarantined generation does not become idle on disconnect or resume. The owner captures its instance identity at birth and checks it on admission and every command. A template waiting for fork identity cannot fill, and an owner cannot adopt a different identity in place. Fork handoff requires a launcher restart after identity application before fill is supported; that restart must confirm the old browser generation exited. A restored active operation must never be used as a clean template. These platform lifecycle cases still require validation beyond protocol unit tests.

The protocol covers the API's synchronous guarded CDP scripts and their completion. It does not serialize unrelated customer CDP clients or application JavaScript timers. It is not a security boundary against privileged process/file mutation inside the browser.

## Integration order

1. Complete real-image lifecycle and API adversarial validation.
2. Merge/release the image companion, then integrate that image through the normal release path. Drain incompatible pool/template inventory; validate the target platform's process primitives and fork reset behavior.
3. Merge the API transport change. Its protocol negotiation is mandatory; no fallback to ordinary CDP is permitted.
4. Consider the default-off fill gate separately. Neither companion PR enables it.

Pending evidence: actual image owner restart with surviving Chrome, rejected/uncertain stop, lost-response quarantine, terminal old clients, independent browsers, two full public HTTP runs and the formerly failing lost-key regression. Protocol unit tests and API authorization tests against stock Chromium are not substitutes for these checks.
