/**
 * DOM Sync Types
 *
 * TypeScript interfaces for the DOM overlay system that mirrors
 * interactive elements from the remote browser.
 */

/** Element type categories for DOM sync */
export type DomElementType = 'inputs' | 'buttons' | 'links' | 'images' | 'media'

/** All available DOM element types */
export const DOM_ELEMENT_TYPES: DomElementType[] = ['inputs', 'buttons', 'links', 'images', 'media']

/** Color scheme for each element type (rgba format) */
export const DOM_TYPE_COLORS: Record<DomElementType, { bg: string; border: string }> = {
  inputs: { bg: 'rgba(139, 92, 246, 0.1)', border: 'rgba(139, 92, 246, 0.3)' },   // Purple
  buttons: { bg: 'rgba(59, 130, 246, 0.1)', border: 'rgba(59, 130, 246, 0.3)' },  // Blue
  links: { bg: 'rgba(34, 197, 94, 0.1)', border: 'rgba(34, 197, 94, 0.3)' },      // Green
  images: { bg: 'rgba(249, 115, 22, 0.1)', border: 'rgba(249, 115, 22, 0.3)' },   // Orange
  media: { bg: 'rgba(6, 182, 212, 0.1)', border: 'rgba(6, 182, 212, 0.3)' },      // Cyan
}

export interface DomElement {
  /** Unique identifier for the element (e.g., "d0", "d1") */
  id: string
  /** HTML tag name (input, button, a, select, textarea, etc.) */
  tag: string
  /** Element type category */
  type: DomElementType
  /** Bounding rectangle in viewport coordinates */
  rect: {
    x: number
    y: number
    w: number
    h: number
  }
  /** CSS z-index value */
  z: number
}

export interface DomViewport {
  /** Viewport width in pixels */
  w: number
  /** Viewport height in pixels */
  h: number
  /** Horizontal scroll position (scrollX) */
  sx: number
  /** Vertical scroll position (scrollY) */
  sy: number
}

export interface DomWindowBounds {
  /** Window X position on screen */
  x: number
  /** Window Y position on screen */
  y: number
  /** Window width (outer) */
  width: number
  /** Window height (outer) */
  height: number
  /** Chrome offset from window top to viewport top (tabs + address bar + bookmarks) */
  chromeTop: number
  /** Chrome offset from window left to viewport left (usually minimal) */
  chromeLeft: number
  /** Whether browser is in fullscreen mode */
  fullscreen: boolean
}

export interface DomSyncPayload {
  /** Monotonically increasing sequence number */
  seq: number
  /** Timestamp when the sync was generated (ms since epoch) */
  ts: number
  /** Array of interactive elements with their bounding boxes */
  elements: DomElement[]
  /** Current viewport dimensions and scroll position */
  viewport: DomViewport
  /** Browser window bounds and chrome offsets for coordinate mapping */
  windowBounds: DomWindowBounds
  /** Current page URL */
  url: string
}

export interface DomWebSocketMessage {
  event: 'dom/sync' | 'dom/update' | 'dom/start' | 'dom/stop'
  data?: DomSyncPayload
}
