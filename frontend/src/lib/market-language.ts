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
  if (language !== 'zh') return item.question
  const translated = item.question_zh?.trim()
  if (translated && hasCJK(translated)) return translated
  // Polymarket's Chinese UI translates the event title and the market type,
  // but not every sports market question. Use that same Chinese context when
  // Gamma only echoes the English question back.
  const eventTitle = item.event_title_zh?.trim()
  if (eventTitle && hasCJK(eventTitle)) {
    const group = localizeGroupItem(item)
    return group ? `${eventTitle} · ${group}` : eventTitle
  }
  return translated || item.question
}

export function marketOutcome(item: Opportunity, language: MarketLanguage): string {
  if (language !== 'zh') return item.outcome
  const translated = item.outcome_zh?.trim()
  if (translated && hasCJK(translated)) return translated
  return localizeOutcome(item.outcome)
}

function hasCJK(value: string): boolean {
  return /[\u3400-\u9fff]/.test(value)
}

function localizeOutcome(value: string): string {
  switch (value.trim().toLowerCase()) {
    case 'over':
      return '大'
    case 'under':
      return '小'
    case 'yes':
      return '是'
    case 'no':
      return '否'
    case 'draw':
      return '平局'
    default:
      return value
  }
}

function localizeGroupItem(item: Opportunity): string {
  const question = item.question.trim()
  const group = item.group_item_title?.trim() || ''
  const spread = question.match(/^Spread:\s*(.+)$/i)
  if (spread) return `让分：${spread[1]}`
  const exactScore = question.match(/^Exact Score:\s*(.+?)\?*$/i)
  if (exactScore) return `准确比分：${exactScore[1]}`
  const total = question.match(/^(.+?):\s*O\/U\s+(.+)$/i)
  if (total) return `总分 ${total[2]}`
  if (/^Both Teams to Score/i.test(question)) return '两队都得分'
  if (/^First Team to Score/i.test(question)) return '第一支得分球队'
  if (/^Moneyline/i.test(question)) return '胜负'
  if (/^Team Totals?/i.test(question)) return '球队总分'
  if (/^Halftime Result/i.test(question)) return '半场结果'
  if (/^Player Props/i.test(question)) return '球员道具'
  return group
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
