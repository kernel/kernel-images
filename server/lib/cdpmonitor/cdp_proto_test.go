package cdpmonitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLayer1Roundtrip verifies that each Layer-1 struct retains every field
// from a captured real-Chrome JSON frame. For each type we decode a fixture
// into the struct, re-marshal the struct, and JSON-compare the two. Any
// non-equivalence is an audit failure — a PDL field has been forgotten,
// silently type-coerced, or mis-tagged.
//
// Fixtures deliberately use non-zero values for all bool/numeric fields
// marked with omitempty so that the round-trip actually exercises retention.
// Chrome never distinguishes "absent" from "zero" on the wire, so omitempty
// is correct; the audit cares about fields with real data being preserved.
func TestLayer1Roundtrip(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		target  func() any
	}{
		{"Runtime.consoleAPICalled", "Runtime_consoleAPICalled.json", func() any { return new(cdpRuntimeConsoleAPICalledParams) }},
		{"Runtime.exceptionThrown", "Runtime_exceptionThrown.json", func() any { return new(cdpRuntimeExceptionThrownParams) }},
		{"Runtime.bindingCalled", "Runtime_bindingCalled.json", func() any { return new(cdpRuntimeBindingCalledParams) }},
		{"Network.requestWillBeSent", "Network_requestWillBeSent.json", func() any { return new(cdpNetworkRequestWillBeSentParams) }},
		{"Network.responseReceived", "Network_responseReceived.json", func() any { return new(cdpNetworkResponseReceivedParams) }},
		{"Network.loadingFinished", "Network_loadingFinished.json", func() any { return new(cdpNetworkLoadingFinishedParams) }},
		{"Network.loadingFailed", "Network_loadingFailed.json", func() any { return new(cdpNetworkLoadingFailedParams) }},
		{"Page.frameNavigated", "Page_frameNavigated.json", func() any { return new(cdpPageFrameNavigatedParams) }},
		{"Page.domContentEventFired", "Page_domContentEventFired.json", func() any { return new(cdpPageDomContentEventFiredParams) }},
		{"Page.loadEventFired", "Page_loadEventFired.json", func() any { return new(cdpPageLoadEventFiredParams) }},
		{"PerformanceTimeline.timelineEventAdded", "PerformanceTimeline_timelineEventAdded.json", func() any { return new(cdpPerformanceTimelineEventAddedParams) }},
		{"Target.attachedToTarget", "Target_attachedToTarget.json", func() any { return new(cdpTargetAttachedToTargetParams) }},
		{"Target.detachedFromTarget", "Target_detachedFromTarget.json", func() any { return new(cdpTargetDetachedFromTargetParams) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			require.NoError(t, err, "read fixture %s", tc.fixture)

			dst := tc.target()
			require.NoError(t, json.Unmarshal(raw, dst), "unmarshal fixture into Layer-1 struct")

			reMarshaled, err := json.Marshal(dst)
			require.NoError(t, err, "re-marshal Layer-1 struct")

			require.JSONEq(t, string(raw), string(reMarshaled),
				"Layer-1 struct dropped or mis-typed a field — diff fixture vs. re-marshaled output to find the missing field")
		})
	}
}
