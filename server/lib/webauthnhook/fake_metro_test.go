package webauthnhook

import (
	"bytes"
	"cmp"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

type metroRequest struct {
	op   string
	auth string
	body Request
}

// fakeMetro stands in for metro-api: in "sign" mode it registers one ES256
// passkey and signs assertions with it, building client data from the
// origins it is sent.
type fakeMetro struct {
	mu          sync.Mutex
	mode        string
	requests    []metroRequest
	credentials []json.RawMessage
	key         *ecdsa.PrivateKey
	keyID       []byte
	userHandle  []byte
}

func newFakeMetro() *fakeMetro { return &fakeMetro{mode: "sign"} }

func (f *fakeMetro) setMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *fakeMetro) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeMetro) last(t *testing.T) metroRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.requests)
	return f.requests[len(f.requests)-1]
}

func (f *fakeMetro) lastCredential() json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentials[len(f.credentials)-1]
}

func (f *fakeMetro) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/webauthn/")
	var body Request
	if r.Method != http.MethodPost || (op != "get" && op != "create") || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, metroRequest{op: op, auth: r.Header.Get("Authorization"), body: body})
	var answer any
	switch f.mode {
	case "unavailable":
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	case "not_handled":
		answer = notHandled
	case "error":
		answer = Answer{Result: "error", Name: "InvalidStateError", Message: "credential already registered"}
	default:
		credential, err := f.sign(op, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.credentials = append(f.credentials, credential)
		answer = Answer{Result: "credential", Credential: credential}
	}
	_ = json.NewEncoder(w).Encode(answer)
}

func (f *fakeMetro) sign(op string, req Request) (json.RawMessage, error) {
	var options struct {
		Challenge string `json:"challenge"`
		RPID      string `json:"rpId"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(req.PublicKey, &options); err != nil {
		return nil, err
	}
	origin, err := url.Parse(req.Origin)
	if err != nil {
		return nil, err
	}
	rpID := cmp.Or(options.RPID, options.RP.ID, origin.Hostname())
	clientData := map[string]any{"type": "webauthn." + op, "challenge": options.Challenge, "origin": req.Origin, "crossOrigin": req.Origin != req.TopOrigin}
	if req.Origin != req.TopOrigin {
		clientData["topOrigin"] = req.TopOrigin
	}
	clientDataJSON, _ := json.Marshal(clientData)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append(rpHash[:], flagUP|flagUV|flagBE|flagBS, 0, 0, 0, 0)

	if op == "create" {
		f.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		f.keyID = make([]byte, 32)
		_, _ = rand.Read(f.keyID)
		if f.userHandle, err = base64.RawURLEncoding.DecodeString(options.User.ID); err != nil {
			return nil, err
		}
		cose := cborMap([][2][]byte{
			{cborInt(1), cborInt(2)},
			{cborInt(3), cborInt(-7)},
			{cborInt(-1), cborInt(1)},
			{cborInt(-2), cborBytes(f.key.X.FillBytes(make([]byte, 32)))},
			{cborInt(-3), cborBytes(f.key.Y.FillBytes(make([]byte, 32)))},
		})
		authData[32] |= flagAT
		authData = append(authData, make([]byte, 16)...)
		authData = binary.BigEndian.AppendUint16(authData, uint16(len(f.keyID)))
		authData = append(append(authData, f.keyID...), cose...)
		attestation := cborMap([][2][]byte{
			{cborText("fmt"), cborText("none")},
			{cborText("attStmt"), cborMap(nil)},
			{cborText("authData"), cborBytes(authData)},
		})
		spki, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"id": b64(f.keyID), "rawId": b64(f.keyID), "type": "public-key", "authenticatorAttachment": "platform",
			"clientExtensionResults": map[string]any{},
			"response": map[string]any{
				"clientDataJSON": b64(clientDataJSON), "attestationObject": b64(attestation), "authenticatorData": b64(authData),
				"publicKey": b64(spki), "publicKeyAlgorithm": -7, "transports": []string{"internal", "hybrid"},
			},
		})
	}
	if f.key == nil {
		return nil, fmt.Errorf("no passkey registered")
	}
	clientHash := sha256.Sum256(clientDataJSON)
	digest := sha256.Sum256(append(bytes.Clone(authData), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64(f.keyID), "rawId": b64(f.keyID), "type": "public-key", "authenticatorAttachment": "platform",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"clientDataJSON": b64(clientDataJSON), "authenticatorData": b64(authData),
			"signature": b64(signature), "userHandle": b64(f.userHandle),
		},
	})
}

// --- minimal CBOR for attestation objects and COSE keys ---

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	default:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
	}
}

func cborInt(n int64) []byte {
	if n < 0 {
		return cborHead(1, uint64(-1-n))
	}
	return cborHead(0, uint64(n))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }

func cborMap(pairs [][2][]byte) []byte {
	out := cborHead(5, uint64(len(pairs)))
	for _, pair := range pairs {
		out = append(append(out, pair[0]...), pair[1]...)
	}
	return out
}

func cborDecode(data []byte) (any, error) {
	value, rest, err := cborItem(data)
	if err == nil && len(rest) != 0 {
		err = fmt.Errorf("cbor: %d trailing bytes", len(rest))
	}
	return value, err
}

func cborItem(data []byte) (any, []byte, error) {
	if len(data) == 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	major, info := data[0]>>5, data[0]&0x1f
	data = data[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info == 24 && len(data) >= 1:
		n, data = uint64(data[0]), data[1:]
	case info == 25 && len(data) >= 2:
		n, data = uint64(binary.BigEndian.Uint16(data)), data[2:]
	default:
		return nil, nil, fmt.Errorf("cbor: unsupported head %#x", info)
	}
	switch major {
	case 0:
		return int64(n), data, nil
	case 1:
		return -1 - int64(n), data, nil
	case 2, 3:
		if uint64(len(data)) < n {
			return nil, nil, io.ErrUnexpectedEOF
		}
		if major == 2 {
			return bytes.Clone(data[:n]), data[n:], nil
		}
		return string(data[:n]), data[n:], nil
	case 5:
		out := make(map[any]any, n)
		for range n {
			key, rest, err := cborItem(data)
			if err != nil {
				return nil, nil, err
			}
			value, rest, err := cborItem(rest)
			if err != nil {
				return nil, nil, err
			}
			out[key], data = value, rest
		}
		return out, data, nil
	}
	return nil, nil, fmt.Errorf("cbor: unsupported major type %d", major)
}
