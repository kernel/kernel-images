package events

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// The api_call/platform_api_call split is only meaningful if the operation map
// and the event-type map agree on which category each type carries.
func TestCategoryMapsAgreeOnAPICallSplit(t *testing.T) {
	cat, ok := CategoryForType("api_call")
	require.True(t, ok)
	assert.Equal(t, Control, cat)

	cat, ok = CategoryForType("platform_api_call")
	require.True(t, ok)
	assert.Equal(t, Platform, cat)
}

func TestCaptchaCategories(t *testing.T) {
	for _, eventType := range []string{"captcha_solve_started", "captcha_challenge_result"} {
		t.Run(eventType, func(t *testing.T) {
			cat, ok := CategoryForType(eventType)
			require.True(t, ok)
			assert.Equal(t, Captcha, cat)
		})
	}
}

func TestCategoryForOperation(t *testing.T) {
	require.NotEmpty(t, categoryByOperationID, "generator produced no operations")

	// Sampled rather than exhaustive: the spec is the source of truth for the
	// full mapping, and restating all of it here would just be a second copy.
	control, ok := CategoryForOperation("ExecutePlaywrightCode")
	require.True(t, ok)
	assert.Equal(t, Control, control)

	platform, ok := CategoryForOperation("StartRecording")
	require.True(t, ok)
	assert.Equal(t, Platform, platform)

	_, ok = CategoryForOperation("NotAnOperation")
	assert.False(t, ok)
}

// Every operation must land in one of the two api_call categories: any other
// category would produce an event type nothing publishes.
func TestOperationCategoriesAreAPICallCategories(t *testing.T) {
	for operationID, cat := range categoryByOperationID {
		assert.Contains(t, []oapi.TelemetryEventCategory{Control, Platform}, cat, "operation %s", operationID)
	}
}
