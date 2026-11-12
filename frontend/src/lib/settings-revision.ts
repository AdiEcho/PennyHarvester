// Settings have no revision field in the API. Keep a small browser-side
// revision per account so caches that depend on applied settings (such as
// order-book quotes) can be invalidated when a settings save succeeds. This is
// not an application revision and never substitutes for the server's value.
const PREFIX = 'pennyharvester.settings-revision.'

export function readSettingsRevision(accountId: number | undefined): number {
  if (!accountId) return 0
  try {
    const value = Number(window.localStorage.getItem(`${PREFIX}${accountId}`))
    return Number.isFinite(value) && value > 0 ? value : 0
  } catch {
    return 0
  }
}

export function bumpSettingsRevision(accountId: number | undefined): number {
  if (!accountId) return 0
  const next = readSettingsRevision(accountId) + 1
  try {
    window.localStorage.setItem(`${PREFIX}${accountId}`, String(next))
  } catch {
    // Private browsing or a blocked storage area only costs cache invalidation;
    // it must not make a successful settings save look failed.
  }
  return next
}
