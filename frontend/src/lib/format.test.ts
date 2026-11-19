import { describe, expect, it } from 'vitest'
import { formatDate, formatFullDate, formatWindowAxis } from './format'

// The API returns UTC instants. Every clock in the UI must read Beijing time,
// so 2026-11-13T02:10:00Z is 11/13 10:10 there and stays that way even when the
// browser (or the test runner) sits in another timezone.
const utcInstant = '2026-11-13T02:10:00Z'

describe('beijing time formatting', () => {
  it('shows an instant in Beijing time regardless of the local zone', () => {
    expect(formatDate(utcInstant)).toBe('11/13 10:10')
    expect(formatFullDate(utcInstant)).toBe('2026/11/13 10:10:00')
    expect(formatWindowAxis(Date.parse(utcInstant), '7d')).toBe('11/13')
  })

  it('labels equity axes by window', () => {
    expect(formatWindowAxis(Date.parse(utcInstant), '1d')).toBe('10:10')
    expect(formatWindowAxis(Date.parse(utcInstant), '3d')).toBe('11/13 10:10')
  })

  it('drops the clock when only the day is asked for', () => {
    expect(formatDate(utcInstant, false)).toBe('11/13')
  })

  it('rolls the day over at the Beijing boundary, not at UTC', () => {
    // 2026-11-13T17:30:00Z is already 11/14 in Beijing.
    expect(formatDate('2026-11-13T17:30:00Z')).toBe('11/14 01:30')
  })

  it('keeps the empty and invalid placeholders', () => {
    expect(formatDate(null)).toBe('暂无记录')
    expect(formatFullDate(null)).toBe('暂无记录')
    expect(formatDate('not-a-time')).toBe('时间未知')
    expect(formatFullDate('not-a-time')).toBe('时间未知')
    expect(formatWindowAxis(Number.NaN, '1d')).toBe('')
  })
})
