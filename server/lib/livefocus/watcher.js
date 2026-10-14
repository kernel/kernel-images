// Reports the focused editable element of one frame, and for a top-level frame
// the editable fields on screen. It runs in an isolated world: the page cannot
// see its globals or listeners, and page changes to prototypes or globals do
// not reach it. Nothing here logs or throws to the page. The server polls
// next(); a watcher that is not polled for IDLE_MS removes itself, so a
// dropped connection leaves nothing running in the page.
(() => {
  const IDLE_MS = 30000
  const HEARTBEAT_MS = 10000
  const TICK_MS = 1000
  const MAX_FIELDS = 64
  const MAX_FRAME_DEPTH = 3
  const TEXT_TYPES = new Set(['text', 'search', 'email', 'url', 'tel', 'password', 'number'])
  const CODE_HINT = /one.?time|otp|2fa|mfa|totp|verif|passcode|security.?code|auth\w*.?code|sms.?code|\bcode\b|\bpin\b/i
  const NOT_CODE_HINT = /zip|postal|post.?code|promo|coupon|gift|discount|voucher|country|area|referral/i

  const top = window.top === window
  let version = 0
  let report = null
  let serialized = ''
  let waiter = null
  let waitingSince = 0
  let polledAt = Date.now()
  let timer = 0
  let timerAt = 0
  let stopped = false

  const isInput = (el) => el.localName === 'input'

  // the element keyboard input goes to in this document, through open shadow roots
  function deepActive(doc) {
    let el = doc.activeElement
    while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement
    return el
  }

  function editable(el) {
    if (!el || el.nodeType !== 1) return false
    if (isInput(el)) return TEXT_TYPES.has(el.type) && !el.disabled && !el.readOnly
    if (el.localName === 'textarea') return !el.disabled && !el.readOnly
    return el.isContentEditable
  }

  function isCode(el, type, autocomplete) {
    if (autocomplete.split(/\s+/).includes('one-time-code')) return true
    if (!isInput(el) || !['text', 'number', 'tel'].includes(type)) return false
    const labels = el.labels && el.labels.length ? el.labels[0].textContent.slice(0, 100) : ''
    const hint = [el.name, el.id, el.getAttribute('placeholder'), el.getAttribute('aria-label'), labels].join(' ')
    if (NOT_CODE_HINT.test(hint)) return false
    // one box per digit
    if (el.maxLength === 1) return true
    if (!CODE_HINT.test(hint)) return false
    const numeric = type !== 'text' || /numeric|decimal/.test(el.inputMode) || /\\d|\[0-9\]/.test(el.pattern)
    return numeric || (el.maxLength > 0 && el.maxLength <= 10)
  }

  function describe(el, offset) {
    const attr = (name) => el.getAttribute(name)
    const type = isInput(el) ? el.type : el.localName === 'textarea' ? 'textarea' : 'contenteditable'
    const autocomplete = (attr('autocomplete') || '').trim().toLowerCase()
    const field = {
      type,
      inputMode: (attr('inputmode') || '').toLowerCase(),
      autocomplete,
      enterKeyHint: (attr('enterkeyhint') || '').toLowerCase(),
      autocapitalize: attr('autocapitalize'),
      autocorrect: attr('autocorrect'),
      spellcheck: attr('spellcheck'),
      code: isCode(el, type, autocomplete),
      rect: null,
    }
    if (offset) {
      const r = el.getBoundingClientRect()
      field.rect = {
        x: Math.round(r.left + offset.x),
        y: Math.round(r.top + offset.y),
        w: Math.round(r.width),
        h: Math.round(r.height),
      }
    }
    return field
  }

  // where this frame's viewport sits in the top frame's, while every frame up
  // the chain is same-origin; null once a cross-origin frame is in the way
  function frameOffset() {
    let x = 0
    let y = 0
    let win = window
    try {
      while (win !== win.top) {
        const owner = win.frameElement
        if (!owner) return null
        const content = contentBox(owner)
        x += content.x
        y += content.y
        win = win.parent
      }
    } catch (e) {
      return null
    }
    return { x, y }
  }

  function contentBox(frame) {
    const r = frame.getBoundingClientRect()
    const style = frame.ownerDocument.defaultView.getComputedStyle(frame)
    return {
      x: r.left + frame.clientLeft + parseFloat(style.paddingLeft),
      y: r.top + frame.clientTop + parseFloat(style.paddingTop),
    }
  }

  // Editable fields that a tap at their center would reach, in this document
  // and same-origin child documents, in top-frame coordinates.
  function scan(doc, offset, clip, depth, fields) {
    const win = doc.defaultView
    if (!win) return
    const width = win.innerWidth
    const height = win.innerHeight
    for (const el of doc.querySelectorAll('input, textarea, [contenteditable]')) {
      if (fields.length >= MAX_FIELDS) return
      if (!editable(el)) continue
      // only the root of a contenteditable region
      if (!isInput(el) && el.localName !== 'textarea' && el.parentElement && el.parentElement.isContentEditable) continue
      const r = el.getBoundingClientRect()
      const left = Math.max(r.left, 0, clip.x - offset.x)
      const right = Math.min(r.right, width, clip.x + clip.w - offset.x)
      const topEdge = Math.max(r.top, 0, clip.y - offset.y)
      const bottom = Math.min(r.bottom, height, clip.y + clip.h - offset.y)
      if (right - left < 2 || bottom - topEdge < 2) continue
      const hit = doc.elementFromPoint((left + right) / 2, (topEdge + bottom) / 2)
      if (!hit || !(hit === el || el.contains(hit) || hit.control === el)) continue
      fields.push(describe(el, offset))
    }
    if (depth >= MAX_FRAME_DEPTH) return
    for (const frame of doc.querySelectorAll('iframe, frame')) {
      let child = null
      try {
        child = frame.contentDocument
      } catch (e) {}
      if (!child) continue
      const content = contentBox(frame)
      const childOffset = { x: offset.x + content.x, y: offset.y + content.y }
      const r = frame.getBoundingClientRect()
      const childClip = intersect(clip, { x: offset.x + r.left, y: offset.y + r.top, w: r.width, h: r.height })
      if (childClip) scan(child, childOffset, childClip, depth + 1, fields)
    }
  }

  function intersect(a, b) {
    const x = Math.max(a.x, b.x)
    const y = Math.max(a.y, b.y)
    const w = Math.min(a.x + a.w, b.x + b.w) - x
    const h = Math.min(a.y + a.h, b.y + b.h) - y
    return w > 0 && h > 0 ? { x, y, w, h } : null
  }

  function collect() {
    const focused = document.hasFocus()
    const active = deepActive(document)
    const state = {
      top,
      visible: document.visibilityState === 'visible',
      focused,
      frames: window.length,
      focus: focused && editable(active) ? describe(active, frameOffset()) : null,
    }
    if (top) {
      state.viewport = {
        w: window.innerWidth,
        h: window.innerHeight,
        dpr: window.devicePixelRatio,
        screenW: screen.width,
        screenH: screen.height,
        screenX: window.screenX,
        screenY: window.screenY,
      }
      state.fields = []
      if (state.visible) scan(document, { x: 0, y: 0 }, { x: 0, y: 0, w: window.innerWidth, h: window.innerHeight }, 0, state.fields)
    }
    return state
  }

  function update() {
    timer = 0
    if (stopped) return
    let state
    try {
      state = collect()
    } catch (e) {
      return
    }
    const next = JSON.stringify(state)
    if (next === serialized) return
    serialized = next
    report = state
    version++
    wake()
  }

  // runs update within ms, sooner if an earlier one is already scheduled
  function schedule(ms) {
    const at = Date.now() + ms
    if (timer && timerAt <= at) return
    if (timer) clearTimeout(timer)
    timerAt = at
    timer = setTimeout(update, ms)
  }

  function wake() {
    if (!waiter) return
    const resolve = waiter
    waiter = null
    resolve(Object.assign({ v: version }, report))
  }

  // A blur reports a little later than a focus, so a field that is replaced
  // or refocused right away does not report a blur in between. Scrolling
  // reports fields sooner than other page changes, which can be constant.
  const onFocusIn = () => schedule(0)
  const onFocusOut = () => schedule(100)
  const onScroll = () => schedule(100)
  const onChange = () => schedule(200)
  const listeners = [
    [document, 'focusin', onFocusIn],
    [document, 'focusout', onFocusOut],
    [window, 'focus', onFocusIn],
    [window, 'blur', onFocusOut],
    [document, 'visibilitychange', onFocusIn],
    [document, 'scroll', onScroll],
    [window, 'resize', onChange],
  ]
  for (const [target, type, fn] of listeners) target.addEventListener(type, fn, { capture: true, passive: true })
  const observer = top ? new MutationObserver(onChange) : null
  if (observer) {
    observer.observe(document, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['class', 'style', 'hidden', 'disabled', 'readonly', 'type', 'contenteditable'],
    })
  }

  const tick = setInterval(() => {
    if (waiter && Date.now() - waitingSince >= HEARTBEAT_MS) {
      wake()
    } else if (!waiter && Date.now() - polledAt > IDLE_MS) {
      stop()
      return
    }
    schedule(0)
  }, TICK_MS)

  function stop() {
    if (stopped) return
    stopped = true
    clearInterval(tick)
    if (timer) clearTimeout(timer)
    for (const [target, type, fn] of listeners) target.removeEventListener(type, fn, { capture: true })
    if (observer) observer.disconnect()
    if (waiter) {
      const resolve = waiter
      waiter = null
      resolve({ stopped: true })
    }
  }

  update()

  return {
    // the report after version seen, or the current one at the heartbeat
    next(seen) {
      polledAt = Date.now()
      if (stopped) return Promise.resolve({ stopped: true })
      if (seen !== version) return Promise.resolve(Object.assign({ v: version }, report))
      if (waiter) wake()
      waitingSince = Date.now()
      return new Promise((resolve) => {
        waiter = resolve
      })
    },
    stop,
  }
})()
