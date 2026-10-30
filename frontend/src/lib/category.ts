import type { Opportunity } from '@/types'

export type CategoryFilter = 'all' | 'non_sports' | 'sports' | 'politics' | 'crypto' | 'finance' | 'tech' | 'culture' | 'other'

export const categoryFilters: { value: CategoryFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'non_sports', label: '非体育' },
  { value: 'politics', label: '政治' },
  { value: 'crypto', label: '加密' },
  { value: 'finance', label: '金融' },
  { value: 'tech', label: '科技' },
  { value: 'culture', label: '文化' },
  { value: 'sports', label: '体育' },
  { value: 'other', label: '其他' },
]

const categoryLabels: Record<string, string> = {
  sports: '体育',
  politics: '政治',
  crypto: '加密',
  finance: '金融',
  tech: '科技',
  culture: '文化',
  other: '其他',
}

export function categoryLabel(category?: string | null) {
  return categoryLabels[category || ''] || '其他'
}

export function isCategoryFilter(value: string | null | undefined): value is CategoryFilter {
  return categoryFilters.some((option) => option.value === value)
}

export function normalizeCategoryFilter(value: string | null | undefined): CategoryFilter {
  return isCategoryFilter(value) ? value : 'non_sports'
}

export function matchesCategory(item: Opportunity, filter: CategoryFilter) {
  if (filter === 'all') return true
  if (filter === 'non_sports') return item.category !== 'sports'
  return item.category === filter
}

export function categoryCounts(items: Opportunity[]) {
  const counts: Record<CategoryFilter, number> = {
    all: items.length,
    non_sports: 0,
    sports: 0,
    politics: 0,
    crypto: 0,
    finance: 0,
    tech: 0,
    culture: 0,
    other: 0,
  }
  for (const item of items) {
    if (item.category !== 'sports') counts.non_sports++
    const key = isCategoryFilter(item.category) ? item.category : 'other'
    counts[key]++
  }
  return counts
}
