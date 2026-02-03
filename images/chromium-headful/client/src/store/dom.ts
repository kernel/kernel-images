import { getterTree, mutationTree, actionTree } from 'typed-vuex'
import { DomElement, DomViewport, DomWindowBounds, DomSyncPayload } from '~/neko/dom-types'

export const namespaced = true

const defaultWindowBounds: DomWindowBounds = {
  x: 0,
  y: 0,
  width: 1920,
  height: 1080,
  chromeTop: 0,
  chromeLeft: 0,
  fullscreen: false,
}

export const state = () => ({
  enabled: false,
  connected: false,
  elements: [] as DomElement[],
  viewport: { w: 1280, h: 720, sx: 0, sy: 0 } as DomViewport,
  windowBounds: { ...defaultWindowBounds } as DomWindowBounds,
  url: '',
  seq: 0,
})

export const getters = getterTree(state, {
  hasElements: (state) => state.elements.length > 0,
  elementCount: (state) => state.elements.length,
})

export const mutations = mutationTree(state, {
  setEnabled(state, enabled: boolean) {
    state.enabled = enabled
  },

  setConnected(state, connected: boolean) {
    state.connected = connected
  },

  applySync(state, payload: DomSyncPayload) {
    // Only apply if sequence is newer (to handle out-of-order messages)
    if (payload.seq > state.seq) {
      state.elements = payload.elements
      state.viewport = payload.viewport
      state.windowBounds = payload.windowBounds
      state.url = payload.url
      state.seq = payload.seq
    }
  },

  reset(state) {
    state.elements = []
    state.viewport = { w: 1280, h: 720, sx: 0, sy: 0 }
    state.windowBounds = { ...defaultWindowBounds }
    state.url = ''
    state.seq = 0
    state.connected = false
  },
})

export const actions = actionTree(
  { state, getters, mutations },
  {
    // Actions can be added later if needed for WebSocket management
  },
)
