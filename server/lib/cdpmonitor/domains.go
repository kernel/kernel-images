package cdpmonitor

import _ "embed"

// bindingName is the JS function exposed via Runtime.addBinding.
// Page JS calls this to fire Runtime.bindingCalled CDP events.
const bindingName = "__kernelEvent"

// isPageLikeTarget reports whether the target type supports page-level CDP
// domains (Page.*, PerformanceTimeline.*, Page.addScriptToEvaluateOnNewDocument).
// Workers and service workers only support Runtime.* and Network.*.
func isPageLikeTarget(targetType string) bool {
	return targetType == "page" || targetType == "iframe"
}

// injectedJS tracks clicks, keys, and scrolls via the __kernelEvent binding.
// Layout shifts are handled natively by PerformanceTimeline.enable.
//
//go:embed interaction.js
var injectedJS string
