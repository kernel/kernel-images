// Answers navigator.credentials.get/create publicKey requests through the
// kernel-images binding instead of the browser's authenticators. Requests
// carry no origin: kernel-images takes it from Chromium. Anything the binding
// does not answer with a credential or an error goes to the browser's own
// implementation.
//
// Protocol: the page calls BINDING with {id, op, mediation?, publicKey} and
// resends the same id every RESEND_MS until it has a final answer.
// kernel-images replies through REPLY with {id, ack: true} for every send it
// receives and {id, result, ...} once answered. Silence for SILENCE_MS (no
// connection, or kernel-images restarted) falls back to the browser.
(function () {
  const BINDING = '__kernelPasskey';
  const REPLY = '__kernelPasskeyReply';
  const RESEND_MS = 1000;
  const SILENCE_MS = 10000;

  if (Object.prototype.hasOwnProperty.call(globalThis, REPLY)) return;
  if (typeof CredentialsContainer !== 'function' || typeof PublicKeyCredential !== 'function') return;

  const container = CredentialsContainer.prototype;
  const nativeGet = container.get;
  const nativeCreate = container.create;
  const nativeCapabilities = PublicKeyCredential.getClientCapabilities;
  const setTimer = globalThis.setTimeout.bind(globalThis);
  const clearTimer = globalThis.clearTimeout.bind(globalThis);
  const now = Date.now.bind(Date);
  const stringify = JSON.stringify;
  const pending = new Map();

  Object.defineProperty(globalThis, REPLY, {
    value: function (message) {
      const request = message && pending.get(message.id);
      if (!request) return;
      if (message.ack) {
        request.heard = now();
        return;
      }
      request.finish(message);
    },
  });

  function isBuffer(value) {
    return value instanceof ArrayBuffer || ArrayBuffer.isView(value);
  }

  function bytes(value) {
    return value instanceof ArrayBuffer ? new Uint8Array(value) : new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  }

  function encode(buffer) {
    let binary = '';
    for (const byte of bytes(buffer)) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function decode(text) {
    const base64 = text.replace(/-/g, '+').replace(/_/g, '/');
    const binary = atob(base64 + '='.repeat((4 - (base64.length % 4)) % 4));
    const out = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
    return out.buffer;
  }

  // WebAuthn L3 JSON form of creation and request options: every buffer
  // becomes base64url.
  function toJSONValue(value) {
    if (isBuffer(value)) return encode(value);
    if (Array.isArray(value)) return value.map(toJSONValue);
    if (value && typeof value === 'object') {
      const out = {};
      for (const key of Object.keys(value)) {
        const converted = toJSONValue(value[key]);
        if (converted !== undefined) out[key] = converted;
      }
      return out;
    }
    if (typeof value === 'function' || typeof value === 'symbol') return undefined;
    return value;
  }

  function define(target, values) {
    for (const key of Object.keys(values)) {
      Object.defineProperty(target, key, { value: values[key], enumerable: true });
    }
    return target;
  }

  function credentialFrom(json) {
    json.type = 'public-key';
    json.authenticatorAttachment = json.authenticatorAttachment || 'platform';
    json.clientExtensionResults = json.clientExtensionResults || {};
    const r = json.response;
    let response;
    if (r.attestationObject !== undefined) {
      response = define(Object.create(AuthenticatorAttestationResponse.prototype), {
        clientDataJSON: decode(r.clientDataJSON),
        attestationObject: decode(r.attestationObject),
        getAuthenticatorData: () => decode(r.authenticatorData),
        getPublicKey: () => (r.publicKey ? decode(r.publicKey) : null),
        getPublicKeyAlgorithm: () => r.publicKeyAlgorithm,
        getTransports: () => (r.transports || []).slice(),
      });
    } else {
      response = define(Object.create(AuthenticatorAssertionResponse.prototype), {
        clientDataJSON: decode(r.clientDataJSON),
        authenticatorData: decode(r.authenticatorData),
        signature: decode(r.signature),
        userHandle: r.userHandle ? decode(r.userHandle) : null,
      });
    }
    return define(Object.create(PublicKeyCredential.prototype), {
      id: json.id,
      rawId: decode(json.rawId),
      type: json.type,
      authenticatorAttachment: json.authenticatorAttachment,
      response,
      getClientExtensionResults: () => structuredClone(json.clientExtensionResults),
      toJSON: () => structuredClone(json),
    });
  }

  function abortReason(signal) {
    return signal.reason !== undefined ? signal.reason : new DOMException('The operation was aborted.', 'AbortError');
  }

  // Resolves with the final answer, or null when kernel-images stays silent.
  function exchange(op, options, signal) {
    return new Promise((resolve, reject) => {
      const id = encode(crypto.getRandomValues(new Uint8Array(16)));
      const message = { id, op, publicKey: toJSONValue(options.publicKey) };
      if (options.mediation !== undefined) message.mediation = String(options.mediation);
      const payload = stringify(message);
      let timer;
      const request = { heard: now(), finish: (answer) => settle(() => resolve(answer)) };
      function settle(done) {
        pending.delete(id);
        clearTimer(timer);
        if (signal) signal.removeEventListener('abort', onAbort);
        done();
      }
      function onAbort() {
        settle(() => reject(abortReason(signal)));
      }
      function send() {
        if (now() - request.heard >= SILENCE_MS) {
          request.finish(null);
          return;
        }
        try {
          globalThis[BINDING](payload);
        } catch {
          // No binding yet: same as an unanswered send.
        }
        timer = setTimer(send, RESEND_MS);
      }
      if (signal) signal.addEventListener('abort', onAbort);
      pending.set(id, request);
      send();
    });
  }

  async function intercept(op, native, container, options) {
    const signal = options.signal;
    if (signal && signal.aborted) throw abortReason(signal);
    const answer = await exchange(op, options, signal);
    if (answer && answer.result === 'credential') return credentialFrom(answer.credential);
    if (answer && answer.result === 'error') throw new DOMException(answer.message, answer.name);
    return native.call(container, options);
  }

  function handles(options) {
    return options !== null && typeof options === 'object' && options.publicKey !== undefined;
  }

  container.get = function get(options) {
    if (!handles(options)) return nativeGet.apply(this, arguments);
    return intercept('get', nativeGet, this, options);
  };
  container.create = function create(options) {
    if (!handles(options)) return nativeCreate.apply(this, arguments);
    return intercept('create', nativeCreate, this, options);
  };

  PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable = function isUserVerifyingPlatformAuthenticatorAvailable() {
    return Promise.resolve(true);
  };
  PublicKeyCredential.isConditionalMediationAvailable = function isConditionalMediationAvailable() {
    return Promise.resolve(true);
  };
  if (typeof nativeCapabilities === 'function') {
    PublicKeyCredential.getClientCapabilities = async function getClientCapabilities() {
      let capabilities = {};
      try {
        capabilities = await nativeCapabilities.call(PublicKeyCredential);
      } catch {
        // Report the passkey capabilities alone.
      }
      return Object.assign({}, capabilities, {
        conditionalCreate: true,
        conditionalGet: true,
        passkeyPlatformAuthenticator: true,
        userVerifyingPlatformAuthenticator: true,
      });
    };
  }
})();
