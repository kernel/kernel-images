package webauthnhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/cdpmonitor"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/stretchr/testify/require"
)

// TestChromiumPageHook drives the hook through a real Chromium over its own
// DevTools connection, answered by a fake metro-api that signs with ES256. A
// telemetry monitor runs alongside it on a separate connection.
// Sites are served over HTTPS under example.test and other.test, so the
// cross-site iframe runs out of process.
//
// Opt-in: set KERNEL_WEBAUTHN_CHROME_E2E=1 with a chromium binary on PATH.
// The fallback cases each wait out the hook's 10-second window.
func TestChromiumPageHook(t *testing.T) {
	if os.Getenv("KERNEL_WEBAUTHN_CHROME_E2E") == "" {
		t.Skip("set KERNEL_WEBAUTHN_CHROME_E2E=1 to run the real-Chromium WebAuthn hook test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := newChromeEnv(t, ctx)
	login := env.site("login.example.test")

	var registered pageCredential
	t.Run("create registers a passkey through the signer", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		registered = result.Credential
		requireHookCredential(t, registered, "attestation")
		require.Equal(t, -7, registered.Algorithm)
		require.Equal(t, fakeTransports, registered.Transports)

		request := env.metro.last(t)
		require.Equal(t, "create", request.op)
		require.Equal(t, "Bearer instance-jwt", request.auth)
		require.Equal(t, login, request.body.Origin)
		require.Equal(t, login, request.body.TopOrigin)
		require.Empty(t, request.body.Mediation)
		var options struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			PubKeyCredParams []struct {
				Alg int `json:"alg"`
			} `json:"pubKeyCredParams"`
		}
		require.NoError(t, json.Unmarshal(request.body.PublicKey, &options))
		require.Equal(t, b64(challenge), options.Challenge)
		require.Equal(t, b64(userID), options.User.ID)
		require.Equal(t, "alice", options.User.Name)
		require.Equal(t, -7, options.PubKeyCredParams[0].Alg)

		require.JSONEq(t, string(env.metro.lastCredential()), string(registered.JSON))
		verifyAttestation(t, registered, "login.example.test")
	})

	t.Run("get signs in with the registered passkey", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		result := tab.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
		require.Equal(t, registered.ID, result.Credential.ID)
		require.Equal(t, b64(userID), result.Credential.UserHandle)
		require.JSONEq(t, string(env.metro.lastCredential()), string(result.Credential.JSON))
		verifyAssertion(t, registered, result.Credential, "login.example.test", login, false)

		request := env.metro.last(t)
		require.Equal(t, "get", request.op)
		require.Equal(t, login, request.body.Origin)
		require.Contains(t, string(request.body.PublicKey), `"id":"`+registered.ID+`"`)
	})

	t.Run("conditional get at page load is answered", func(t *testing.T) {
		before := env.metro.count()
		tab := env.open(t, login+"/conditional")
		result := tab.settle(t, "window.conditional")
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
		require.Equal(t, before+1, env.metro.count())
		require.Equal(t, "conditional", env.metro.last(t).body.Mediation)
	})

	// Decision pinned here: targets are not paused on attach, so a new tab's
	// first document runs its load-time script before the hook is installed.
	// The call goes to the browser; a reload is answered.
	t.Run("a new tab's first document is unhooked at load, and a reload is answered", func(t *testing.T) {
		before := env.metro.count()
		tab := env.openDirect(t, login+"/conditional")
		tab.waitHooked(t)
		require.JSONEq(t, `"pending"`, string(tab.eval(t,
			`Promise.race([window.conditional.then(() => 'answered', () => 'rejected'), new Promise((r) => setTimeout(() => r('pending'), 500))])`)))
		require.Equal(t, before, env.metro.count())

		tab.env.driver.call(t, tab.sessionID, "Page.reload", nil)
		require.Eventually(t, func() bool {
			value, err := tab.evalRaw(`document.readyState === 'complete' && typeof window.conditional === 'object'`)
			return err == nil && string(value) == "true"
		}, 15*time.Second, 50*time.Millisecond)
		result := tab.settle(t, "window.conditional")
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
		require.Equal(t, before+1, env.metro.count())
	})

	t.Run("capabilities report a platform passkey authenticator", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		value := tab.eval(t, `Promise.all([
			PublicKeyCredential.isConditionalMediationAvailable(),
			PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable(),
			PublicKeyCredential.getClientCapabilities(),
		]).then(([conditional, uvpa, capabilities]) => ({conditional, uvpa, capabilities}))`)
		var capabilities struct {
			Conditional  bool            `json:"conditional"`
			UVPA         bool            `json:"uvpa"`
			Capabilities map[string]bool `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(value, &capabilities))
		require.True(t, capabilities.Conditional)
		require.True(t, capabilities.UVPA)
		for _, key := range []string{"conditionalGet", "conditionalCreate", "passkeyPlatformAuthenticator", "userVerifyingPlatformAuthenticator"} {
			require.True(t, capabilities.Capabilities[key], key)
		}
	})

	t.Run("an error answer rejects with that DOMException", func(t *testing.T) {
		env.metro.setMode("error")
		defer env.metro.setMode("sign")
		tab := env.open(t, login+"/page")
		result := tab.settle(t, createCall)
		require.Equal(t, "InvalidStateError", result.Error)
		require.Equal(t, "credential already registered", result.Message)
		require.True(t, result.DOMException)
	})

	t.Run("not_handled falls back to the browser", func(t *testing.T) {
		env.metro.setMode("not_handled")
		defer env.metro.setMode("sign")
		tab := env.open(t, login+"/page")
		tab.addVirtualAuthenticator(t)
		before := env.metro.count()
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		require.False(t, result.Credential.Own, "the browser's own credential has no own properties")
		require.True(t, result.Credential.Instance)
		require.Equal(t, before+1, env.metro.count())
	})

	t.Run("non-publicKey requests go straight to the browser", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		before := env.metro.count()
		require.JSONEq(t, `"null"`, string(tab.eval(t, `navigator.credentials.get({password: true, mediation: 'silent'}).then(String)`)))
		require.JSONEq(t, `"[object PasswordCredential]"`, string(tab.eval(t,
			`navigator.credentials.create({password: {id: 'alice', password: 'secret'}}).then(c => Object.prototype.toString.call(c))`)))
		require.Equal(t, before, env.metro.count())
	})

	t.Run("an aborted signal rejects without asking the signer", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		before := env.metro.count()
		result := tab.settle(t, `(() => {
			const controller = new AbortController();
			controller.abort();
			return navigator.credentials.get({signal: controller.signal, publicKey: {challenge: new Uint8Array(32)}});
		})()`)
		require.Equal(t, "AbortError", result.Error)
		require.Equal(t, before, env.metro.count())
	})

	t.Run("a cross-site iframe sends its own origin and the top origin", func(t *testing.T) {
		widget := env.site("widget.other.test")
		tab := env.open(t, login+"/page?frame="+url.QueryEscape(widget+"/page")+"&allow=publickey-credentials-get")
		frame := tab.oopif(t, widget)
		result := frame.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
		request := env.metro.last(t)
		require.Equal(t, widget, request.body.Origin)
		require.Equal(t, login, request.body.TopOrigin)
		verifyAssertion(t, registered, result.Credential, "widget.other.test", widget, true)
	})

	t.Run("a nested cross-site iframe gets the page's origin as top origin", func(t *testing.T) {
		widget, deep := env.site("widget.other.test"), env.site("deep.third.test")
		inner := deep + "/page"
		outer := widget + "/page?frame=" + url.QueryEscape(inner) + "&allow=publickey-credentials-get"
		tab := env.open(t, login+"/page?frame="+url.QueryEscape(outer)+"&allow=publickey-credentials-get")
		frame := tab.oopif(t, deep)
		result := frame.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
		request := env.metro.last(t)
		require.Equal(t, deep, request.body.Origin)
		require.Equal(t, login, request.body.TopOrigin)
	})

	t.Run("a same-site cross-origin iframe sends its own origin", func(t *testing.T) {
		other := env.site("other.example.test")
		tab := env.open(t, login+"/page?frame="+url.QueryEscape(other+"/page")+"&allow=publickey-credentials-get")
		frame := tab.childFrame(t, other)
		result := frame.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		request := env.metro.last(t)
		require.Equal(t, other, request.body.Origin)
		require.Equal(t, login, request.body.TopOrigin)
	})

	t.Run("an iframe without permission is left to the browser", func(t *testing.T) {
		widget := env.site("widget.other.test")
		tab := env.open(t, login+"/page?frame="+url.QueryEscape(widget+"/page"))
		frame := tab.oopif(t, widget)
		before := env.metro.count()
		result := frame.settle(t, pageGet(registered.ID))
		require.Equal(t, "NotAllowedError", result.Error)
		require.Equal(t, before, env.metro.count())
	})

	t.Run("telemetry capture on and off leaves the hook answering", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		interaction := func(want string) {
			require.Eventually(t, func() bool {
				value, err := tab.evalRaw(`window.__kernelEventInjected === true`)
				return err == nil && string(value) == want
			}, 15*time.Second, 50*time.Millisecond, "telemetry interaction capture did not reach %s", want)
		}
		answers := func() {
			result := tab.settle(t, pageGet(registered.ID))
			require.Empty(t, result.Error)
			requireHookCredential(t, result.Credential, "assertion")
		}

		require.NoError(t, env.monitor.SetTelemetry(true))
		interaction("true")
		answers()

		// Telemetry cleanup disables Runtime and Page on its own sessions.
		require.NoError(t, env.monitor.SetTelemetry(false))
		interaction("false")
		answers()
		tab.navigate(t, login+"/page?after-telemetry")
		tab.waitHooked(t)
		answers()
	})

	t.Run("a missing binding falls back after three seconds", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		tab.addVirtualAuthenticator(t)
		tab.eval(t, `delete window.__kernelPasskey`)
		started := time.Now()
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		require.False(t, result.Credential.Own)
		require.GreaterOrEqual(t, time.Since(started), 3*time.Second)
		require.Less(t, time.Since(started), 6*time.Second)
	})

	t.Run("an abort stops kernel-images asking metro-api", func(t *testing.T) {
		env.metro.setMode("unavailable")
		defer env.metro.setMode("sign")
		tab := env.open(t, login+"/page")
		before := env.metro.count()
		result := tab.settle(t, `(() => {
			const controller = new AbortController();
			const promise = navigator.credentials.get({signal: controller.signal, publicKey: {challenge: new Uint8Array(32)}});
			setTimeout(() => controller.abort(), 300);
			return promise;
		})()`)
		require.Equal(t, "AbortError", result.Error)
		attempts := env.metro.count() - before
		time.Sleep(3 * time.Second)
		require.Equal(t, attempts, env.metro.count()-before, "an aborted request stops retrying")
	})

	// A request whose document is gone stops retrying metro-api, like an abort.
	for _, leave := range []struct {
		name string
		do   func(t *testing.T, tab *tab)
	}{
		{"navigating away", func(t *testing.T, tab *tab) { tab.navigate(t, login+"/page?away") }},
		{"closing the tab", func(t *testing.T, tab *tab) {
			env.driver.call(t, "", "Target.closeTarget", map[string]any{"targetId": tab.targetID})
		}},
	} {
		t.Run(leave.name+" stops kernel-images asking metro-api", func(t *testing.T) {
			env.metro.setMode("unavailable")
			defer env.metro.setMode("sign")
			tab := env.open(t, login+"/page")
			before := env.metro.count()
			tab.eval(t, `navigator.credentials.get({publicKey: {challenge: new Uint8Array(32)}}).catch(() => {}), 1`)
			require.Eventually(t, func() bool { return env.metro.count() > before }, 5*time.Second, 20*time.Millisecond)
			leave.do(t, tab)
			time.Sleep(500 * time.Millisecond)
			attempts := env.metro.count()
			time.Sleep(3 * time.Second)
			require.Equal(t, attempts, env.metro.count(), "a request whose document is gone stops retrying")
		})
	}

	t.Run("an unavailable signer falls back after the window", func(t *testing.T) {
		env.metro.setMode("unavailable")
		defer env.metro.setMode("sign")
		tab := env.open(t, login+"/page")
		tab.addVirtualAuthenticator(t)
		started := time.Now()
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		require.False(t, result.Credential.Own)
		require.GreaterOrEqual(t, time.Since(started), signerWindow)
	})

	t.Run("a new upstream URL re-dials and the same document is answered", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		before := env.hook.currentConnection()
		env.upstream.set(env.browser, true)
		require.Eventually(t, func() bool {
			conn := env.hook.currentConnection()
			return conn != nil && conn != before
		}, 15*time.Second, 50*time.Millisecond, "the hook did not reconnect")
		result := tab.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
	})

	// The tracker detaches a session whose initialization fails; the hook's
	// periodic refresh attaches the target again, without a reload.
	t.Run("a detached hook session is attached again and answers the same document", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		conn := env.hook.current()
		var sessionID string
		conn.mu.Lock()
		for id, s := range conn.sessions {
			if s.targetID == tab.targetID && s.targetType == "page" {
				sessionID = id
			}
		}
		conn.mu.Unlock()
		require.NotEmpty(t, sessionID)
		_, err := conn.protocol.Send(ctx, "Target.detachFromTarget", map[string]any{"sessionId": sessionID}, "")
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			conn.mu.Lock()
			defer conn.mu.Unlock()
			return conn.sessions[sessionID] == nil
		}, 5*time.Second, 20*time.Millisecond)

		result := tab.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
	})

	t.Run("a lost connection falls back, and a new one answers the same document", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		env.upstream.set("", false)
		_ = env.hook.currentConnection().protocol.Close()
		require.Eventually(t, func() bool { return env.hook.currentConnection() == nil }, 5*time.Second, 50*time.Millisecond)
		tab.addVirtualAuthenticator(t)
		before := env.metro.count()
		started := time.Now()
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		require.False(t, result.Credential.Own)
		require.GreaterOrEqual(t, time.Since(started), 10*time.Second)
		require.Equal(t, before, env.metro.count())

		env.upstream.set(env.browser, true)
		result = tab.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
	})
}

var (
	challenge = []byte("0123456789abcdef0123456789abcdef")
	userID    = []byte{1, 2, 3, 4}
)

const hookedExpression = `Object.prototype.hasOwnProperty.call(window, '__kernelPasskeyReply')`

var createCall = fmt.Sprintf(`navigator.credentials.create({publicKey: {
	challenge: unb64(%q),
	rp: {name: 'Example'},
	user: {id: unb64(%q), name: 'alice', displayName: 'Alice'},
	pubKeyCredParams: [{type: 'public-key', alg: -7}],
}})`, b64(challenge), b64(userID))

func pageGet(credentialID string) string {
	return fmt.Sprintf(`navigator.credentials.get({publicKey: {
		challenge: unb64(%q),
		allowCredentials: [{type: 'public-key', id: unb64(%q)}],
	}})`, b64(challenge), credentialID)
}

const pageHelpers = `<script>
function b64(buffer) {
  let binary = '';
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function unb64(text) {
  const binary = atob(text.replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}
function summarize(c) {
  const r = c.response;
  const out = {
    instance: c instanceof PublicKeyCredential && r instanceof AuthenticatorResponse,
    tag: Object.prototype.toString.call(c),
    own: Object.getOwnPropertyNames(c).length > 0,
    json: c.toJSON(),
    id: c.id, type: c.type, attachment: c.authenticatorAttachment, rawId: b64(c.rawId),
    extensions: c.getClientExtensionResults(),
    clientDataJSON: b64(r.clientDataJSON),
  };
  if (r instanceof AuthenticatorAttestationResponse) {
    Object.assign(out, {
      kind: 'attestation', attestationObject: b64(r.attestationObject), authenticatorData: b64(r.getAuthenticatorData()),
      publicKey: r.getPublicKey() && b64(r.getPublicKey()), algorithm: r.getPublicKeyAlgorithm(), transports: r.getTransports(),
    });
  } else if (r instanceof AuthenticatorAssertionResponse) {
    Object.assign(out, {
      kind: 'assertion', authenticatorData: b64(r.authenticatorData), signature: b64(r.signature),
      userHandle: r.userHandle && b64(r.userHandle),
    });
  }
  return out;
}
function settle(promise) {
  return promise.then(
    (c) => ({credential: summarize(c)}),
    (e) => ({error: e.name, message: e.message, dom: e instanceof DOMException}));
}
</script>`

type pageCredential struct {
	Instance          bool            `json:"instance"`
	Tag               string          `json:"tag"`
	Own               bool            `json:"own"`
	JSON              json.RawMessage `json:"json"`
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	Attachment        string          `json:"attachment"`
	RawID             string          `json:"rawId"`
	Extensions        map[string]any  `json:"extensions"`
	ClientDataJSON    string          `json:"clientDataJSON"`
	Kind              string          `json:"kind"`
	AttestationObject string          `json:"attestationObject"`
	AuthenticatorData string          `json:"authenticatorData"`
	PublicKey         string          `json:"publicKey"`
	Algorithm         int             `json:"algorithm"`
	Transports        []string        `json:"transports"`
	Signature         string          `json:"signature"`
	UserHandle        string          `json:"userHandle"`
}

type pageResult struct {
	Credential   pageCredential `json:"credential"`
	Error        string         `json:"error"`
	Message      string         `json:"message"`
	DOMException bool           `json:"dom"`
}

// requireHookCredential checks a credential built by the hook: native
// prototypes, and toJSON agreeing with every buffer the page can read.
func requireHookCredential(t *testing.T, c pageCredential, kind string) {
	t.Helper()
	require.True(t, c.Own, "expected a credential from the hook, not the browser")
	require.True(t, c.Instance)
	require.Equal(t, "[object PublicKeyCredential]", c.Tag)
	require.Equal(t, kind, c.Kind)
	require.Equal(t, "public-key", c.Type)
	require.Equal(t, "platform", c.Attachment)
	require.Empty(t, c.Extensions)
	require.Equal(t, c.ID, c.RawID)

	var round struct {
		ID       string         `json:"id"`
		RawID    string         `json:"rawId"`
		Type     string         `json:"type"`
		Response map[string]any `json:"response"`
		Attach   string         `json:"authenticatorAttachment"`
		Ext      map[string]any `json:"clientExtensionResults"`
	}
	require.NoError(t, json.Unmarshal(c.JSON, &round))
	require.Equal(t, c.ID, round.ID)
	require.Equal(t, c.RawID, round.RawID)
	require.Equal(t, "public-key", round.Type)
	require.Equal(t, "platform", round.Attach)
	require.Equal(t, c.ClientDataJSON, round.Response["clientDataJSON"])
	require.Equal(t, c.AuthenticatorData, round.Response["authenticatorData"])
	if kind == "attestation" {
		require.Equal(t, c.AttestationObject, round.Response["attestationObject"])
		require.Equal(t, c.PublicKey, round.Response["publicKey"])
	} else {
		require.Equal(t, c.Signature, round.Response["signature"])
		require.Equal(t, c.UserHandle, round.Response["userHandle"])
	}
}

func verifyAttestation(t *testing.T, c pageCredential, rpID string) {
	t.Helper()
	object, err := cborDecode(unb64(t, c.AttestationObject))
	require.NoError(t, err)
	attestation := object.(map[any]any)
	require.Equal(t, "none", attestation["fmt"])
	require.Empty(t, attestation["attStmt"])
	authData := attestation["authData"].([]byte)
	require.Equal(t, unb64(t, c.AuthenticatorData), authData)

	rpHash := sha256.Sum256([]byte(rpID))
	require.Equal(t, rpHash[:], authData[:32])
	require.Equal(t, byte(flagUP|flagUV|flagBE|flagBS|flagAT), authData[32])
	require.Zero(t, binary.BigEndian.Uint32(authData[33:37]))
	require.Equal(t, make([]byte, 16), authData[37:53])
	idLen := int(binary.BigEndian.Uint16(authData[53:55]))
	require.Equal(t, unb64(t, c.RawID), authData[55:55+idLen])

	key, err := cborDecode(authData[55+idLen:])
	require.NoError(t, err)
	cose := key.(map[any]any)
	public := parsePublicKey(t, c.PublicKey)
	require.Equal(t, int64(2), cose[int64(1)])
	require.Equal(t, int64(-7), cose[int64(3)])
	require.Equal(t, public.X.FillBytes(make([]byte, 32)), cose[int64(-2)])
	require.Equal(t, public.Y.FillBytes(make([]byte, 32)), cose[int64(-3)])

	var clientData map[string]any
	require.NoError(t, json.Unmarshal(unb64(t, c.ClientDataJSON), &clientData))
	require.Equal(t, "webauthn.create", clientData["type"])
	require.Equal(t, b64(challenge), clientData["challenge"])
}

func verifyAssertion(t *testing.T, registered, c pageCredential, rpID, origin string, crossOrigin bool) {
	t.Helper()
	authData := unb64(t, c.AuthenticatorData)
	rpHash := sha256.Sum256([]byte(rpID))
	require.Equal(t, rpHash[:], authData[:32])
	require.Equal(t, byte(flagUP|flagUV|flagBE|flagBS), authData[32])
	require.Zero(t, binary.BigEndian.Uint32(authData[33:37]))

	clientDataJSON := unb64(t, c.ClientDataJSON)
	var clientData map[string]any
	require.NoError(t, json.Unmarshal(clientDataJSON, &clientData))
	require.Equal(t, "webauthn.get", clientData["type"])
	require.Equal(t, origin, clientData["origin"])
	require.Equal(t, crossOrigin, clientData["crossOrigin"])

	clientHash := sha256.Sum256(clientDataJSON)
	digest := sha256.Sum256(append(authData, clientHash[:]...))
	require.True(t, ecdsa.VerifyASN1(parsePublicKey(t, registered.PublicKey), digest[:], unb64(t, c.Signature)), "assertion signature does not verify")
}

func parsePublicKey(t *testing.T, spki string) *ecdsa.PublicKey {
	t.Helper()
	key, err := x509.ParsePKIXPublicKey(unb64(t, spki))
	require.NoError(t, err)
	return key.(*ecdsa.PublicKey)
}

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func unb64(t *testing.T, text string) []byte {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(text)
	require.NoError(t, err)
	return data
}

// --- environment ---

type chromeEnv struct {
	ctx      context.Context
	port     string
	browser  string
	driver   *driver
	metro    *fakeMetro
	monitor  *cdpmonitor.Monitor
	upstream *switchableUpstream
	hook     *Hook
}

func newChromeEnv(t *testing.T, ctx context.Context) *chromeEnv {
	sites := httptest.NewTLSServer(http.HandlerFunc(serveSite))
	t.Cleanup(sites.Close)
	siteURL, err := url.Parse(sites.URL)
	require.NoError(t, err)

	metro := newFakeMetro()
	metroServer := httptest.NewServer(metro)
	t.Cleanup(metroServer.Close)

	browser := launchChromium(t, ctx, "--ignore-certificate-errors", "--host-resolver-rules=MAP *.test 127.0.0.1")
	monitor := cdpmonitor.New(&switchableUpstream{url: browser}, func(events.Event) (events.Envelope, bool) { return events.Envelope{}, false }, 0, discard, nil)
	require.NoError(t, monitor.SetTelemetry(false))
	require.NoError(t, monitor.Start(ctx))
	t.Cleanup(monitor.Stop)

	upstream := &switchableUpstream{url: browser}
	hook := New(ctx, Config{
		StatePath: filepath.Join(t.TempDir(), "webauthn-hook.json"),
		Signer: &MetroSigner{
			BaseURL: func() string { return metroServer.URL },
			Token:   func() string { return "instance-jwt" },
			Client:  metroServer.Client(),
			Logger:  discard,
		},
		Logger: discard,
	}, upstream)
	t.Cleanup(hook.Stop)
	require.NoError(t, hook.Set(ctx, true))

	return &chromeEnv{
		ctx: ctx, port: siteURL.Port(), browser: browser, driver: dialDriver(t, ctx, browser),
		metro: metro, monitor: monitor, upstream: upstream, hook: hook,
	}
}

func (e *chromeEnv) site(host string) string {
	return "https://" + host + ":" + e.port
}

func serveSite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	body := pageHelpers
	switch r.URL.Path {
	case "/page":
		if frame := r.URL.Query().Get("frame"); frame != "" {
			body += fmt.Sprintf(`<iframe src=%q allow=%q></iframe>`, frame, r.URL.Query().Get("allow"))
		}
	case "/conditional":
		body += `<script>window.conditional = (navigator.credentials.get({mediation: 'conditional', publicKey: {challenge: new Uint8Array(32)}}));</script>`
	default:
		http.NotFound(w, r)
		return
	}
	_, _ = io.WriteString(w, "<!doctype html><html><body>"+body+"</body></html>")
}

type tab struct {
	env       *chromeEnv
	targetID  string
	sessionID string
	contextID int
}

func (e *chromeEnv) open(t *testing.T, pageURL string) *tab {
	t.Helper()
	// Start on a page the hook reaches through runImmediately, so its
	// new-document registration exists before the real navigation.
	tb := e.openDirect(t, e.site("login.example.test")+"/page")
	tb.waitHooked(t)
	tb.navigate(t, pageURL)
	tb.waitHooked(t)
	return tb
}

// openDirect opens a tab on pageURL, racing the hook's attachment as a new
// tab would.
func (e *chromeEnv) openDirect(t *testing.T, pageURL string) *tab {
	t.Helper()
	raw := e.driver.call(t, "", "Target.createTarget", map[string]any{"url": pageURL})
	var target struct {
		TargetID string `json:"targetId"`
	}
	require.NoError(t, json.Unmarshal(raw, &target))
	t.Cleanup(func() {
		_, _ = e.driver.client.Send(context.Background(), "Target.closeTarget", map[string]any{"targetId": target.TargetID}, "")
	})
	tb := &tab{env: e, targetID: target.TargetID, sessionID: e.driver.attach(t, target.TargetID)}
	tb.waitLoaded(t, pageURL)
	return tb
}

func (tb *tab) navigate(t *testing.T, pageURL string) {
	t.Helper()
	tb.env.driver.call(t, tb.sessionID, "Page.navigate", map[string]any{"url": pageURL})
	tb.waitLoaded(t, pageURL)
}

func (tb *tab) waitLoaded(t *testing.T, pageURL string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var state string
		value, err := tb.evalRaw("document.readyState === 'complete' && location.href")
		return err == nil && json.Unmarshal(value, &state) == nil && state == pageURL
	}, 15*time.Second, 50*time.Millisecond, "page %s did not load", pageURL)
}

func (tb *tab) waitHooked(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		value, err := tb.evalRaw(hookedExpression)
		return err == nil && string(value) == "true"
	}, 15*time.Second, 50*time.Millisecond, "the hook was not installed")
}

func (tb *tab) evalRaw(expression string) (json.RawMessage, error) {
	params := map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}
	if tb.contextID != 0 {
		params["contextId"] = tb.contextID
	}
	ctx, cancel := context.WithTimeout(tb.env.ctx, 30*time.Second)
	defer cancel()
	raw, err := tb.env.driver.client.Send(ctx, "Runtime.evaluate", params, tb.sessionID)
	if err != nil {
		return nil, err
	}
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil {
		return nil, fmt.Errorf("evaluation threw: %s", result.ExceptionDetails)
	}
	return result.Result.Value, nil
}

func (tb *tab) eval(t *testing.T, expression string) json.RawMessage {
	t.Helper()
	value, err := tb.evalRaw(expression)
	require.NoError(t, err)
	return value
}

func (tb *tab) settle(t *testing.T, expression string) pageResult {
	t.Helper()
	var result pageResult
	require.NoError(t, json.Unmarshal(tb.eval(t, "settle("+expression+")"), &result))
	return result
}

func (tb *tab) addVirtualAuthenticator(t *testing.T) {
	t.Helper()
	tb.env.driver.call(t, tb.sessionID, "WebAuthn.enable", nil)
	tb.env.driver.call(t, tb.sessionID, "WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
		"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
		"isUserVerified": true, "automaticPresenceSimulation": true,
	}})
}

// oopif attaches to the out-of-process iframe showing origin.
func (tb *tab) oopif(t *testing.T, origin string) *tab {
	t.Helper()
	var targetID string
	require.Eventually(t, func() bool {
		raw, err := tb.env.driver.client.Send(tb.env.ctx, "Target.getTargets", nil, "")
		if err != nil {
			return false
		}
		var result struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		_ = json.Unmarshal(raw, &result)
		for _, target := range result.TargetInfos {
			if target.Type == "iframe" && strings.HasPrefix(target.URL, origin+"/") {
				targetID = target.TargetID
				return true
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond, "no out-of-process iframe for %s", origin)
	frame := &tab{env: tb.env, sessionID: tb.env.driver.attach(t, targetID)}
	frame.waitHooked(t)
	return frame
}

// childFrame evaluates in a same-process child frame's main world.
func (tb *tab) childFrame(t *testing.T, origin string) *tab {
	t.Helper()
	tb.env.driver.call(t, tb.sessionID, "Runtime.enable", nil)
	frame := &tab{env: tb.env, sessionID: tb.sessionID}
	require.Eventually(t, func() bool {
		frame.contextID = tb.env.driver.contextFor(tb.sessionID, origin)
		return frame.contextID != 0
	}, 15*time.Second, 50*time.Millisecond, "no same-process frame for %s", origin)
	frame.waitHooked(t)
	return frame
}

// --- CDP driver, independent of the monitor's connection ---

type driver struct {
	ctx    context.Context
	client *cdpclient.Client

	mu       sync.Mutex
	contexts map[string]map[int]string // session -> context ID -> origin, main worlds only
}

func dialDriver(t *testing.T, ctx context.Context, browser string) *driver {
	client, err := cdpclient.DialWithEvents(ctx, browser)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	d := &driver{ctx: ctx, client: client, contexts: make(map[string]map[int]string)}
	go func() {
		for message := range client.Events() {
			if message.Method != "Runtime.executionContextCreated" {
				continue
			}
			var event struct {
				Context struct {
					ID      int    `json:"id"`
					Origin  string `json:"origin"`
					AuxData struct {
						IsDefault bool `json:"isDefault"`
					} `json:"auxData"`
				} `json:"context"`
			}
			if json.Unmarshal(message.Params, &event) == nil && event.Context.AuxData.IsDefault {
				d.mu.Lock()
				if d.contexts[message.SessionID] == nil {
					d.contexts[message.SessionID] = make(map[int]string)
				}
				d.contexts[message.SessionID][event.Context.ID] = event.Context.Origin
				d.mu.Unlock()
			}
		}
	}()
	return d
}

func (d *driver) call(t *testing.T, sessionID, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	raw, err := d.client.Send(ctx, method, params, sessionID)
	require.NoError(t, err, method)
	return raw
}

func (d *driver) attach(t *testing.T, targetID string) string {
	t.Helper()
	var result struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(d.call(t, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}), &result))
	return result.SessionID
}

func (d *driver) contextFor(sessionID, origin string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, contextOrigin := range d.contexts[sessionID] {
		if contextOrigin == origin {
			return id
		}
	}
	return 0
}

func (h *Hook) currentConnection() *connection {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

var devToolsURL = regexp.MustCompile(`ws://\S+`)

func launchChromium(t *testing.T, ctx context.Context, extraArgs ...string) string {
	t.Helper()
	var chrome string
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("chromium not found on PATH")
	}
	args := append([]string{
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--remote-debugging-port=0", "--user-data-dir=" + t.TempDir(),
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, chrome, append(args, "about:blank")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	found := make(chan string, 1)
	go func() {
		var seen []byte
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			seen = append(seen, buf[:n]...)
			if match := devToolsURL.Find(seen); match != nil {
				found <- string(match)
				_, _ = io.Copy(io.Discard, stderr)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case url := <-found:
		return url
	case <-time.After(time.Minute):
		t.Fatal("chromium did not report a DevTools URL")
		return ""
	}
}

// TestChromiumPageHookDisable turns the hook off and on again in its own
// browser, since that changes state the other cases rely on.
func TestChromiumPageHookDisable(t *testing.T) {
	if os.Getenv("KERNEL_WEBAUTHN_CHROME_E2E") == "" {
		t.Skip("set KERNEL_WEBAUTHN_CHROME_E2E=1 to run the real-Chromium WebAuthn hook test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := newChromeEnv(t, ctx)
	login := env.site("login.example.test")
	created := env.open(t, login+"/page").settle(t, createCall)
	require.Empty(t, created.Error)
	registered := created.Credential

	t.Run("disabling suspends hooked documents and stops new ones from getting the hook", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		require.NoError(t, env.hook.Set(ctx, false))
		defer func() { require.NoError(t, env.hook.Set(ctx, true)) }()
		// Chromium drops a tab's virtual authenticators when another DevTools
		// session on it detaches, so add one only after the hook's has.
		tab.addVirtualAuthenticator(t)

		before := env.metro.count()
		started := time.Now()
		result := tab.settle(t, createCall)
		require.Empty(t, result.Error)
		require.False(t, result.Credential.Own, "an already-hooked document falls back to the browser")
		require.Less(t, time.Since(started), 2*time.Second, "a suspended document falls back without waiting")
		require.Equal(t, before, env.metro.count())

		tab.navigate(t, login+"/page?reloaded")
		require.JSONEq(t, `false`, string(tab.eval(t, hookedExpression)))
		require.JSONEq(t, `true`, string(tab.eval(t, `CredentialsContainer.prototype.get.toString().includes('[native code]')`)))
	})

	t.Run("re-enabling resumes a suspended document", func(t *testing.T) {
		tab := env.open(t, login+"/page")
		require.NoError(t, env.hook.Set(ctx, false))
		require.NoError(t, env.hook.Set(ctx, true))
		result := tab.settle(t, pageGet(registered.ID))
		require.Empty(t, result.Error)
		requireHookCredential(t, result.Credential, "assertion")
	})
}
