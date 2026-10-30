// Package runasroot defines the privilege policy for customer-facing services.
package runasroot

import (
	"os"
	"strings"
)

// Enabled reports whether RUN_AS_ROOT explicitly enables root mode.
func Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_AS_ROOT")), "true")
}
