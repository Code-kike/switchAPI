// 全局布局：桌面左侧栏 / 移动端底部导航（手机浏览器一等公民）。
// 首次进入用 GET /state 做鉴权探测（401 由 client.ts 全局跳登录），
// 探测通过后建立 ws/ui 实时通道。

import { useEffect } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeftRight, Gauge, LogOut, MonitorSmartphone, ScrollText, Server } from 'lucide-react'
import { apiGet, apiPost } from '@/api/client'
import type { StateResp } from '@/api/types'
import { startWs, stopWs } from '@/ws'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

const NAV = [
  { to: '/', label: '仪表盘', icon: Gauge },
  { to: '/providers', label: '供应商', icon: Server },
  { to: '/usage', label: '用量', icon: ArrowLeftRight },
  { to: '/devices', label: '设备', icon: MonitorSmartphone },
  { to: '/events', label: '事件', icon: ScrollText },
]

export function Layout() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { isSuccess } = useQuery({
    queryKey: ['state'],
    queryFn: () => apiGet<StateResp>('/api/v1/state'),
  })

  useEffect(() => {
    if (isSuccess) startWs(qc)
    return () => stopWs()
  }, [isSuccess, qc])

  const logout = async () => {
    await apiPost('/api/v1/auth/logout')
    stopWs()
    navigate('/login')
  }

  return (
    <div className="flex min-h-svh">
      {/* 桌面侧边栏 */}
      <aside className="hidden w-52 shrink-0 flex-col border-r bg-sidebar md:flex">
        <div className="flex h-14 items-center px-5 text-lg font-semibold">switchAPI</div>
        <nav className="flex-1 space-y-1 px-3">
          {NAV.map(({ to, label, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              end={to === '/'}
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-3 rounded-md px-3 py-2 text-sm transition-colors',
                  isActive
                    ? 'bg-sidebar-accent font-medium text-sidebar-accent-foreground'
                    : 'text-muted-foreground hover:bg-sidebar-accent/50',
                )
              }
            >
              <Icon className="size-4" />
              {label}
            </NavLink>
          ))}
        </nav>
        <div className="p-3">
          <Button variant="ghost" className="w-full justify-start gap-3 text-muted-foreground" onClick={logout}>
            <LogOut className="size-4" />
            退出登录
          </Button>
        </div>
      </aside>

      {/* 主内容 */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center justify-between border-b px-4 md:hidden">
          <span className="text-lg font-semibold">switchAPI</span>
          <Button variant="ghost" size="icon" onClick={logout} aria-label="退出登录">
            <LogOut className="size-4" />
          </Button>
        </header>
        <main className="flex-1 p-4 pb-20 md:p-6 md:pb-6">
          <Outlet />
        </main>
      </div>

      {/* 移动端底部导航 */}
      <nav className="fixed inset-x-0 bottom-0 z-10 flex border-t bg-background md:hidden">
        {NAV.map(({ to, label, icon: Icon }) => (
          <NavLink
            key={to}
            to={to}
            end={to === '/'}
            className={({ isActive }) =>
              cn(
                'flex flex-1 flex-col items-center gap-0.5 py-2 text-[11px]',
                isActive ? 'text-primary' : 'text-muted-foreground',
              )
            }
          >
            <Icon className="size-5" />
            {label}
          </NavLink>
        ))}
      </nav>
    </div>
  )
}
