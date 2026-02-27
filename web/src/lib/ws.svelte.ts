import { applyEvent, setConnected } from './state.svelte'
import type { WebSocketEvent } from './types'

let socket: WebSocket | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let shouldReconnect = true

function wsURL(): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/ws`
}

function scheduleReconnect(): void {
  if (!shouldReconnect || reconnectTimer) {
    return
  }

  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    connect()
  }, 1500)
}

export function connect(): void {
  shouldReconnect = true

  if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) {
    return
  }

  socket = new WebSocket(wsURL())

  socket.addEventListener('open', () => {
    setConnected(true)
  })

  socket.addEventListener('message', (event) => {
    try {
      const payload = JSON.parse(event.data) as WebSocketEvent
      applyEvent(payload)
    } catch (error) {
      void error
    }
  })

  socket.addEventListener('close', () => {
    setConnected(false)
    scheduleReconnect()
  })

  socket.addEventListener('error', () => {
    setConnected(false)
  })
}

export function disconnect(): void {
  shouldReconnect = false
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }

  if (socket) {
    socket.close()
    socket = null
  }
}
