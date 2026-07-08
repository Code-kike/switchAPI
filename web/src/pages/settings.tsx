// 设置：备份快照、口令加密导出/导入、CSV、cc-switch 一键导入（M4）。

import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Download, HardDriveDownload, Upload } from 'lucide-react'
import { apiGet, apiPost } from '@/api/client'
import type { BackupInfo, CCSwitchImportResp } from '@/api/types'
import { fmtTime } from '@/lib/format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

function downloadJSON(name: string, data: unknown) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  URL.revokeObjectURL(url)
}

function BackupCard() {
  const qc = useQueryClient()
  const backups = useQuery({ queryKey: ['backups'], queryFn: () => apiGet<BackupInfo[]>('/api/v1/backups') })
  const run = useMutation({
    mutationFn: () => apiPost<BackupInfo>('/api/v1/backup/run'),
    onSuccess: (info) => {
      toast.success(`快照完成：${info.name}`)
      qc.invalidateQueries({ queryKey: ['backups'] })
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between text-base">
          备份快照
          <Button size="sm" onClick={() => run.mutate()} disabled={run.isPending}>
            <HardDriveDownload className="size-4" /> 立即备份
          </Button>
        </CardTitle>
        <CardDescription>每日自动 + 配置变更后 5 分钟内自动快照；保留最近 10 份（存于 Hub 数据目录 backups/）。</CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>文件</TableHead>
              <TableHead className="text-right">大小</TableHead>
              <TableHead className="text-right">时间</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(backups.data ?? []).map((b) => (
              <TableRow key={b.name}>
                <TableCell className="font-mono text-xs">{b.name}</TableCell>
                <TableCell className="text-right">{(b.size_bytes / 1024).toFixed(0)} KB</TableCell>
                <TableCell className="whitespace-nowrap text-right text-muted-foreground">{fmtTime(b.created_at)}</TableCell>
              </TableRow>
            ))}
            {backups.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={3} className="text-center text-muted-foreground">还没有快照</TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}

function ExportCard() {
  const [pass, setPass] = useState('')
  const exp = useMutation({
    mutationFn: (body: Record<string, unknown>) => apiPost<Record<string, unknown>>('/api/v1/export', body),
    onSuccess: (data) => {
      downloadJSON(`switchapi-export-${new Date().toISOString().slice(0, 10)}.json`, data)
      toast.success('导出文件已下载')
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">导出配置</CardTitle>
        <CardDescription>供应商（含 API key）、备选序列、切换状态与覆盖价。含密钥，强烈建议口令加密。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-2">
          <Label htmlFor="exp-pass">加密口令</Label>
          <Input id="exp-pass" type="password" value={pass} onChange={(e) => setPass(e.target.value)}
            placeholder="留空则需确认明文导出" />
        </div>
        <div className="flex gap-2">
          <Button onClick={() => exp.mutate({ passphrase: pass })} disabled={!pass || exp.isPending}>
            <Download className="size-4" /> 加密导出
          </Button>
          <Button
            variant="outline"
            disabled={exp.isPending}
            onClick={() => {
              if (window.confirm('明文导出的文件包含全部供应商 API key 的明文，任何拿到该文件的人都能直接使用你的额度。确定继续？'))
                exp.mutate({ plaintext_confirmed: true })
            }}
          >
            明文导出
          </Button>
          <Button variant="outline" onClick={() => window.open('/api/v1/usage/export.csv', '_blank')}>
            用量 CSV
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function ImportCard() {
  const qc = useQueryClient()
  const [pass, setPass] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)
  const imp = useMutation({
    mutationFn: async () => {
      const f = fileRef.current?.files?.[0]
      if (!f) throw new Error('请选择导出文件')
      const body = JSON.parse(await f.text()) as Record<string, unknown>
      if (pass) body.passphrase = pass
      return apiPost<{ providers: number }>('/api/v1/import', body)
    },
    onSuccess: (r) => {
      toast.success(`导入完成：${r.providers} 个供应商`)
      qc.invalidateQueries()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">导入配置</CardTitle>
        <CardDescription>选择本产品的导出文件；加密文件需输入原口令。同 ID 供应商将被覆盖。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <Input ref={fileRef} type="file" accept="application/json,.json" />
        <div className="space-y-2">
          <Label htmlFor="imp-pass">口令（加密文件）</Label>
          <Input id="imp-pass" type="password" value={pass} onChange={(e) => setPass(e.target.value)} />
        </div>
        <Button onClick={() => imp.mutate()} disabled={imp.isPending}>
          <Upload className="size-4" /> {imp.isPending ? '导入中…' : '导入'}
        </Button>
      </CardContent>
    </Card>
  )
}

function CCSwitchCard() {
  const qc = useQueryClient()
  const fileRef = useRef<HTMLInputElement>(null)
  const [result, setResult] = useState<CCSwitchImportResp | null>(null)
  const imp = useMutation({
    mutationFn: async () => {
      const f = fileRef.current?.files?.[0]
      if (!f) throw new Error('请选择 cc-switch.db 或 config.json')
      const resp = await fetch('/api/v1/import/cc-switch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: f,
      })
      const data = (await resp.json()) as CCSwitchImportResp & { error?: { message: string } }
      if (!resp.ok) throw new Error(data.error?.message ?? `HTTP ${resp.status}`)
      return data
    },
    onSuccess: (r) => {
      setResult(r)
      toast.success(`cc-switch 导入完成：${r.imported.length} 成功，${r.skipped.length} 跳过`)
      qc.invalidateQueries()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">从 cc-switch 迁移</CardTitle>
        <CardDescription>
          上传 <code className="font-mono">~/.cc-switch/cc-switch.db</code>（v3.8+）或旧版 config.json。
          可映射项全部导入；协议转换类/OAuth 托管类/回环地址/空 key 会跳过并列明原因。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex gap-2">
          <Input ref={fileRef} type="file" accept=".db,.json,application/octet-stream,application/json" />
          <Button onClick={() => imp.mutate()} disabled={imp.isPending}>
            {imp.isPending ? '导入中…' : '导入'}
          </Button>
        </div>
        {result && (
          <div className="space-y-2">
            <p className="text-sm">
              成功导入 <b>{result.imported.length}</b> 项
              {result.imported.length > 0 && (
                <span className="text-muted-foreground">
                  ：{result.imported.map((i) => i.name).join('、')}
                </span>
              )}
            </p>
            {result.skipped.length > 0 && (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>跳过项</TableHead>
                    <TableHead>原因</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {result.skipped.map((s2, i) => (
                    <TableRow key={i}>
                      <TableCell>
                        {s2.name} <Badge variant="outline">{s2.app}</Badge>
                      </TableCell>
                      <TableCell className="text-muted-foreground">{s2.reason}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

export default function SettingsPage() {
  return (
    <div className="space-y-6">
      <h2 className="text-lg font-semibold">设置</h2>
      <div className="grid gap-4 lg:grid-cols-2">
        <BackupCard />
        <ExportCard />
        <ImportCard />
        <CCSwitchCard />
      </div>
    </div>
  )
}
