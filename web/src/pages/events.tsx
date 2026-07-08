// 事件时间线：switch/failover/pairing/provider/auth… 全局操作审计流。

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { apiGet, qs } from '@/api/client'
import type { EventRow } from '@/api/types'
import { fmtTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

const KIND_LABEL: Record<string, { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }> = {
  switch: { label: '切换', variant: 'default' },
  failover: { label: '故障切换', variant: 'destructive' },
  pairing: { label: '配对', variant: 'secondary' },
  provider: { label: '供应商', variant: 'outline' },
  auth: { label: '认证', variant: 'outline' },
  backup: { label: '备份', variant: 'outline' },
  speedtest: { label: '测速', variant: 'outline' },
  probe: { label: '探测', variant: 'outline' },
}

// 把 payload 摘要成一行中文描述；未知结构回退到紧凑 JSON。
function summarize(e: EventRow): string {
  const p = e.payload as Record<string, string>
  switch (e.kind) {
    case 'switch':
      return `${p.app}：${p.from ? `${p.from.slice(0, 8)}… → ` : ''}${p.to_name ?? p.to}`
    case 'pairing':
      return p.action === 'paired' ? `设备「${p.name}」完成配对` : p.action === 'revoked' ? `设备 ${String(p.device_id).slice(0, 8)}… 被吊销` : JSON.stringify(p)
    case 'provider':
      return p.action === 'created' ? `新建供应商「${p.name}」` : p.action === 'deleted' ? `删除供应商 ${String(p.id).slice(0, 8)}…` : JSON.stringify(p)
    case 'auth':
      return p.action === 'bootstrap' ? '首次登录，管理员密码已设定' : JSON.stringify(p)
    default:
      return JSON.stringify(p)
  }
}

export default function EventsPage() {
  const [limit, setLimit] = useState(50)
  const events = useQuery({
    queryKey: ['events', limit],
    queryFn: () => apiGet<EventRow[]>(`/api/v1/events${qs({ limit })}`),
  })

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold">事件时间线</h2>
      <Card>
        <CardContent className="pt-6">
          <ol className="space-y-3">
            {(events.data ?? []).map((e) => {
              const kind = KIND_LABEL[e.kind] ?? { label: e.kind, variant: 'outline' as const }
              return (
                <li key={e.id} className="flex items-start gap-3 border-b pb-3 last:border-b-0 last:pb-0">
                  <Badge variant={kind.variant} className="mt-0.5 shrink-0">
                    {kind.label}
                  </Badge>
                  <div className="min-w-0 flex-1">
                    <p className="break-all text-sm">{summarize(e)}</p>
                    <p className="mt-0.5 text-xs text-muted-foreground">{fmtTime(e.ts)}</p>
                  </div>
                </li>
              )
            })}
            {events.data?.length === 0 && (
              <li className="text-center text-sm text-muted-foreground">暂无事件</li>
            )}
          </ol>
          {(events.data?.length ?? 0) >= limit && (
            <div className="mt-4 text-center">
              <Button variant="outline" size="sm" onClick={() => setLimit(limit + 100)}>
                加载更多
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
