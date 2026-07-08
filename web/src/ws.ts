// ws/ui 客户端：登录后建立，断线指数退避重连（1s→30s）。
// 三类下行都是"失效通知"——只做 react-query invalidate，让对应查询 refetch。

import type { QueryClient } from '@tanstack/react-query'
import type { WsEnvelope } from '@/api/types'

let socket: WebSocket | null = null
let attempt = 0
let stopped = false

export function startWs(qc: QueryClient) {
  stopped = false
  if (socket) return
  connect(qc)
}

export function stopWs() {
  stopped = true
  socket?.close()
  socket = null
}

function connect(qc: QueryClient) {
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const ws = new WebSocket(`${proto}://${window.location.host}/api/v1/ws/ui`)
  socket = ws

  ws.onopen = () => {
    attempt = 0
    // 重连后本地缓存可能已落后，全部刷新一遍。
    qc.invalidateQueries()
  }

  ws.onmessage = (msg) => {
    let env: WsEnvelope
    try {
      env = JSON.parse(msg.data as string) as WsEnvelope
    } catch {
      return
    }
    switch (env.type) {
      case 'state_changed':
        qc.invalidateQueries({ queryKey: ['state'] })
        qc.invalidateQueries({ queryKey: ['providers'] })
        break
      case 'usage_tick':
        qc.invalidateQueries({ queryKey: ['stats'] })
        qc.invalidateQueries({ queryKey: ['usage'] })
        break
      case 'event':
        qc.invalidateQueries({ queryKey: ['events'] })
        break
    }
  }

  ws.onclose = () => {
    socket = null
    if (stopped) return
    const delay = Math.min(1000 * 2 ** attempt, 30_000)
    attempt++
    setTimeout(() => {
      if (!stopped && !socket) connect(qc)
    }, delay)
  }
}
