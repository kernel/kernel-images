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
