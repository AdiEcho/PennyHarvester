import type { Settings, Venue, VenueSettings } from '@/types'
import { VENUES, venueMeta } from '@/lib/venue'

export const numericKeys = ['min_price', 'max_price', 'max_days', 'min_liquidity', 'order_size', 'max_position', 'max_exposure', 'max_event_exposure', 'scan_interval_seconds', 'slippage_bps', 'fee_bps', 'max_price_change_24h', 'entry_window_minutes', 'sports_entry_window_minutes', 'crypto_entry_window_seconds', 'stop_loss_ratio', 'max_price_drift_bps', 'crypto_min_confidence'] as const
export const selectKeys = ['default_category', 'sports_policy'] as const
export type NumericKey = typeof numericKeys[number]
export type SelectKey = typeof selectKeys[number]
export type SharedDraft = Record<NumericKey, string> & Record<SelectKey, string> & { stop_loss_enabled: boolean }

export const venueNumericKeys = ['min_liquidity', 'order_size', 'max_position', 'max_exposure', 'fee_bps'] as const
export type VenueNumericKey = typeof venueNumericKeys[number]
export type VenueDraft = Record<VenueNumericKey, string> & { enabled: boolean }

export interface Draft {
  shared: SharedDraft
  venues: Record<Venue, VenueDraft>
}

// Error keys are either a shared numeric key or `${venue}.${venueKey}` so each
// tab only shows the errors that belong to it.
export type FieldErrors = Record<string, string | undefined>
export type SettingsTab = 'shared' | Venue

export function toVenueDraft(venue: Venue, value?: VenueSettings): VenueDraft {
  // 0 / missing means "inherit the global value", so it maps to an empty field
  // whose placeholder shows the inherited number. A venue with no stored entry
  // keeps its platform default (Predict.fun stays disabled until enabled here).
  return {
    enabled: value?.enabled ?? venueMeta(venue).enabledByDefault,
    min_liquidity: value?.min_liquidity ? String(value.min_liquidity) : '',
    order_size: value?.order_size ? String(value.order_size) : '',
    max_position: value?.max_position ? String(value.max_position) : '',
    max_exposure: value?.max_exposure ? String(value.max_exposure) : '',
    fee_bps: value?.fee_bps ? String(value.fee_bps) : '',
  }
}

export function toDraft(settings: Settings): Draft {
  return {
    shared: {
      min_price: String(settings.min_price),
      max_price: String(settings.max_price),
      max_days: String(settings.max_days),
      min_liquidity: String(settings.min_liquidity),
      order_size: String(settings.order_size),
      max_position: String(settings.max_position),
      max_exposure: String(settings.max_exposure),
      max_event_exposure: String(settings.max_event_exposure ?? 0),
      scan_interval_seconds: String(settings.scan_interval_seconds),
      slippage_bps: String(settings.slippage_bps),
      fee_bps: String(settings.fee_bps),
      default_category: settings.default_category,
      sports_policy: settings.sports_policy,
      max_price_change_24h: String(settings.max_price_change_24h),
      entry_window_minutes: String(settings.entry_window_minutes),
      sports_entry_window_minutes: String(settings.sports_entry_window_minutes),
      crypto_entry_window_seconds: String(settings.crypto_entry_window_seconds),
      stop_loss_ratio: String(settings.stop_loss_ratio),
      stop_loss_enabled: settings.stop_loss_enabled,
      max_price_drift_bps: String(settings.max_price_drift_bps ?? 50),
      crypto_min_confidence: String(settings.crypto_min_confidence ?? 0.5),
    },
    venues: {
      polymarket: toVenueDraft('polymarket', settings.venues?.polymarket),
      predictfun: toVenueDraft('predictfun', settings.venues?.predictfun),
    },
  }
}

export function toShared(draft: SharedDraft) {
  return {
    min_price: Number(draft.min_price),
    max_price: Number(draft.max_price),
    max_days: Number(draft.max_days),
    min_liquidity: Number(draft.min_liquidity),
    order_size: Number(draft.order_size),
    max_position: Number(draft.max_position),
    max_exposure: Number(draft.max_exposure),
    max_event_exposure: Number(draft.max_event_exposure),
    scan_interval_seconds: Number(draft.scan_interval_seconds),
    slippage_bps: Number(draft.slippage_bps),
    fee_bps: Number(draft.fee_bps),
    default_category: draft.default_category,
    sports_policy: draft.sports_policy,
    max_price_change_24h: Number(draft.max_price_change_24h),
    entry_window_minutes: Number(draft.entry_window_minutes),
    sports_entry_window_minutes: Number(draft.sports_entry_window_minutes),
    crypto_entry_window_seconds: Number(draft.crypto_entry_window_seconds),
    stop_loss_ratio: Number(draft.stop_loss_ratio),
    stop_loss_enabled: draft.stop_loss_enabled,
    max_price_drift_bps: Number(draft.max_price_drift_bps),
    crypto_min_confidence: Number(draft.crypto_min_confidence),
  }
}

/** Empty field = inherit (0), per the API contract. */
export function numberOrZero(value: string): number {
  return value.trim() === '' ? 0 : Number(value)
}

/**
 * Builds the PUT body from the loaded object, never from a fresh literal: fields
 * the form does not render (new backend fields, sibling venue values) survive.
 */
export function toPayload(base: Settings, draft: Draft): Settings {
  const venues = {} as Record<Venue, VenueSettings>
  for (const venue of VENUES) {
    const loaded = base.venues?.[venue]
    const current = draft.venues[venue]
    venues[venue] = {
      ...(loaded ?? { enabled: venueMeta(venue).enabledByDefault, min_liquidity: 0, order_size: 0, max_position: 0, max_exposure: 0, fee_bps: 0 }),
      enabled: current.enabled,
      min_liquidity: numberOrZero(current.min_liquidity),
      order_size: numberOrZero(current.order_size),
      max_position: numberOrZero(current.max_position),
      max_exposure: numberOrZero(current.max_exposure),
      fee_bps: numberOrZero(current.fee_bps),
    }
  }
  return { ...base, ...toShared(draft.shared), venues }
}
