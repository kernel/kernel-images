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
})();