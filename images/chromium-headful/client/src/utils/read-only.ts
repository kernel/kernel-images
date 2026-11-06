declare global {
  interface Window {
    // Injected by the live-view proxy for read-only links.
    KERNEL_LIVE_VIEW_READ_ONLY?: unknown
  }
}

export function isReadOnlyMessage(event: MessageEvent, parent: Window, parentOrigin: string): boolean {
  return (
    event.source === parent &&
    parentOrigin !== '*' &&
    parentOrigin !== 'null' &&
    event.origin === parentOrigin &&
    event.data !== null &&
    typeof event.data === 'object' &&
    event.data.type === 'KERNEL_SET_READ_ONLY' &&
    typeof event.data.readOnly === 'boolean' &&
    (event.data.requestId === undefined || typeof event.data.requestId === 'string')
  )
}

// A read-only link stays read-only: the server refuses its input either way, so
// the page neither unlocks nor offers controls it cannot use.
export function resolveReadOnly(requested: boolean, forced: unknown): boolean {
  return forced === true || requested
}
