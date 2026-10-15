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

// AcceptedLocation is the location of the persisted accepted bundle.
type AcceptedLocation struct {
	TimeZone  string   `json:"timezone"`
	Languages []string `json:"languages"`
}

// ReadAccepted returns the location of the persisted accepted bundle, or nil
// when none has been accepted.
func ReadAccepted(data []byte) *AcceptedLocation {
	var state struct {
		Accepted *AcceptedLocation `json:"accepted"`
	}
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return state.Accepted
}
