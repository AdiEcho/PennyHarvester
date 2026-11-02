// Venue vocabulary for the multi-venue UI.
//
// Polymarket settles in USDC on Polygon, Predict.fun in USDT on BNB Chain. The
// two currencies are different assets, so this module is the only place that
// knows venue names, labels and currency formatting, and every money render in
// the UI goes through fmtMoney(). Nothing here converts between currencies.
import type { Settings } from '@/types'

export type Venue = 'polymarket' | 'predictfun'
export type Currency = 'USDC' | 'USDT'

export type VenueTone = 'info' | 'default'

export interface VenueMeta {
  /** Canonical id used by the API. */
  venue: Venue
  label: string
  /** Two-letter tag for dense table cells. */
  short: 'PM' | 'PF'
  currency: Currency
  /** Collateral decimals; copy only, the UI never does raw unit math. */
  quoteDecimals: number
  /** Collateral token name as the venue names it. */
  collateral: string
  /** Badge variant used for the venue chip. */
  tone: VenueTone
  /** Chip colours for the venue filter (single source for the pill look). */
  accentClass: string
  /**
   * Smallest sell the venue accepts, in shares (mirrors
   * venue.Capabilities.MinSellShares). A buy is sized in money, an exit is a
   * limit order floored by shares, so an order size below this number of
   * currency units cannot produce a sellable position. Zero means no floor.
   */
  minSellShares: number
  orderTypes: string[]
  supportsSettlement: boolean
  settlementHint: string
  externalIdLabel: string
  envHint: string
  /** Only Polymarket markets carry a protocol version (v1/v2). */
  hasProtocolVersion: boolean
  /**
   * Whether the venue participates by default when the backend has no stored
   * settings for it. Predict.fun stays off so an upgrade cannot start trading
   * a new exchange with real money.
   */
  enabledByDefault: boolean
  defaults: { gammaBaseURL?: string; clobBaseURL?: string; baseURL?: string }
}

/** Presentation order: Polymarket first, matching the API contract. */
export const VENUES: Venue[] = ['polymarket', 'predictfun']

export const VENUE_META: Record<Venue, VenueMeta> = {
  polymarket: {
    venue: 'polymarket',
    label: 'Polymarket',
    short: 'PM',
    currency: 'USDC',
    quoteDecimals: 6,
    collateral: 'pUSD',
    tone: 'info',
    accentClass: 'border-blue-200 bg-blue-50 text-blue-700',
    minSellShares: 5,
    orderTypes: ['FAK', 'FOK'],
    supportsSettlement: true,
    settlementHint: '结算损益为链上可领取权益，需在 Polymarket 手动领取（赎回需自付 Polygon gas）。',
    externalIdLabel: 'CLOB',
    envHint: 'POLYMARKET_PRIVATE_KEY',
    hasProtocolVersion: true,
    enabledByDefault: true,
    defaults: {
      gammaBaseURL: 'https://gamma-api.polymarket.com',
      clobBaseURL: 'https://clob.polymarket.com',
    },
  },
  predictfun: {
    venue: 'predictfun',
    label: 'Predict.fun',
    short: 'PF',
    currency: 'USDT',
    quoteDecimals: 18,
    collateral: 'USDT',
    tone: 'default',
    accentClass: 'border-slate-300 bg-slate-100 text-slate-700',
    minSellShares: 0,
    orderTypes: ['LIMIT', 'MARKET'],
    supportsSettlement: true,
    settlementHint: '结算结果由 Predict.fun 公布的获胜结果映射（引擎据此关账），链上份额仍需在 Predict.fun 手动赎回（需自付 BNB gas）。',
    externalIdLabel: 'PF 订单',
    envHint: 'PREDICTFUN_PRIVATE_KEY',
    hasProtocolVersion: false,
    enabledByDefault: false,
    defaults: { baseURL: 'https://api.predict.fun' },
  },
}

export function isVenue(value: unknown): value is Venue {
  return value === 'polymarket' || value === 'predictfun'
}

/** Missing or empty venue always means Polymarket (historical behaviour). */
export function venueOf(value: { venue?: string | null } | null | undefined): Venue {
  const venue = value?.venue
  return isVenue(venue) ? venue : 'polymarket'
}

export function venueMeta(venue: Venue | string | null | undefined): VenueMeta {
  return VENUE_META[isVenue(venue) ? venue : 'polymarket']
}

export function currencyOf(venue: Venue | string | null | undefined): Currency {
  return venueMeta(venue).currency
}

export function venueLabel(venue: Venue | string | null | undefined): string {
  return venueMeta(venue).label
}

export function venueShort(venue: Venue | string | null | undefined): 'PM' | 'PF' {
  return venueMeta(venue).short
}

/** Protocol versions (v1/v2) exist on Polymarket only. */
export function protocolVersionOf(item: { venue?: string | null; version?: string | null }): string | null {
  if (!venueMeta(venueOf(item)).hasProtocolVersion) return null
  const version = item.version?.trim()
  return version || null
}

export function settlementHintOf(venue: Venue | string | null | undefined): string {
  return venueMeta(venue).settlementHint
}

export function envHintOf(venue: Venue | string | null | undefined): string {
  return venueMeta(venue).envHint
}

/** Zero/absent venue overrides inherit the global value (contract §4). */
export function venueOrderSize(settings: Settings | null | undefined, venue: Venue): number {
  const override = settings?.venues?.[venue]?.order_size
  if (override && override > 0) return override
  return settings?.order_size && settings.order_size > 0 ? settings.order_size : 0
}

const moneyFormats = new Map<string, Intl.NumberFormat>()

function moneyFormat(digits: number, compact: boolean): Intl.NumberFormat {
  const key = `${digits}:${compact ? 'compact' : 'full'}`
  let format = moneyFormats.get(key)
  if (!format) {
    format = new Intl.NumberFormat('en-US', {
      minimumFractionDigits: compact ? 0 : digits,
      maximumFractionDigits: compact ? 1 : digits,
      ...(compact ? { notation: 'compact' as const } : {}),
    })
    moneyFormats.set(key, format)
  }
  return format
}

interface MoneyOptions {
  /** Prefix positive values with "+" (P&L style). */
  signed?: boolean
  /** Fraction digits; defaults to 2. */
  digits?: number
  /** Compact notation for dense cells (1.2K). */
  compact?: boolean
}

/**
 * Formats an amount with the venue's currency code, e.g. "12.34 USDT".
 * The code is always printed so a USDT number can never be mistaken for USD.
 */
export function fmtMoney(amount: number, venue?: Venue | string | null, options: MoneyOptions = {}): string {
  if (!Number.isFinite(amount)) return '—'
  const code = currencyOf(venue)
  const text = moneyFormat(options.digits ?? 2, Boolean(options.compact)).format(amount)
  if (options.signed && amount > 0) return `+${text} ${code}`
  return `${text} ${code}`
}

/**
 * Axis ticks for per-venue charts: no symbol, no code (the card header carries
 * the currency). Used so a USDT axis never renders a "$".
 */
export function fmtMoneyAxis(value: number): string {
  if (!Number.isFinite(value)) return ''
  if (Math.abs(value) >= 100000) return moneyFormat(0, true).format(value)
  return moneyFormat(2, false).format(value)
}
