// 供应商管理：列表 / 新建（预设模板预填）/ 编辑（api_key 留空=保持不变，协议不可改）/
// 删除（409=生效中）/ 每 App 备选序列拖拽排序。

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { DndContext, closestCenter, type DragEndEvent } from '@dnd-kit/core'
import {
  SortableContext,
  arrayMove,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { GripVertical, Pencil, Plus, Trash2 } from 'lucide-react'
import { ApiError, apiDelete, apiGet, apiPost, apiPut } from '@/api/client'
import {
  APPS,
  APP_LABEL,
  APP_PROTOCOL,
  type App,
  type FallbackResp,
  type Preset,
  type Protocol,
  type Provider,
  type StateResp,
} from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

// ---- 表单 ----

interface FormState {
  name: string
  protocol: Protocol
  base_url: string
  api_key: string
  cost_coefficient: string
  note: string
  redirects: Array<{ from: string; to: string }>
}

const emptyForm: FormState = {
  name: '',
  protocol: 'anthropic',
  base_url: '',
  api_key: '',
  cost_coefficient: '1',
  note: '',
  redirects: [],
}

function formFromProvider(p: Provider): FormState {
  return {
    name: p.name,
    protocol: p.protocol,
    base_url: p.base_url,
    api_key: '',
    cost_coefficient: String(p.cost_coefficient),
    note: p.note,
    redirects: Object.entries(p.model_redirects ?? {}).map(([from, to]) => ({ from, to })),
  }
}

function ProviderDialog({
  open,
  onClose,
  editing,
  presets,
}: {
  open: boolean
  onClose: () => void
  editing: Provider | null
  presets: Preset[]
}) {
  const qc = useQueryClient()
  const [form, setForm] = useState<FormState>(emptyForm)
  useEffect(() => {
    if (open) setForm(editing ? formFromProvider(editing) : emptyForm)
  }, [open, editing])

  const set = (patch: Partial<FormState>) => setForm((f) => ({ ...f, ...patch }))

  const applyPreset = (id: string) => {
    const p = presets.find((x) => x.id === id)
    if (!p) return
    set({ name: p.name, protocol: p.protocol, base_url: p.base_url_hint, cost_coefficient: String(p.cost_coefficient) })
  }

  const save = useMutation({
    mutationFn: () => {
      const redirects: Record<string, string> = {}
      for (const r of form.redirects) {
        if (r.from && r.to) redirects[r.from] = r.to
      }
      const body: Record<string, unknown> = {
        name: form.name,
        base_url: form.base_url,
        cost_coefficient: Number(form.cost_coefficient) || 1,
        note: form.note,
        model_redirects: redirects,
      }
      if (form.api_key) body.api_key = form.api_key
      if (editing) return apiPut(`/api/v1/providers/${editing.id}`, body)
      body.protocol = form.protocol
      return apiPost('/api/v1/providers', body)
    },
    onSuccess: () => {
      toast.success(editing ? '供应商已更新' : '供应商已创建')
      qc.invalidateQueries({ queryKey: ['providers'] })
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  const valid = form.name && form.base_url && (editing || form.api_key)

  return (
    <Dialog open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? '编辑供应商' : '新建供应商'}</DialogTitle>
          {!editing && <DialogDescription>可从预设模板开始，再改成实际站点信息。</DialogDescription>}
        </DialogHeader>
        <div className="space-y-4">
          {!editing && (
            <div className="space-y-2">
              <Label>预设模板</Label>
              <Select onValueChange={(v) => { if (typeof v === 'string') applyPreset(v) }}>
                <SelectTrigger className="w-full">
                  <SelectValue placeholder="选择预设（可选）" />
                </SelectTrigger>
                <SelectContent>
                  {presets.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          <div className="space-y-2">
            <Label htmlFor="p-name">名称</Label>
            <Input id="p-name" value={form.name} onChange={(e) => set({ name: e.target.value })} />
          </div>
          <div className="space-y-2">
            <Label>协议{editing && '（不可修改）'}</Label>
            <Select
              items={[{ value: 'anthropic', label: 'anthropic（Claude Code）' }, { value: 'openai', label: 'openai（Codex）' }]}
              value={form.protocol}
              onValueChange={(v) => v && set({ protocol: v as Protocol })}
              disabled={!!editing}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="anthropic">anthropic（Claude Code）</SelectItem>
                <SelectItem value="openai">openai（Codex）</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="p-url">Base URL</Label>
            <Input
              id="p-url"
              value={form.base_url}
              onChange={(e) => set({ base_url: e.target.value })}
              placeholder={form.protocol === 'anthropic' ? 'https://relay.example（不含 /v1）' : 'https://relay.example/v1（含 /v1）'}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="p-key">API Key</Label>
            <Input
              id="p-key"
              type="password"
              value={form.api_key}
              onChange={(e) => set({ api_key: e.target.value })}
              placeholder={editing ? `留空保持不变（当前尾号 ${editing.key_last4 || '????'}）` : 'sk-…'}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="p-coeff">折扣系数</Label>
            <Input
              id="p-coeff"
              type="number"
              step="0.01"
              min="0"
              value={form.cost_coefficient}
              onChange={(e) => set({ cost_coefficient: e.target.value })}
            />
            <p className="text-xs text-muted-foreground">费用 = LiteLLM 基准价 × 系数（0.1 表示一折站点）</p>
          </div>
          <div className="space-y-2">
            <Label>模型重定向</Label>
            {form.redirects.map((r, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  value={r.from}
                  placeholder="请求模型名"
                  onChange={(e) =>
                    set({ redirects: form.redirects.map((x, j) => (j === i ? { ...x, from: e.target.value } : x)) })
                  }
                />
                <span className="text-muted-foreground">→</span>
                <Input
                  value={r.to}
                  placeholder="实际执行模型名"
                  onChange={(e) =>
                    set({ redirects: form.redirects.map((x, j) => (j === i ? { ...x, to: e.target.value } : x)) })
                  }
                />
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="删除重定向"
                  onClick={() => set({ redirects: form.redirects.filter((_, j) => j !== i) })}
                >
                  <Trash2 className="size-4" />
                </Button>
              </div>
            ))}
            <Button
              variant="outline"
              size="sm"
              onClick={() => set({ redirects: [...form.redirects, { from: '', to: '' }] })}
            >
              <Plus className="size-4" /> 添加重定向
            </Button>
          </div>
          <div className="space-y-2">
            <Label htmlFor="p-note">备注</Label>
            <Input id="p-note" value={form.note} onChange={(e) => set({ note: e.target.value })} />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            取消
          </Button>
          <Button onClick={() => save.mutate()} disabled={!valid || save.isPending}>
            {save.isPending ? '保存中…' : '保存'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---- 备选序列 ----

function SortableRow({ id, name }: { id: string; name: string }) {
  const { attributes, listeners, setNodeRef, transform, transition } = useSortable({ id })
  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className="flex items-center gap-2 rounded-md border bg-background px-3 py-2"
    >
      <button className="cursor-grab touch-none text-muted-foreground" {...attributes} {...listeners}>
        <GripVertical className="size-4" />
      </button>
      <span className="text-sm">{name}</span>
    </div>
  )
}

function FallbackEditor({ app, providers }: { app: App; providers: Provider[] }) {
  const qc = useQueryClient()
  const candidates = providers.filter((p) => p.protocol === APP_PROTOCOL[app])
  const { data } = useQuery({
    queryKey: ['fallback', app],
    queryFn: () => apiGet<FallbackResp>(`/api/v1/fallback-order/${app}`),
  })
  const [order, setOrder] = useState<string[] | null>(null)
  const ids = order ?? data?.provider_ids ?? []
  // 尚未进序列的供应商可一键追加。
  const missing = candidates.filter((p) => !ids.includes(p.id))

  const saveMut = useMutation({
    mutationFn: (provider_ids: string[]) =>
      apiPut(`/api/v1/fallback-order/${app}`, { provider_ids }),
    onSuccess: () => {
      toast.success(`${APP_LABEL[app]} 备选序列已保存`)
      qc.invalidateQueries({ queryKey: ['fallback', app] })
      setOrder(null)
    },
    onError: (err) => toast.error(err.message),
  })

  const onDragEnd = (e: DragEndEvent) => {
    const { active, over } = e
    if (!over || active.id === over.id) return
    const next = arrayMove(ids, ids.indexOf(String(active.id)), ids.indexOf(String(over.id)))
    setOrder(next)
    saveMut.mutate(next)
  }

  const nameOf = (id: string) => candidates.find((p) => p.id === id)?.name ?? id

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-base">{APP_LABEL[app]} 备选序列</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {ids.length === 0 && <p className="text-sm text-muted-foreground">未配置。故障切换（M4）按此顺序自动降级。</p>}
        <DndContext collisionDetection={closestCenter} onDragEnd={onDragEnd}>
          <SortableContext items={ids} strategy={verticalListSortingStrategy}>
            <div className="space-y-2">
              {ids.map((id) => (
                <SortableRow key={id} id={id} name={nameOf(id)} />
              ))}
            </div>
          </SortableContext>
        </DndContext>
        {missing.length > 0 && (
          <div className="flex flex-wrap gap-2 pt-1">
            {missing.map((p) => (
              <Button
                key={p.id}
                variant="outline"
                size="sm"
                onClick={() => saveMut.mutate([...ids, p.id])}
              >
                <Plus className="size-4" /> {p.name}
              </Button>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

// ---- 页面 ----

export default function ProvidersPage() {
  const qc = useQueryClient()
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Provider | null>(null)

  const providers = useQuery({
    queryKey: ['providers'],
    queryFn: () => apiGet<Provider[]>('/api/v1/providers'),
  })
  const presets = useQuery({ queryKey: ['presets'], queryFn: () => apiGet<Preset[]>('/api/v1/presets') })
  const state = useQuery({ queryKey: ['state'], queryFn: () => apiGet<StateResp>('/api/v1/state') })

  const activeIDs = new Set(
    APPS.map((a) => state.data?.[a]?.active_provider_id).filter(Boolean) as string[],
  )

  const del = useMutation({
    mutationFn: (id: string) => apiDelete(`/api/v1/providers/${id}`),
    onSuccess: () => {
      toast.success('供应商已删除')
      qc.invalidateQueries({ queryKey: ['providers'] })
    },
    onError: (err) =>
      toast.error(err instanceof ApiError && err.status === 409 ? '该供应商正在生效中，请先切换到其他供应商' : err.message),
  })

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">供应商</h2>
        <Button
          onClick={() => {
            setEditing(null)
            setDialogOpen(true)
          }}
        >
          <Plus className="size-4" /> 新建
        </Button>
      </div>

      <Card>
        <CardContent className="overflow-x-auto pt-6">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>协议</TableHead>
                <TableHead>Base URL</TableHead>
                <TableHead>Key</TableHead>
                <TableHead className="text-right">折扣系数</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(providers.data ?? []).map((p) => (
                <TableRow key={p.id}>
                  <TableCell className="font-medium">
                    {p.name}
                    {activeIDs.has(p.id) && (
                      <Badge className="ml-2" variant="default">
                        生效中
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline">{p.protocol}</Badge>
                  </TableCell>
                  <TableCell className="max-w-64 truncate text-muted-foreground">{p.base_url}</TableCell>
                  <TableCell className="text-muted-foreground">····{p.key_last4}</TableCell>
                  <TableCell className="text-right">{p.cost_coefficient}</TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="编辑"
                      onClick={() => {
                        setEditing(p)
                        setDialogOpen(true)
                      }}
                    >
                      <Pencil className="size-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label="删除"
                      onClick={() => {
                        if (window.confirm(`确认删除供应商「${p.name}」？`)) del.mutate(p.id)
                      }}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
              {providers.data?.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="text-center text-muted-foreground">
                    还没有供应商，点右上角「新建」开始
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="grid gap-4 lg:grid-cols-2">
        {APPS.map((app) => (
          <FallbackEditor key={app} app={app} providers={providers.data ?? []} />
        ))}
      </div>

      <ProviderDialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        editing={editing}
        presets={presets.data ?? []}
      />
    </div>
  )
}
