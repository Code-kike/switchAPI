// 用量明细：筛选（时间/App/供应商/设备/模型）+ 分页表格。cost=null 显示"未知"。

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { apiGet, qs } from '@/api/client'
import { APP_LABEL, type Device, type Provider, type UsageResp } from '@/api/types'
import { fmtCost, fmtDuration, fmtTime, fmtTokens } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const PAGE_SIZE = 50
const ALL = '__all__'

const DAY_OPTIONS = [
  { key: '1', label: '今天' },
  { key: '7', label: '近 7 天' },
  { key: '30', label: '近 30 天' },
  { key: '90', label: '近 90 天' },
]

export default function UsagePage() {
  const [days, setDays] = useState('7')
  const [app, setApp] = useState(ALL)
  const [provider, setProvider] = useState(ALL)
  const [device, setDevice] = useState(ALL)
  const [model, setModel] = useState('')
  const [page, setPage] = useState(0)

  const now = Math.floor(Date.now() / 1000)
  const params = {
    from: now - Number(days) * 86400,
    to: now,
    app: app === ALL ? '' : app,
    provider_id: provider === ALL ? '' : provider,
    device_id: device === ALL ? '' : device,
    model: model.trim(),
    limit: PAGE_SIZE,
    offset: page * PAGE_SIZE,
  }

  const providers = useQuery({
    queryKey: ['providers'],
    queryFn: () => apiGet<Provider[]>('/api/v1/providers'),
  })
  const devices = useQuery({ queryKey: ['devices'], queryFn: () => apiGet<Device[]>('/api/v1/devices') })
  const usage = useQuery({
    queryKey: ['usage', params],
    queryFn: () => apiGet<UsageResp>(`/api/v1/usage${qs(params)}`),
  })

  const providerName = (id: string) => providers.data?.find((p) => p.id === id)?.name ?? id.slice(0, 8)
  const deviceName = (id: string) => devices.data?.find((d) => d.id === id)?.name ?? id.slice(0, 8)

  const total = usage.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const resetPage = () => setPage(0)

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold">用量明细</h2>

      {/* 筛选 */}
      <div className="flex flex-wrap items-center gap-2">
        <Select items={DAY_OPTIONS.map((o) => ({ value: o.key, label: o.label }))} value={days} onValueChange={(v) => { if (v) { setDays(v); resetPage() } }}>
          <SelectTrigger className="w-28"><SelectValue /></SelectTrigger>
          <SelectContent>
            {DAY_OPTIONS.map((o) => (
              <SelectItem key={o.key} value={o.key}>{o.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select items={[{ value: ALL, label: '全部 App' }, { value: 'claude-code', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }]} value={app} onValueChange={(v) => { if (v) { setApp(v); resetPage() } }}>
          <SelectTrigger className="w-36"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>全部 App</SelectItem>
            <SelectItem value="claude-code">Claude Code</SelectItem>
            <SelectItem value="codex">Codex</SelectItem>
          </SelectContent>
        </Select>
        <Select items={[{ value: ALL, label: '全部供应商' }, ...(providers.data ?? []).map((p) => ({ value: p.id, label: p.name }))]} value={provider} onValueChange={(v) => { if (v) { setProvider(v); resetPage() } }}>
          <SelectTrigger className="w-40"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>全部供应商</SelectItem>
            {(providers.data ?? []).map((p) => (
              <SelectItem key={p.id} value={p.id}>{p.name}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select items={[{ value: ALL, label: '全部设备' }, ...(devices.data ?? []).map((d) => ({ value: d.id, label: d.name }))]} value={device} onValueChange={(v) => { if (v) { setDevice(v); resetPage() } }}>
          <SelectTrigger className="w-36"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>全部设备</SelectItem>
            {(devices.data ?? []).map((d) => (
              <SelectItem key={d.id} value={d.id}>{d.name}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="w-52"
          placeholder="按模型名筛选（实际执行名）"
          value={model}
          onChange={(e) => { setModel(e.target.value); resetPage() }}
        />
      </div>

      <Card>
        <CardContent className="overflow-x-auto pt-6">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>App</TableHead>
                <TableHead>供应商</TableHead>
                <TableHead>模型</TableHead>
                <TableHead className="text-right">输入</TableHead>
                <TableHead className="text-right">输出</TableHead>
                <TableHead className="text-right">缓存写</TableHead>
                <TableHead className="text-right">缓存读</TableHead>
                <TableHead className="text-right">耗时</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>设备</TableHead>
                <TableHead className="text-right">费用</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(usage.data?.rows ?? []).map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="whitespace-nowrap text-muted-foreground">{fmtTime(r.ts)}</TableCell>
                  <TableCell>{APP_LABEL[r.app as keyof typeof APP_LABEL] ?? r.app}</TableCell>
                  <TableCell className="max-w-32 truncate">{providerName(r.provider_id)}</TableCell>
                  <TableCell className="max-w-48 truncate">
                    {r.model_redirected ? (
                      <span title={`请求名 ${r.model} → 实际 ${r.model_redirected}`}>
                        {r.model_redirected} <Badge variant="outline">重定向</Badge>
                      </span>
                    ) : (
                      r.model
                    )}
                  </TableCell>
                  <TableCell className="text-right">{fmtTokens(r.input_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(r.output_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(r.cache_write_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(r.cache_read_tokens)}</TableCell>
                  <TableCell className="whitespace-nowrap text-right">{fmtDuration(r.duration_ms)}</TableCell>
                  <TableCell>
                    <Badge variant={r.status >= 200 && r.status < 300 ? 'secondary' : 'destructive'}>
                      {r.status}
                    </Badge>
                    {r.usage_source !== 'wire' && (
                      <Badge className="ml-1" variant="outline">{r.usage_source === 'estimated' ? '估算' : '缺失'}</Badge>
                    )}
                  </TableCell>
                  <TableCell className="max-w-28 truncate text-muted-foreground">{deviceName(r.device_id)}</TableCell>
                  <TableCell className="text-right">{fmtCost(r.cost)}</TableCell>
                </TableRow>
              ))}
              {usage.data?.rows?.length === 0 && (
                <TableRow>
                  <TableCell colSpan={12} className="text-center text-muted-foreground">
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* 分页 */}
      <div className="flex items-center justify-between text-sm text-muted-foreground">
        <span>共 {total} 条</span>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" disabled={page === 0} onClick={() => setPage(page - 1)}>
            上一页
          </Button>
          <span>
            {page + 1} / {pages}
          </span>
          <Button variant="outline" size="sm" disabled={page + 1 >= pages} onClick={() => setPage(page + 1)}>
            下一页
          </Button>
        </div>
      </div>
    </div>
  )
}
