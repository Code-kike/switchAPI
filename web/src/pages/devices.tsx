// 设备管理：列表（平台/最近在线/状态）、生成配对码（TTL 倒计时 + 接入指引）、吊销。

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Plus, Trash2 } from 'lucide-react'
import { apiDelete, apiGet, apiPost } from '@/api/client'
import type { Device, PairingCode } from '@/api/types'
import { fmtTime, relTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

function PairingDialog({ code, onClose }: { code: PairingCode | null; onClose: () => void }) {
  const [left, setLeft] = useState(0)
  useEffect(() => {
    if (!code) return
    const tick = () => setLeft(Math.max(0, code.expires_at - Math.floor(Date.now() / 1000)))
    tick()
    const timer = setInterval(tick, 1000)
    return () => clearInterval(timer)
  }, [code])

  const hubURL = window.location.origin

  return (
    <Dialog open={!!code} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>设备配对码</DialogTitle>
          <DialogDescription>一次性使用，{left > 0 ? `${Math.floor(left / 60)} 分 ${left % 60} 秒后过期` : '已过期'}。</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="text-center font-mono text-4xl font-bold tracking-[0.3em]">
            {code?.code}
          </div>
          <div className="rounded-md bg-muted p-3 text-sm">
            <p className="mb-1 text-muted-foreground">在目标设备上执行：</p>
            <code className="break-all font-mono text-xs">
              switchapi-agent pair --hub {hubURL} --code {code?.code} --name 设备名
            </code>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

export default function DevicesPage() {
  const qc = useQueryClient()
  const [pairing, setPairing] = useState<PairingCode | null>(null)

  const devices = useQuery({ queryKey: ['devices'], queryFn: () => apiGet<Device[]>('/api/v1/devices') })

  const genCode = useMutation({
    mutationFn: () => apiPost<PairingCode>('/api/v1/devices/pairing-code'),
    onSuccess: (pc) => setPairing(pc),
    onError: (err) => toast.error(err.message),
  })

  const revoke = useMutation({
    mutationFn: (id: string) => apiDelete(`/api/v1/devices/${id}`),
    onSuccess: () => {
      toast.success('设备已吊销')
      qc.invalidateQueries({ queryKey: ['devices'] })
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">设备</h2>
        <Button onClick={() => genCode.mutate()} disabled={genCode.isPending}>
          <Plus className="size-4" /> 生成配对码
        </Button>
      </div>

      <Card>
        <CardContent className="overflow-x-auto pt-6">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>平台</TableHead>
                <TableHead>配对时间</TableHead>
                <TableHead>最近在线</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(devices.data ?? []).map((d) => (
                <TableRow key={d.id}>
                  <TableCell className="font-medium">{d.name}</TableCell>
                  <TableCell className="text-muted-foreground">{d.platform || '—'}</TableCell>
                  <TableCell className="whitespace-nowrap text-muted-foreground">{fmtTime(d.paired_at)}</TableCell>
                  <TableCell className="whitespace-nowrap">{relTime(d.last_seen)}</TableCell>
                  <TableCell>
                    {d.revoked ? <Badge variant="destructive">已吊销</Badge> : <Badge variant="secondary">正常</Badge>}
                  </TableCell>
                  <TableCell className="text-right">
                    {!d.revoked && (
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label="吊销"
                        onClick={() => {
                          if (window.confirm(`确认吊销设备「${d.name}」？其 Agent 将立即断开且无法重连。`))
                            revoke.mutate(d.id)
                        }}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
              {devices.data?.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    还没有设备，点「生成配对码」把第一台开发机接进来
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <PairingDialog code={pairing} onClose={() => setPairing(null)} />
    </div>
  )
}
