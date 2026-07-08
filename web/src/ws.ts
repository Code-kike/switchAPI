// ws/ui 客户端：登录后建立，断线指数退避重连（1s→30s）。
// 三类下行都是"失效通知"——只做 react-query invalidate，让对应查询 refetch。

import type { QueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
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
      case 'event': {
        qc.invalidateQueries({ queryKey: ['events'] })
        notifyReliabilityEvent(qc, env.payload)
        break
      }
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

// failover/probe 事件 → 即时 toast（双端可见通知：桌面壳装载同一 SPA）。
function notifyReliabilityEvent(qc: QueryClient, payload: unknown) {
  const ev = payload as { kind?: string; payload?: Record<string, string> }
  const p = ev?.payload ?? {}
  if (ev?.kind === 'failover') {
    qc.invalidateQueries({ queryKey: ['health'] })
    if (p.action === 'switched') {
      toast.warning(`故障切换：${p.app} 已从「${p.from_name}」切到「${p.to_name}」`, { duration: 10000 })
    } else if (p.action === 'no_candidate') {
      toast.error(`「${p.provider}」故障，且备选序列没有健康候选（保持当前供应商）`, { duration: 10000 })
    } else if (p.action === 'vetoed') {
      toast.info(`「${p.provider}」的故障上报被否决：${p.reason}`, { duration: 8000 })
    }
  } else if (ev?.kind === 'probe') {
    qc.invalidateQueries({ queryKey: ['health'] })
    if (p.action === 'recovered') {
      toast.success(`「${p.provider}」已恢复，可一键切回`, { duration: 10000 })
    }
  } else if (ev?.kind === 'speedtest' && p.action === 'completed') {
    qc.invalidateQueries({ queryKey: ['speedtest'] })
  }
}
