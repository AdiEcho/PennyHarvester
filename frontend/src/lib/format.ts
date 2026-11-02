// Money is formatted by fmtMoney() in lib/venue.ts so the venue currency is
// always part of the string. There is deliberately no bare USD formatter here:
// a USDT amount must never be rendered with a dollar sign.
const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 })

export function formatNumber(value: number, maximumFractionDigits = 2) {
  if (!Number.isFinite(value)) return '—'
  return new Intl.NumberFormat('zh-CN', { maximumFractionDigits }).format(value)
}

export function formatPercent(ratio: number, digits = 2, signed = false) {
  if (!Number.isFinite(ratio)) return '—'
  return new Intl.NumberFormat('zh-CN', {
    style: 'percent',
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
    signDisplay: signed ? 'exceptZero' : 'auto',
  }).format(ratio)
}

// A price is a probability per share (0–1), not a currency amount, so it is
// printed without a currency symbol: the venue chip/currency label supplies the
// unit and USDC/USDT can never be confused here.
export function formatPrice(value: number) {
  if (!Number.isFinite(value)) return '—'
  return value.toFixed(3)
}

// Gamma reports oneDayPriceChange in probability dollars (0.03 = 3 cents).
// Show cents because the strategy's whole edge lives in the last few cents.
export function formatPriceChange(value: number | null | undefined) {
  if (value == null || !Number.isFinite(value)) return '—'
  const cents = value * 100
  return `${cents > 0 ? '+' : ''}${cents.toFixed(1)}¢`
}

export function formatDate(value: string | null, includeTime = true) {
  if (!value) return '暂无记录'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    ...(includeTime ? { hour: '2-digit', minute: '2-digit', hour12: false } : {}),
  }).format(date)
}

export function formatFullDate(value: string | null) {
  if (!value) return '暂无记录'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '时间未知'
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}

export function formatAxisDate(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date)
}

export function pnlClass(value: number) {
  if (value > 0) return 'text-emerald-700'
  if (value < 0) return 'text-rose-600'
  return 'text-slate-600'
}

export { number as numberFormatter }
