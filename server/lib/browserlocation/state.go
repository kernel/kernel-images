package browserlocation

import (
	"encoding/json"
	"os"
	"strings"
)

const defaultStatePath = "/run/kernel/browser-location.json"

// StatePath is where the image API persists the accepted location bundle.
func StatePath() string {
	if value := strings.TrimSpace(os.Getenv("KERNEL_BROWSER_LOCATION_STATE_PATH")); value != "" {
		return value
	}
	return defaultStatePath
}

// AcceptedTimeZone returns the timezone of the persisted accepted bundle, or
// "" when none has been accepted.
func AcceptedTimeZone(data []byte) string {
	var state struct {
		Accepted *struct {
			TimeZone string `json:"timezone"`
		} `json:"accepted"`
	}
	if json.Unmarshal(data, &state) != nil || state.Accepted == nil {
		return ""
	}
	return state.Accepted.TimeZone
}
