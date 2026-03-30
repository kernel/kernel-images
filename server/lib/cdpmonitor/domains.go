package cdpmonitor

import "context"

// enableDomains sends the standard enable commands for all CDP domains the
// monitor listens to. sessionID is empty for browser-level commands, or a
// flat-mode session ID for per-target commands. Failures are non-fatal;
// the monitor continues enabling remaining domains.
func (m *Monitor) enableDomains(ctx context.Context, sessionID string) {
	for _, method := range []string{
		"Runtime.enable",
		"Network.enable",
		"Page.enable",
		"DOM.enable",
	} {
		_, _ = m.send(ctx, method, nil, sessionID)
	}
}

// injectedJS is the combined interaction tracking + PerformanceObserver script
// injected into every new document via Page.addScriptToEvaluateOnNewDocument.
const injectedJS = `(function() {
  var prefix = '[KERNEL_EVENT] ';

  // --- Click tracking ---
  document.addEventListener('click', function(e) {
    var t = e.target || {};
    console.log(prefix + JSON.stringify({
      type: 'interaction_click',
      x: e.clientX, y: e.clientY,
      selector: t.id ? '#' + t.id : (t.className ? '.' + String(t.className).split(' ')[0] : ''),
      tag: t.tagName || '',
      text: (t.innerText || '').slice(0, 100)
    }));
  }, true);

  // --- Keydown tracking ---
  document.addEventListener('keydown', function(e) {
    var t = e.target || {};
    console.log(prefix + JSON.stringify({
      type: 'interaction_key',
      key: e.key,
      selector: t.id ? '#' + t.id : (t.className ? '.' + String(t.className).split(' ')[0] : ''),
      tag: t.tagName || ''
    }));
  }, true);

  // --- Scroll tracking with 300ms debounce ---
  var scrollTimer = null;
  var scrollStart = {x: window.scrollX, y: window.scrollY};
  document.addEventListener('scroll', function(e) {
    var fromX = scrollStart.x, fromY = scrollStart.y;
    var target = e.target;
    var sel = target === document ? 'document' : (target.id ? '#' + target.id : (target.className ? '.' + String(target.className).split(' ')[0] : ''));
    if (scrollTimer) clearTimeout(scrollTimer);
    scrollTimer = setTimeout(function() {
      var toX = window.scrollX, toY = window.scrollY;
      if (Math.abs(toX - fromX) > 5 || Math.abs(toY - fromY) > 5) {
        console.log(prefix + JSON.stringify({
          type: 'scroll_settled',
          from_x: fromX, from_y: fromY,
          to_x: toX, to_y: toY,
          target_selector: sel
        }));
      }
      scrollStart = {x: toX, y: toY};
    }, 300);
  }, true);

  // --- Layout shift via PerformanceObserver ---
  if (typeof PerformanceObserver !== 'undefined') {
    try {
      new PerformanceObserver(function(list) {
        list.getEntries().forEach(function(entry) {
          if (entry.hadRecentInput) return;
          var sources = (entry.sources || []).map(function(s) {
            return {
              element: s.node ? (s.node.id ? '#' + s.node.id : s.node.tagName) : '',
              previous_rect: s.previousRect ? [s.previousRect.x, s.previousRect.y, s.previousRect.width, s.previousRect.height] : null,
              current_rect: s.currentRect ? [s.currentRect.x, s.currentRect.y, s.currentRect.width, s.currentRect.height] : null
            };
          });
          console.log(prefix + JSON.stringify({
            type: 'layout_shift',
            score: entry.value,
            sources: sources
          }));
        });
      }).observe({type: 'layout-shift', buffered: true});
    } catch(e) {}
  }
})();`

// injectScript sends Page.addScriptToEvaluateOnNewDocument with the combined
// interaction tracking + PerformanceObserver JS for the given target session.
func (m *Monitor) injectScript(ctx context.Context, sessionID string) error {
	_, err := m.send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": injectedJS,
	}, sessionID)
	return err
}
