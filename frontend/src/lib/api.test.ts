import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from './api'

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

describe('api request contract', () => {
  it('encodes account, venue, status and pagination in one request', async () => {
    fetchMock.mockResolvedValueOnce(json({ items: [], total: 0, limit: 25, offset: 50 }))

    await api.orders(7, 'all', 25, 50, 'predictfun')

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/orders?status=all&limit=25&offset=50&account_id=7&venue=predictfun',
      expect.objectContaining({
        method: 'GET',
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      }),
    )
  })

  it('omits the UI-only all status and empty chain-operation filters', async () => {
    fetchMock.mockResolvedValueOnce(json({ items: [], total: 0, limit: 50, offset: 0 }))

    await api.chainOperations({ account_id: 7, venue: 'polymarket', status: 'all', limit: 50, offset: 100 })

    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/chain-operations?account_id=7&venue=polymarket&limit=50&offset=100')
  })

  it('posts a chain-operation resolution with the exact audit body', async () => {
    fetchMock.mockResolvedValueOnce(json({ id: 9, status: 'confirmed' }))

    await api.resolveChainOperation(9, {
      status: 'confirmed',
      reason: 'checked polygonscan receipt',
      actor: 'ops-1',
      tx_hash: '0x' + 'ab'.repeat(32),
    })

    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/chain-operations/9/resolve')
    expect(init?.method).toBe('POST')
    expect(init?.body).toBe(JSON.stringify({
      status: 'confirmed',
      reason: 'checked polygonscan receipt',
      actor: 'ops-1',
      tx_hash: '0x' + 'ab'.repeat(32),
    }))
  })

  it('does not send account or venue when no scope is selected', async () => {
    fetchMock.mockResolvedValueOnce(json({ items: [], total: 0 }))

    await api.positions(undefined, 'open', null)

    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/positions?status=open')
  })

  it('keeps the exact JSON body and content type for mutations', async () => {
    fetchMock.mockResolvedValueOnce(json({ ok: true }))

    await api.login('secret')

    const [, init] = fetchMock.mock.calls[0]
    expect(init?.method).toBe('POST')
    expect(init?.body).toBe(JSON.stringify({ password: 'secret' }))
    expect(init?.headers).toEqual({
      Accept: 'application/json',
      'Content-Type': 'application/json',
    })
  })

  it('surfaces a JSON server error verbatim with its status', async () => {
    fetchMock.mockResolvedValueOnce(json({ error: '账户不存在' }, 404))

    await expect(api.dashboard(99)).rejects.toMatchObject({
      name: 'ApiError',
      message: '账户不存在',
      status: 404,
    })
  })

  it('falls back to the HTTP status for a non-JSON upstream error', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response('bad gateway', { status: 502, headers: { 'Content-Type': 'text/plain' } }),
    )

    await expect(api.health()).rejects.toMatchObject({
      name: 'ApiError',
      status: 502,
      message: '请求失败（HTTP 502），请稍后重试。',
    })
  })

  it('rejects a malformed JSON success instead of returning null', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response('{"broken"', { status: 200, headers: { 'Content-Type': 'application/json' } }),
    )

    const error = await api.dashboard(1).catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(200)
    expect((error as ApiError).message).toContain('无法解析')
  })

  it('wraps transport failures without masking AbortError', async () => {
    fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    await expect(api.health()).rejects.toMatchObject({
      name: 'ApiError',
      status: 0,
      message: '无法连接后端服务，请确认服务已启动后重试。',
    })

    const abort = new DOMException('Aborted', 'AbortError')
    fetchMock.mockRejectedValueOnce(abort)
    await expect(api.health()).rejects.toBe(abort)
  })

  it('broadcasts an unauthorized event for protected endpoints only', async () => {
    const onUnauthorized = vi.fn()
    window.addEventListener('penny:unauthorized', onUnauthorized)
    fetchMock.mockResolvedValueOnce(json({ error: '未登录' }, 401))

    await expect(api.dashboard(1)).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)

    fetchMock.mockResolvedValueOnce(json({ error: '密码错误' }, 401))
    await expect(api.login('wrong')).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
    window.removeEventListener('penny:unauthorized', onUnauthorized)
  })
})
