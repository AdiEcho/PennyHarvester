import type { Venue } from '@/types'
import type { Currency } from '@/lib/venue'
import { VENUES, venueMeta } from '@/lib/venue'
import { type Draft, type FieldErrors, type SharedDraft, type VenueNumericKey, numericKeys, numberOrZero, toShared, venueNumericKeys } from '@/lib/settings-draft'
import { defaultCategoryOptions, sportsPolicyOptions } from '@/lib/category'

export { defaultCategoryOptions, sportsPolicyOptions }

/** One venue whose effective order size could not produce a sellable position. */
export interface StopLossFloorViolation {
  venue: Venue
  label: string
  currency: Currency
  floor: number
  orderSize: number
}

// Cross-field rules compare *effective* values (override or inherited) so an
// inherited single-order size cannot silently exceed its venue caps.
export function validateShared(shared: SharedDraft): FieldErrors {
  const errors: FieldErrors = {}
  const settings = toShared(shared)
  for (const key of numericKeys) {
    if (!Number.isFinite(settings[key])) errors[key] = '请输入有效数字。'
  }
  if (settings.min_price <= 0) errors.min_price = '最低价格必须大于 0。'
  if (settings.max_price >= 1) errors.max_price = '最高价格必须小于 1。'
  if (settings.min_price > settings.max_price) {
    errors.min_price = '最低价格不能高于最高价格。'
    errors.max_price = '最高价格不能低于最低价格。'
  }
  if (!Number.isInteger(settings.max_days) || settings.max_days < 1 || settings.max_days > 3650) errors.max_days = '请输入 1–3650 之间的整数天数。'
  if (settings.min_liquidity < 0) errors.min_liquidity = '最低流动性不能小于 0。'
  if (settings.order_size <= 0) errors.order_size = '单笔金额必须大于 0。'
  if (settings.max_position < settings.order_size) errors.max_position = '单市场持仓上限不能低于单笔金额。'
  if (settings.max_exposure < settings.max_position) errors.max_exposure = '总敞口上限不能低于单市场持仓上限。'
  if (settings.max_event_exposure < 0) errors.max_event_exposure = '单事件敞口上限不能小于 0（0 表示不限）。'
  else if (settings.max_event_exposure > 0 && settings.max_event_exposure < settings.order_size) errors.max_event_exposure = '单事件敞口上限不能低于单笔金额（0 表示不限）。'
  if (!Number.isInteger(settings.scan_interval_seconds) || settings.scan_interval_seconds < 10 || settings.scan_interval_seconds > 86400) errors.scan_interval_seconds = '请输入 10–86400 之间的整数秒数。'
  if (settings.slippage_bps < 0 || settings.slippage_bps > 1000) errors.slippage_bps = '滑点范围为 0–1000 bps。'
  if (settings.fee_bps < 0 || settings.fee_bps > 1000) errors.fee_bps = '费用范围为 0–1000 bps。'
  if (!defaultCategoryOptions.some((option) => option.value === settings.default_category)) errors.default_category = '请选择有效的默认分类。'
  if (!sportsPolicyOptions.some((option) => option.value === settings.sports_policy)) errors.sports_policy = '请选择有效的体育市场策略。'
  if (settings.max_price_change_24h < 0 || settings.max_price_change_24h > 1) errors.max_price_change_24h = '请输入 0–1 之间的数值（0 表示不限制）。'
  if (!Number.isInteger(settings.entry_window_minutes) || settings.entry_window_minutes < 0 || settings.entry_window_minutes > 1440) errors.entry_window_minutes = '请输入 0–1440 之间的整数分钟数。'
  if (!Number.isInteger(settings.sports_entry_window_minutes) || settings.sports_entry_window_minutes < 0 || settings.sports_entry_window_minutes > 1440) errors.sports_entry_window_minutes = '请输入 0–1440 之间的整数分钟数。'
  if (!Number.isInteger(settings.crypto_entry_window_seconds) || settings.crypto_entry_window_seconds < 0 || settings.crypto_entry_window_seconds > 86400) errors.crypto_entry_window_seconds = '请输入 0–86400 之间的整数秒数。'
  if (settings.stop_loss_ratio <= 0 || settings.stop_loss_ratio >= 1) errors.stop_loss_ratio = '请输入 0–1 之间的小数，例如 0.70。'
  if (settings.max_price_drift_bps < 0 || settings.max_price_drift_bps > 1000) errors.max_price_drift_bps = '请输入 0–1000 之间的数值。'
  if (settings.crypto_min_confidence < 0 || settings.crypto_min_confidence > 1) errors.crypto_min_confidence = '请输入 0–1 之间的数值（0 表示不限制）。'
  return errors
}

/**
 * Venues where the stop-loss would promise an exit the exchange refuses. A buy
 * is sized in money, but the exit is a limit order the venue floors by shares,
 * so a per-venue order size below that floor always lands under it (a share
 * costs less than one currency unit). The backend refuses to enable the
 * stop-loss in that state; the form mirrors it so the reason is visible before
 * the save round-trip.
 */
export function stopLossFloorViolations(draft: Draft): StopLossFloorViolation[] {
  if (!draft.shared.stop_loss_enabled) return []
  const shared = toShared(draft.shared)
  const violations: StopLossFloorViolation[] = []
  for (const venue of VENUES) {
    const meta = venueMeta(venue)
    if (!meta.minSellShares) continue
    if (!draft.venues[venue].enabled) continue
    const orderSize = numberOrZero(draft.venues[venue].order_size) || shared.order_size
    if (orderSize + 1e-9 >= meta.minSellShares) continue
    violations.push({ venue, label: meta.label, currency: meta.currency, floor: meta.minSellShares, orderSize })
  }
  return violations
}

export function validate(draft: Draft): FieldErrors {
  const errors = validateShared(draft.shared)
  const shared = toShared(draft.shared)
  for (const venue of VENUES) {
    const current = draft.venues[venue]
    const key = (field: VenueNumericKey) => `${venue}.${field}`
    for (const field of venueNumericKeys) {
      if (current[field].trim() !== '' && !Number.isFinite(Number(current[field]))) {
        errors[key(field)] = '请输入有效数字；留空表示继承账户策略值。'
      }
    }
    const orderSize = numberOrZero(current.order_size) || shared.order_size
    const maxPosition = numberOrZero(current.max_position) || shared.max_position
    const maxExposure = numberOrZero(current.max_exposure) || shared.max_exposure
    const feeBps = numberOrZero(current.fee_bps)
    if (current.order_size.trim() !== '' && orderSize <= 0) errors[key('order_size')] = '单笔金额必须大于 0。'
    if (maxPosition < orderSize) errors[key('max_position')] = `单市场持仓上限不能低于单笔金额（当前生效 ${orderSize}）。`
    if (maxExposure < maxPosition) errors[key('max_exposure')] = `总敞口上限不能低于单市场持仓上限（当前生效 ${maxPosition}）。`
    if ((current.fee_bps.trim() !== '' && (feeBps < 0 || feeBps > 1000)) || feeBps > 1000) errors[key('fee_bps')] = '费用范围为 0–1000 bps（留空表示继承账户策略值）。'
    if (current.min_liquidity.trim() !== '' && numberOrZero(current.min_liquidity) < 0) errors[key('min_liquidity')] = '最低流动性不能小于 0。'
  }
  // The stop-loss sells a whole position as a limit order, and every venue floors
  // such an order by shares. An order size in money below that floor buys a
  // position the bot can never exit, so the backend refuses to enable the
  // stop-loss then; mirror the rule here so the reason lands on the field.
  for (const violation of stopLossFloorViolations(draft)) {
    const message = `开启自动止损需要${violation.label}单笔金额不低于 ${violation.floor} ${violation.currency}（${violation.label} 卖单最小 ${violation.floor} 份，单笔金额更低就买不到能卖出的份额）。`
    if (draft.venues[violation.venue].order_size.trim() !== '') errors[`${violation.venue}.order_size`] = message
    else errors.order_size = message
  }
  return errors
}
