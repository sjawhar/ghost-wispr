import { afterEach, describe, expect, it, vi } from 'vitest'
import { appState, resetState } from '../state.svelte'
import { connect, disconnect } from '../ws.svelte'

class MockSocket {
  static instances: MockSocket[] = []

  url: string
  readyState = 1
  listeners = new Map<string, Array<(event?: any) => void>>()

  constructor(url: string) {
    this.url = url
    MockSocket.instances.push(this)
  }

  addEventListener(type: string, listener: (event?: any) => void) {
    const list = this.listeners.get(type) ?? []
    list.push(listener)
    this.listeners.set(type, list)
  }

  close() {
    this.emit('close')
  }

  emit(type: string, event?: any) {
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event)
    }
  }
}

describe('ws manager', () => {
  afterEach(() => {
    disconnect()
    resetState()
    MockSocket.instances = []
    vi.restoreAllMocks()
  })

  it('connects and updates connected state', () => {
    vi.stubGlobal('WebSocket', MockSocket as any)

    connect()
    expect(MockSocket.instances.length).toBe(1)

    MockSocket.instances[0].emit('open')
    expect(appState.connected).toBe(true)
  })

  it('applies incoming websocket events', () => {
    vi.stubGlobal('WebSocket', MockSocket as any)

    connect()
    MockSocket.instances[0].emit('message', {
      data: JSON.stringify({
        type: 'status_changed',
        version: 1,
        timestamp: new Date().toISOString(),
        paused: true,
      }),
    })

    expect(appState.paused).toBe(true)
  })
})
