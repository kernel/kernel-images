package cdpmonitor

import (
	"strings"

	"github.com/onkernel/kernel-images/server/lib/events"
)

// categoryFor derives an EventCategory from a CDP event type string.
// It splits on the first dot and maps the prefix to a category.
func categoryFor(eventType string) events.EventCategory {
	prefix, _, _ := strings.Cut(eventType, ".")
	switch prefix {
	case "console":
		return events.CategoryConsole
	case "network":
		return events.CategoryNetwork
	case "page", "navigation", "dom", "target":
		return events.CategoryPage
	case "interaction", "layout", "scroll":
		return events.CategoryInteraction
	case "liveview":
		return events.CategoryLiveview
	case "captcha":
		return events.CategoryCaptcha
	case "screenshot", "monitor":
		return events.CategorySystem
	default:
		return events.CategorySystem
	}
}
