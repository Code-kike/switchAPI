// 仪表盘：per-App 当前供应商 + 一键切换、汇总卡片、趋势图、维度拆解。
// 数据经 ws/ui 失效通知自动刷新（switch → state_changed、新用量 → usage_tick）。

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Line,
  ResponsiveContainer,
  Tooltip as ChartTooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { apiGet, apiPost, qs } from '@/api/client'
import {
  APPS,
  APP_LABEL,
  APP_PROTOCOL,
  type App,
  type BreakdownEntry,
  type Provider,
  type StateResp,
  type SummaryResp,
  type Totals,
  type TrendEntry,
} from '@/api/types'
import { fmtCost, fmtTokens } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'

// ---- 时间范围 ----

const RANGES = [
  { key: 'today', label: '今天', days: 1 },
  { key: '7d', label: '7 天', days: 7 },
  { key: '30d', label: '30 天', days: 30 },
] as const
type RangeKey = (typeof RANGES)[number]['key']

function rangeParams(key: RangeKey): { from: number; to: number } {
  const now = Math.floor(Date.now() / 1000)
  if (key === 'today') {
    const d = new Date()
    d.setHours(0, 0, 0, 0)
    return { from: Math.floor(d.getTime() / 1000), to: now }
  }
  const days = RANGES.find((r) => r.key === key)?.days ?? 7
  return { from: now - days * 86400, to: now }
}

// ---- 切换卡片 ----

function SwitchCard({ app, state, providers }: { app: App; state?: StateResp; providers?: Provider[] }) {
  const qc = useQueryClient()
  const candidates = (providers ?? []).filter((p) => p.protocol === APP_PROTOCOL[app])
  const activeID = state?.[app]?.active_provider_id ?? ''
  const active = candidates.find((p) => p.id === activeID)

  const switchMut = useMutation({
    mutationFn: (providerID: string) =>
      apiPost('/api/v1/switch', { app, provider_id: providerID }),
    onSuccess: () => {
      toast.success(`${APP_LABEL[app]} 已切换`)
      qc.invalidateQueries({ queryKey: ['state'] })
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center justify-between text-base">
          {APP_LABEL[app]}
          <Badge variant="outline">{APP_PROTOCOL[app]}</Badge>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="text-2xl font-semibold">{active ? active.name : '未设置'}</div>
        <Select
          items={candidates.map((p) => ({ value: p.id, label: p.name }))}
          value={activeID || undefined}
          onValueChange={(v) => v && switchMut.mutate(v)}
          disabled={switchMut.isPending || candidates.length === 0}
        >
          <SelectTrigger className="w-full">
            <SelectValue placeholder={candidates.length ? '选择供应商切换' : '暂无可用供应商'} />
          </SelectTrigger>
          <SelectContent>
            {candidates.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </CardContent>
    </Card>
  )
}

// ---- 汇总卡片 ----

function StatCard({ title, value, hint }: { title: string; value: string; hint?: string }) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-sm font-normal text-muted-foreground">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold">{value}</div>
        {hint && <div className="mt-1 text-xs text-muted-foreground">{hint}</div>}
      </CardContent>
    </Card>
  )
}

function totalTokens(t: Totals): number {
  return t.input_tokens + t.output_tokens + t.cache_write_tokens + t.cache_read_tokens
}

// ---- 页面 ----

export default function DashboardPage() {
  const [range, setRange] = useState<RangeKey>('7d')
  const [bucket, setBucket] = useState<'hour' | 'day'>('hour')
  const [dim, setDim] = useState<'provider' | 'model' | 'app' | 'device'>('provider')
  const { from, to } = rangeParams(range)

  const state = useQuery({ queryKey: ['state'], queryFn: () => apiGet<StateResp>('/api/v1/state') })
  const providers = useQuery({
    queryKey: ['providers'],
    queryFn: () => apiGet<Provider[]>('/api/v1/providers'),
  })
  const summary = useQuery({
    queryKey: ['stats', 'summary', range],
    queryFn: () => apiGet<SummaryResp>(`/api/v1/stats/summary${qs({ from, to })}`),
  })
  const trend = useQuery({
    queryKey: ['stats', 'trend', range, bucket],
    queryFn: () => apiGet<TrendEntry[]>(`/api/v1/stats/trend${qs({ from, to, bucket })}`),
  })
  const breakdown = useQuery({
    queryKey: ['stats', 'breakdown', range, dim],
    queryFn: () => apiGet<BreakdownEntry[]>(`/api/v1/stats/breakdown${qs({ from, to, by: dim })}`),
  })

  const s = summary.data
  const chart = (trend.data ?? []).map((e) => ({
    time: new Date(e.bucket_ts * 1000).toLocaleString('zh-CN', {
      month: bucket === 'day' ? 'numeric' : undefined,
      day: 'numeric',
      hour: bucket === 'hour' ? 'numeric' : undefined,
      hour12: false,
    }),
    requests: e.requests,
    cost: e.cost ?? 0,
  }))

  return (
    <div className="space-y-6">
      {/* 一键切换 */}
      <div className="grid gap-4 sm:grid-cols-2">
        {APPS.map((app) => (
          <SwitchCard key={app} app={app} state={state.data} providers={providers.data} />
        ))}
      </div>

      {/* 时间范围 */}
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">用量概览</h2>
        <Tabs value={range} onValueChange={(v) => setRange(v as RangeKey)}>
          <TabsList>
            {RANGES.map((r) => (
              <TabsTrigger key={r.key} value={r.key}>
                {r.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      </div>

      {/* 汇总卡片 */}
      {summary.isLoading ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <StatCard title="请求数" value={String(s?.requests ?? 0)} />
          <StatCard
            title="Token 总量"
            value={s ? fmtTokens(totalTokens(s)) : '0'}
            hint={
              s
                ? `输入 ${fmtTokens(s.input_tokens)} · 输出 ${fmtTokens(s.output_tokens)} · 缓存写 ${fmtTokens(s.cache_write_tokens)} · 缓存读 ${fmtTokens(s.cache_read_tokens)}`
                : undefined
            }
          />
          <StatCard
            title="费用"
            value={s && s.requests === 0 ? '—' : fmtCost(s?.cost ?? null)}
            hint={
              s && s.cost_unknown_requests > 0
                ? `另有 ${s.cost_unknown_requests} 个请求因未知模型无法计价`
                : undefined
            }
          />
          <StatCard
            title="按 App"
            value={
              s
                ? APPS.map((a) => `${APP_LABEL[a]} ${s.by_app[a]?.requests ?? 0}`).join(' / ')
                : '—'
            }
          />
        </div>
      )}

      {/* 趋势图 */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between pb-2">
          <CardTitle className="text-base">趋势</CardTitle>
          <Tabs value={bucket} onValueChange={(v) => setBucket(v as 'hour' | 'day')}>
            <TabsList>
              <TabsTrigger value="hour">按小时</TabsTrigger>
              <TabsTrigger value="day">按天</TabsTrigger>
            </TabsList>
          </Tabs>
        </CardHeader>
        <CardContent>
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={chart}>
                <CartesianGrid strokeDasharray="3 3" className="stroke-muted" />
                <XAxis dataKey="time" fontSize={12} />
                <YAxis yAxisId="req" fontSize={12} allowDecimals={false} />
                <YAxis yAxisId="cost" orientation="right" fontSize={12} />
                <ChartTooltip
                  formatter={(value, name) =>
                    name === '费用' ? [fmtCost(Number(value)), name] : [value, name]
                  }
                />
                <Bar yAxisId="req" dataKey="requests" name="请求数" fill="var(--color-primary)" radius={[3, 3, 0, 0]} />
                <Line yAxisId="cost" dataKey="cost" name="费用" stroke="var(--color-chart-2, #10b981)" dot={false} />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
        </CardContent>
      </Card>

      {/* 维度拆解 */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between pb-2">
          <CardTitle className="text-base">拆解</CardTitle>
          <Tabs value={dim} onValueChange={(v) => setDim(v as typeof dim)}>
            <TabsList>
              <TabsTrigger value="provider">供应商</TabsTrigger>
              <TabsTrigger value="model">模型</TabsTrigger>
              <TabsTrigger value="app">App</TabsTrigger>
              <TabsTrigger value="device">设备</TabsTrigger>
            </TabsList>
          </Tabs>
        </CardHeader>
        <CardContent className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead className="text-right">请求数</TableHead>
                <TableHead className="text-right">输入</TableHead>
                <TableHead className="text-right">输出</TableHead>
                <TableHead className="text-right">缓存写</TableHead>
                <TableHead className="text-right">缓存读</TableHead>
                <TableHead className="text-right">费用</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(breakdown.data ?? []).map((e) => (
                <TableRow key={e.key}>
                  <TableCell className="max-w-56 truncate font-medium">{e.name || e.key || '—'}</TableCell>
                  <TableCell className="text-right">{e.requests}</TableCell>
                  <TableCell className="text-right">{fmtTokens(e.input_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(e.output_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(e.cache_write_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtTokens(e.cache_read_tokens)}</TableCell>
                  <TableCell className="text-right">{fmtCost(e.cost)}</TableCell>
                </TableRow>
              ))}
              {breakdown.data?.length === 0 && (
                <TableRow>
                  <TableCell colSpan={7} className="text-center text-muted-foreground">
                    所选时间范围内没有用量
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}
