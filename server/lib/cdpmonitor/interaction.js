(function() {
  if (window.__kernelEventInjected) return;
  var send = window.__kernelEvent;
  if (!send) return;
  window.__kernelEventInjected = true;

  function sel(el) {
    return el.id ? '#' + el.id : (el.className ? '.' + String(el.className).split(' ')[0] : '');
  }

  document.addEventListener('click', function(e) {
    var t = e.target || {};
    send(JSON.stringify({
      type: 'interaction_click',
      x: e.clientX, y: e.clientY,
      selector: sel(t), tag: t.tagName || '',
      text: (t.textContent || '').slice(0, 100)
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

  function scrollPos(target) {
    if (target === document || target === document.documentElement) {
      return {x: window.scrollX, y: window.scrollY};
    }
    return {x: target.scrollLeft || 0, y: target.scrollTop || 0};
  }

  var scrollTimer = null;
  var scrollStart = null;
  var scrollTarget = null;
  document.addEventListener('scroll', function(e) {
    var target = e.target;
    // If target changed mid-scroll, reset tracking for the new target.
    if (scrollTarget !== target) {
      scrollStart = scrollPos(target);
      scrollTarget = target;
    }
    var fromX = scrollStart.x, fromY = scrollStart.y;
    var s = target === document ? 'document' : sel(target);
    if (scrollTimer) clearTimeout(scrollTimer);
    scrollTimer = setTimeout(function() {
      var pos = scrollPos(target);
      if (Math.abs(pos.x - fromX) > 5 || Math.abs(pos.y - fromY) > 5) {
        send(JSON.stringify({
          type: 'scroll_settled',
          from_x: fromX, from_y: fromY,
          to_x: pos.x, to_y: pos.y,
          target_selector: s
        }));
      }
      scrollStart = {x: pos.x, y: pos.y};
      scrollTarget = null;
    }, 300);
  }, true);
})();