// 轻量 fetch 封装：同源 cookie、统一错误、401 全局跳登录（登录接口本身除外）。

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (resp.status === 401 && path !== '/api/v1/auth/login') {
    if (window.location.pathname !== '/login') {
      window.location.href = '/login'
    }
    throw new ApiError(401, '未登录或会话已过期')
  }
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`
    try {
      const data = (await resp.json()) as { error?: { message?: string } }
      if (data.error?.message) msg = data.error.message
    } catch {
      /* 非 JSON 错误体，保留状态码信息 */
    }
    throw new ApiError(resp.status, msg)
  }
  return (await resp.json()) as T
}

export const apiGet = <T>(path: string) => request<T>('GET', path)
export const apiPost = <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {})
export const apiPut = <T>(path: string, body?: unknown) => request<T>('PUT', path, body ?? {})
export const apiDelete = <T>(path: string) => request<T>('DELETE', path)

/** 把非空查询参数拼成 querystring。 */
export function qs(params: Record<string, string | number | undefined | null>): string {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ''
}
