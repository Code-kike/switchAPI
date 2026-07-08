// 设备管理：列表（平台/最近在线/状态）、生成配对码（TTL 倒计时 + 接入指引）、吊销。

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Gauge, Plus, Trash2 } from 'lucide-react'
import { apiDelete, apiGet, apiPost } from '@/api/client'
import type { Device, PairingCode, Provider, SpeedtestRun } from '@/api/types'
import { fmtTime, relTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

function SpeedtestPanel({ devices }: { devices: Device[] }) {
  const qc = useQueryClient()
  const providers = useQuery({
    queryKey: ['providers'],
    queryFn: () => apiGet<Provider[]>('/api/v1/providers'),
  })
  const latest = useQuery({
    queryKey: ['speedtest'],
    queryFn: () => apiGet<SpeedtestRun | null>('/api/v1/speedtest/latest'),
    refetchInterval: (q) => {
      const run = q.state.data
      // 进行中（结果数 < 期望设备数）时轮询。
      if (run && Object.keys(run.results).length < run.expected_devices.length) return 2000
      return false
    },
  })
  const start = useMutation({
    mutationFn: () => apiPost<{ test_id: string }>('/api/v1/speedtest'),
    onSuccess: () => {
      toast.success('测速指令已下发到所有在线设备')
      qc.invalidateQueries({ queryKey: ['speedtest'] })
    },
    onError: (err) => toast.error(err.message),
  })

  const run = latest.data
  const deviceName = (id: string) => devices.find((d) => d.id === id)?.name ?? id.slice(0, 8)
  const providerName = (id: string) => providers.data?.find((p) => p.id === id)?.name ?? id.slice(0, 8)
  const running = run && Object.keys(run.results).length < run.expected_devices.length

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between text-base">
          端点测速
          <Button size="sm" onClick={() => start.mutate()} disabled={start.isPending || !!running}>
            <Gauge className="size-4" /> {running ? '测速中…' : '开始测速'}
          </Button>
        </CardTitle>
        <CardDescription>各在线设备直连每个供应商发起最小补全请求（各自网络位置的真实延迟）。</CardDescription>
      </CardHeader>
      <CardContent className="overflow-x-auto">
        {!run ? (
          <p className="text-sm text-muted-foreground">还没有测速结果</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>设备</TableHead>
                <TableHead>供应商</TableHead>
                <TableHead className="text-right">延迟</TableHead>
                <TableHead>状态</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {Object.entries(run.results).flatMap(([dev, results]) =>
                results.map((r, i) => (
                  <TableRow key={dev + i}>
                    <TableCell>{i === 0 ? deviceName(dev) : ''}</TableCell>
                    <TableCell>{providerName(r.provider_id)}</TableCell>
                    <TableCell className="text-right">{r.latency_ms} ms</TableCell>
                    <TableCell>
                      {r.ok ? (
                        <Badge variant="secondary">可用</Badge>
                      ) : (
                        <Badge variant="destructive" title={r.error}>失败</Badge>
                      )}
                    </TableCell>
                  </TableRow>
                )),
              )}
              {running && (
                <TableRow>
                  <TableCell colSpan={4} className="text-center text-muted-foreground">
                    等待 {run.expected_devices.length - Object.keys(run.results).length} 台设备回报…
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  )
}

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

      <SpeedtestPanel devices={devices.data ?? []} />\n\n      <PairingDialog code={pairing} onClose={() => setPairing(null)} />
    </div>
  )
}
