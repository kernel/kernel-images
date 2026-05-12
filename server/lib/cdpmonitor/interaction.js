(function() {
  if (window.__kernelEventInjected) return;
  var send = window.__kernelEvent;
  if (!send) return;
  window.__kernelEventInjected = true;

  function sel(el) {
    return el.id ? '#' + el.id : (el.className ? '.' + String(el.className).split(' ')[0] : '');
  }

  // Sensitive autocomplete values whose fields must never emit key content.
  // Covers passwords, payment card data, and government identifiers.
  var SENSITIVE_AUTOCOMPLETE = {
    'current-password': true, 'new-password': true, 'one-time-code': true,
    'cc-number': true, 'cc-csc': true, 'cc-exp': true, 'cc-exp-month': true,
    'cc-exp-year': true, 'cc-name': true, 'cc-type': true,
    'transaction-amount': true,
    'ssn': true, 'passport': true, 'drivers-license': true
  };

  // Sensitive name/id substrings, defence-in-depth for fields without autocomplete.
  var SENSITIVE_NAME_RE = /passw|passwd|secret|cvv|cvc|ssn|card.?num|account.?num|pin\b|tax.?id|natl.?id/i;

  // Returns true if the element is one where keystroke content must be suppressed.
  function isSensitiveInput(el) {
    if (!el || !el.tagName) return false;
    var tag = el.tagName.toUpperCase();
    var isEditable = tag === 'INPUT' || tag === 'TEXTAREA'
      || el.isContentEditable
      || (el.getAttribute && el.getAttribute('role') === 'textbox');
    if (!isEditable) return false;
    if (el.type === 'password') return true;
    // autocomplete attribute check.
    var ac = (el.getAttribute && el.getAttribute('autocomplete')) || '';
    var acTokens = ac.toLowerCase().trim().split(/\s+/);
    for (var i = 0; i < acTokens.length; i++) {
      if (SENSITIVE_AUTOCOMPLETE[acTokens[i]]) return true;
    }
    // name/id/aria-label heuristic as fallback, covers custom controls that use ARIA.
    var name = (el.name || el.id || (el.getAttribute && el.getAttribute('aria-label')) || '');
    return SENSITIVE_NAME_RE.test(name);
  }

  // Returns true if the clicked element's textContent should be suppressed.
  // Widens isSensitiveInput to also cover display-only elements (span, div, td, etc.)
  // that may render sensitive values, those elements return false from isSensitiveInput
  // because they are not editable, but their textContent is still sensitive.
  function shouldSuppressClickText(el) {
    if (isSensitiveInput(el)) return true;
    var id = el.id || '';
    var aria = (el.getAttribute && el.getAttribute('aria-label')) || '';
    return SENSITIVE_NAME_RE.test(id) || SENSITIVE_NAME_RE.test(aria);
  }

  // Only reads direct child text nodes and prevents container elements from leaking
  // sensitive values rendered in child elements that pass shouldSuppressClickText.
  function directText(el) {
    var text = '';
    var nodes = el.childNodes;
    for (var i = 0; i < nodes.length; i++) {
      if (nodes[i].nodeType === 3) {
        text += nodes[i].textContent;
      }
    }
    return text.trim();
  }

  document.addEventListener('click', function(e) {
    var t = e.target || {};
    // Suppress text capture for sensitive inputs/display elements; record click position/selector only.
    // Use directText (not textContent) to avoid leaking sensitive values from child elements.
    var text = shouldSuppressClickText(t) ? '' : directText(t).slice(0, 100);
    send(JSON.stringify({
      type: 'interaction_click',
      x: e.clientX, y: e.clientY,
      selector: sel(t), tag: t.tagName || '',
      text: text
    }));
  }, true);

  document.addEventListener('keydown', function(e) {
    var t = e.target || {};
    // Never record which key was pressed inside a sensitive field.
    if (isSensitiveInput(t)) return;
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
          type: 'interaction_scroll_settled',
          from_x: fromX, from_y: fromY,
          to_x: pos.x, to_y: pos.y,
          target_selector: s
        }));
      }
      scrollTarget = null;
    }, 300);
  }, true);
})();