package cdpmonitor

import "context"

// bindingName is the JS function exposed via Runtime.addBinding.
// Page JS calls this to fire Runtime.bindingCalled CDP events.
const bindingName = "__kernelEvent"

// enableDomains enables CDP domains, registers the event binding, and starts
// layout-shift observation. Failures are non-fatal.
func (m *Monitor) enableDomains(ctx context.Context, sessionID string) {
	for _, method := range []string{
		"Runtime.enable",
		"Network.enable",
		"Page.enable",
		"DOM.enable",
	} {
		_, _ = m.send(ctx, method, nil, sessionID)
	}

	_, _ = m.send(ctx, "Runtime.addBinding", map[string]any{
		"name": bindingName,
	}, sessionID)

	_, _ = m.send(ctx, "PerformanceTimeline.enable", map[string]any{
		"eventTypes": []string{"layout-shift"},
	}, sessionID)
}

// injectedJS tracks clicks, keys, and scrolls via the __kernelEvent binding.
// Layout shifts are handled natively by PerformanceTimeline.enable.
const injectedJS = `(function() {
  var send = window.__kernelEvent;
  if (!send) return;

  function sel(el) {
    return el.id ? '#' + el.id : (el.className ? '.' + String(el.className).split(' ')[0] : '');
  }

  document.addEventListener('click', function(e) {
    var t = e.target || {};
    send(JSON.stringify({
      type: 'interaction_click',
      x: e.clientX, y: e.clientY,
      selector: sel(t), tag: t.tagName || '',
      text: (t.innerText || '').slice(0, 100)
    }));
  }, true);

  document.addEventListener('keydown', function(e) {
    var t = e.target || {};
    send(JSON.stringify({
      type: 'interaction_key',
      key: e.key,
      selector: sel(t), tag: t.tagName || ''
    }));
  }, true);

  var scrollTimer = null;
  var scrollStart = {x: window.scrollX, y: window.scrollY};
  document.addEventListener('scroll', function(e) {
    var fromX = scrollStart.x, fromY = scrollStart.y;
    var target = e.target;
    var s = target === document ? 'document' : sel(target);
    if (scrollTimer) clearTimeout(scrollTimer);
    scrollTimer = setTimeout(function() {
      var toX = window.scrollX, toY = window.scrollY;
      if (Math.abs(toX - fromX) > 5 || Math.abs(toY - fromY) > 5) {
        send(JSON.stringify({
          type: 'scroll_settled',
          from_x: fromX, from_y: fromY,
          to_x: toX, to_y: toY,
          target_selector: s
        }));
      }
      scrollStart = {x: toX, y: toY};
    }, 300);
  }, true);
})();`

// injectScript registers the interaction tracking JS for the given session.
func (m *Monitor) injectScript(ctx context.Context, sessionID string) error {
	_, err := m.send(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": injectedJS,
	}, sessionID)
	return err
}
