import type { Opportunity } from '@/types'

export type MarketLanguage = 'zh' | 'en'

const STORAGE_KEY = 'pennyharvester.market-language'

export function readMarketLanguage(): MarketLanguage {
  if (typeof window === 'undefined') return 'zh'
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY)
    if (stored === 'zh' || stored === 'en') return stored
  } catch {
    // Ignore storage access errors (private mode, blocked storage, ...).
  }
  return 'zh'
}

export function persistMarketLanguage(language: MarketLanguage) {
  try {
    window.localStorage.setItem(STORAGE_KEY, language)
  } catch {
    // Ignore storage access errors.
  }
}

export function marketQuestion(item: Opportunity, language: MarketLanguage): string {
  if (language === 'zh') return item.question_zh?.trim() || item.question
  return item.question
}

export function marketOutcome(item: Opportunity, language: MarketLanguage): string {
  if (language === 'zh') return item.outcome_zh?.trim() || item.outcome
  return item.outcome
}

// Polymarket serves localized market pages under /zh. Keep the external link
// consistent with the language selected for the market text.
export function marketURL(url: string, language: MarketLanguage): string {
  if (language !== 'zh' || !/^https?:\/\//i.test(url)) return url
  try {
    const parsed = new URL(url)
    if (parsed.hostname === 'polymarket.com' && !parsed.pathname.startsWith('/zh/')) {
      parsed.pathname = `/zh${parsed.pathname}`
    }
    return parsed.toString()
  } catch {
    return url
  }
}
