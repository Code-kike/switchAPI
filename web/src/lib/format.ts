// 展示格式化：token 数、费用（null=未知）、绝对/相对时间。

/** token 数：1234 → 1.2k，5600000 → 5.6M。 */
export function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(n)
}

/** 费用：null → "未知"（绝不显示 0 误导）。 */
export function fmtCost(cost: number | null | undefined): string {
  if (cost === null || cost === undefined) return '未知'
  if (cost >= 1) return `$${cost.toFixed(2)}`
  return `$${cost.toFixed(4)}`
}

export function fmtTime(unixSec: number): string {
  if (!unixSec) return '—'
  return new Date(unixSec * 1000).toLocaleString('zh-CN', { hour12: false })
}

/** 相对时间：xx 秒/分钟/小时/天前。 */
export function relTime(unixSec: number): string {
  if (!unixSec) return '从未'
  const diff = Math.floor(Date.now() / 1000) - unixSec
  if (diff < 10) return '刚刚'
  if (diff < 60) return `${diff} 秒前`
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  return `${Math.floor(diff / 86400)} 天前`
}

export function fmtDuration(ms: number): string {
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${ms}ms`
}
