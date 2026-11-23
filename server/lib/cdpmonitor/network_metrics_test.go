package cdpmonitor

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func failureTotal(s NetworkSnapshot) uint64 {
	var total uint64
	for _, counts := range s.Failures {
		total += counts[0] + counts[1]
	}
	return total
}

func assertNetworkTotals(t *testing.T, s NetworkSnapshot, failures, completed uint64) {
	t.Helper()
	require.Equal(t, failures, failureTotal(s))
	require.Equal(t, completed, s.Completed)
	require.LessOrEqual(t, failures, completed)
	resets := s.Failures["ERR_CONNECTION_RESET"]
	require.Equal(t, s.Resets, resets[0]+resets[1])
}

func TestNetworkErrorCode(t *testing.T) {
	// Every supported code is tested exactly, without accepting prefixes,
	// suffixes, whitespace, or alternate casing from arbitrary error text.
	codes := strings.Fields(`FAILED ABORTED TIMED_OUT ACCESS_DENIED INSUFFICIENT_RESOURCES
		BLOCKED_BY_CLIENT BLOCKED_BY_ADMINISTRATOR BLOCKED_BY_RESPONSE BLOCKED_BY_CSP BLOCKED_BY_ORB
		CLEARTEXT_NOT_PERMITTED NETWORK_ACCESS_REVOKED BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS LOCAL_NETWORK_PERMISSION_MISSING
		NETWORK_CHANGED INTERNET_DISCONNECTED CONNECTION_CLOSED CONNECTION_RESET CONNECTION_REFUSED
		CONNECTION_ABORTED CONNECTION_FAILED CONNECTION_TIMED_OUT ADDRESS_INVALID ADDRESS_UNREACHABLE NETWORK_ACCESS_DENIED
		NAME_NOT_RESOLVED NAME_RESOLUTION_FAILED DNS_MALFORMED_RESPONSE DNS_SERVER_REQUIRES_TCP DNS_TIMED_OUT
		DNS_SECURE_RESOLVER_HOSTNAME_RESOLUTION_FAILED DNS_REQUEST_CANCELLED SSL_PROTOCOL_ERROR SSL_VERSION_OR_CIPHER_MISMATCH
		SSL_CLIENT_AUTH_CERT_NEEDED BAD_SSL_CLIENT_AUTH_CERT SSL_PINNED_KEY_NOT_IN_CERT_CHAIN
		CERT_COMMON_NAME_INVALID CERT_DATE_INVALID CERT_AUTHORITY_INVALID CERT_CONTAINS_ERRORS CERT_NO_REVOCATION_MECHANISM
		CERT_UNABLE_TO_CHECK_REVOCATION CERT_REVOKED CERT_INVALID CERT_WEAK_SIGNATURE_ALGORITHM CERT_NON_UNIQUE_NAME CERT_WEAK_KEY
		CERT_NAME_CONSTRAINT_VIOLATION CERT_VALIDITY_TOO_LONG CERT_KNOWN_INTERCEPTION_BLOCKED PROXY_CONNECTION_FAILED
		TUNNEL_CONNECTION_FAILED PROXY_AUTH_UNSUPPORTED PROXY_AUTH_REQUESTED PROXY_CERTIFICATE_INVALID
		MANDATORY_PROXY_CONFIGURATION_FAILED PROXY_UNABLE_TO_CONNECT_TO_DESTINATION SOCKS_CONNECTION_FAILED SOCKS_CONNECTION_HOST_UNREACHABLE
		HTTP2_PROTOCOL_ERROR HTTP2_SERVER_REFUSED_STREAM HTTP2_PING_FAILED HTTP2_INADEQUATE_TRANSPORT_SECURITY HTTP2_FLOW_CONTROL_ERROR
		HTTP2_FRAME_SIZE_ERROR HTTP2_COMPRESSION_ERROR HTTP2_STREAM_CLOSED INCOMPLETE_HTTP2_HEADERS HTTP2_RST_STREAM_NO_ERROR_RECEIVED
		QUIC_PROTOCOL_ERROR QUIC_HANDSHAKE_FAILED QUIC_CERT_ROOT_NOT_KNOWN EMPTY_RESPONSE CONTENT_LENGTH_MISMATCH
		INCOMPLETE_CHUNKED_ENCODING CONTENT_DECODING_FAILED INVALID_RESPONSE TOO_MANY_REDIRECTS UNSAFE_REDIRECT UNSAFE_PORT INVALID_URL UNKNOWN_URL_SCHEME`)
	c := newNetworkCounters()
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			text := "net::ERR_" + code
			require.Equal(t, "ERR_"+code, networkErrorCode(text))
			for _, invalid := range []string{text + " extra", " " + text, strings.ToLower(text), "ERR_" + code} {
				require.Equal(t, "other", networkErrorCode(invalid))
			}
			for _, canceled := range []bool{false, true} {
				id := fmt.Sprintf("%s-%t", code, canceled)
				c.terminal("s", id, networkFailed, text, canceled)
				c.terminal("s", id, networkFailed, text, canceled)
			}
			require.Equal(t, [2]uint64{1, 1}, c.snapshot().Failures["ERR_"+code])
		})
	}
	for i, text := range []string{"", "arbitrary https://example.invalid/private", "net::ERR_FUTURE_ERROR", strings.Repeat("x", 10000)} {
		want := "other"
		if text == "" {
			want = "unknown"
		}
		require.Equal(t, want, networkErrorCode(text))
		c.terminal("s", fmt.Sprint(i), networkFailed, text, true)
	}
	require.Len(t, c.snapshot().Failures, len(codes)+2)
	assertNetworkTotals(t, c.snapshot(), uint64(2*len(codes)+4), uint64(2*len(codes)+4))
}

func TestNetworkCounters(t *testing.T) {
	c := newNetworkCounters()
	assertNetworkTotals(t, c.snapshot(), 0, 0)
	require.False(t, c.snapshot().Up)
	require.Len(t, c.snapshot().Failures, 85)
	c.terminal("session", "r", networkFailed, "net::ERR_CONNECTION_RESET", false)
	c.terminal("session", "r", networkFailed, "net::ERR_ABORTED", true)
	c.terminal("session", "r", networkFinished, "", false)
	c.terminal("worker", "r", networkFailed, "net::ERR_CONNECTION_RESET", false)
	assertNetworkTotals(t, c.snapshot(), 2, 2)
	before := c.snapshot()
	c.newGeneration()
	require.Equal(t, before, c.snapshot())
	c.terminal("session", "r", networkFailed, "net::ERR_CONNECTION_RESET", false)
	c.terminal("s", "first-wins", networkFinished, "net::ERR_CONNECTION_RESET", true)
	c.terminal("s", "first-wins", networkFailed, "net::ERR_CONNECTION_RESET", false)
	assertNetworkTotals(t, c.snapshot(), 3, 4)
	// Callers may mutate a snapshot without changing counters or racing events.
	before.Failures["ERR_CONNECTION_RESET"] = [2]uint64{99, 99}
	assertNetworkTotals(t, c.snapshot(), 3, 4)
}

func TestNetworkCountersBounded(t *testing.T) {
	c := newNetworkCounters()
	for i := range terminalHistorySize + 100 {
		c.terminal("s", fmt.Sprint(i), networkFailed, fmt.Sprint(i), false)
	}
	require.Len(t, c.seen, terminalHistorySize)
	require.Len(t, c.snapshot().Failures, 85)
	// FIFO eviction intentionally ends the deduplication guarantee.
	c.terminal("s", "0", networkFailed, "", true)
	assertNetworkTotals(t, c.snapshot(), terminalHistorySize+101, terminalHistorySize+101)
	c.newGeneration()
	require.Empty(t, c.seen)
	before := c.snapshot()
	for _, key := range []networkRequestKey{{"", "r"}, {"s", ""}, {strings.Repeat("s", 257), "r"}, {"s", strings.Repeat("r", 257)}} {
		c.terminal(key.sessionID, key.requestID, networkFailed, "net::ERR_CONNECTION_RESET", false)
	}
	require.Equal(t, before, c.snapshot())
}

func TestNetworkCountersConcurrent(t *testing.T) {
	c := newNetworkCounters()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				c.terminal("s", fmt.Sprint(i), networkFailed, "net::ERR_CONNECTION_RESET", i%2 == 0)
				s := c.snapshot()
				if s.Resets != s.Completed || failureTotal(s) != s.Completed {
					t.Errorf("inconsistent counter snapshot: %+v", s)
					return
				}
			}
		})
	}
	wg.Wait()
	assertNetworkTotals(t, c.snapshot(), 1000, 1000)
	require.Equal(t, [2]uint64{500, 500}, c.snapshot().Failures["ERR_CONNECTION_RESET"])
}

func TestMetricsOnlyDispatch(t *testing.T) {
	m := New(newTestUpstream(""), newEventCollector().publishFn(), 0, discardLogger, nil)
	require.NoError(t, m.SetTelemetry(false))
	for range 3 {
		m.dispatchEvent(cdpMessage{Method: "Network.requestWillBeSent", SessionID: "s", Params: []byte(`{"requestId":"r","redirectResponse":{}}`)})
	}
	for _, test := range []struct {
		kind, fields, code string
		canceled           bool
	}{
		{"loadingFailed", `"errorText":"net::ERR_CONNECTION_RESET"`, "ERR_CONNECTION_RESET", false},
		{"loadingFailed", `"errorText":""`, "unknown", false},
		{"loadingFailed", `"canceled":true`, "unknown", true},
		{"loadingFailed", `"errorText":"unrecognized","canceled":true`, "other", true},
		{"loadingFailed", `"blockedReason":"csp"`, "unknown", false},
		{"loadingFailed", `"errorText":"net::ERR_BLOCKED_BY_CLIENT","blockedReason":"other"`, "ERR_BLOCKED_BY_CLIENT", false},
		{"loadingFinished", `"errorText":"net::ERR_CONNECTION_RESET","canceled":true,"blockedReason":"csp"`, "", false},
	} {
		id := test.kind + test.fields
		params := []byte(fmt.Sprintf(`{"requestId":%q,%s}`, id, test.fields))
		before := m.NetworkSnapshot()
		for range 2 {
			m.dispatchEvent(cdpMessage{Method: "Network." + test.kind, SessionID: "s", Params: params})
		}
		after := m.NetworkSnapshot()
		wantFailures := failureTotal(before)
		if test.code != "" {
			wantFailures++
			index := 0
			if test.canceled {
				index = 1
			}
			require.Equal(t, before.Failures[test.code][index]+1, after.Failures[test.code][index])
		}
		assertNetworkTotals(t, after, wantFailures, before.Completed+1)
	}
	assertNetworkTotals(t, m.NetworkSnapshot(), 6, 7)
	require.Empty(t, m.pendingRequests)
	require.Empty(t, m.computedStates)
}
