import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const dist = path.resolve(here, '..', 'dist')
const port = Number(process.env.PORT || 4173)

const now = '2026-11-13T02:00:00Z'
const account = {
  id: 1,
  name: 'E2E 模拟账户',
  mode: 'paper',
  venue: 'polymarket',
  currency: 'pUSD',
  is_default: true,
  auto_trade: false,
  baseline_equity: 1000,
  created_at: now,
  live_configured: false,
  live_ready: false,
  live_signer: '',
  live_maker: '',
  live_error: '',
}

const metrics = {
  mode: 'paper',
  venue: 'polymarket',
  currency: 'pUSD',
  initial_bankroll: 1000,
  cash: 1000,
  wallet_cash: 0,
  wallet_cash_stale: false,
  equity: 1000,
  realized_pnl: 0,
  unrealized_pnl: 0,
  total_pnl: 0,
  fees_paid: 0,
  total_return: 0,
  open_exposure: 0,
  open_positions: 0,
  settled_positions: 0,
  split_positions: 0,
  stopped_positions: 0,
  win_rate: 0,
  auto_trade_started_at: null,
  annualized_return: 0,
  annualized_days: 0,
  total_orders: 0,
}

function initialOperation() {
  return {
    id: 1,
    account_id: 1,
    venue: 'polymarket',
    chain_id: 137,
    kind: 'redeem',
    position_id: 1,
    market_id: 'market-e2e',
    token_id: '123456789',
    from_address: '0x1111111111111111111111111111111111111111',
    nonce: 7,
    tx_hash: `0x${'ab'.repeat(32)}`,
    status: 'unknown',
    reason: 'receipt timeout; chain result not observed',
    resolution_note: null,
    resolved_by: null,
    resolved_at: null,
    created_at: now,
    updated_at: now,
  }
}

function venueOperation() {
  return {
    id: 2,
    account_id: 0,
    venue: 'polymarket',
    chain_id: 137,
    kind: 'approve',
    position_id: null,
    market_id: '',
    token_id: '',
    from_address: '0x2222222222222222222222222222222222222222',
    nonce: null,
    tx_hash: `0x${'cd'.repeat(32)}`,
    status: 'unknown',
    reason: 'venue-wide approval receipt timeout',
    resolution_note: null,
    resolved_by: null,
    resolved_at: null,
    created_at: now,
    updated_at: now,
  }
}

let operations = [initialOperation(), venueOperation()]
let resolves = []

function json(res, status, body) {
  const payload = JSON.stringify(body)
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(payload),
    'Cache-Control': 'no-store',
  })
  res.end(payload)
}

async function body(req) {
  const chunks = []
  for await (const chunk of req) chunks.push(chunk)
  if (!chunks.length) return {}
  return JSON.parse(Buffer.concat(chunks).toString('utf8'))
}

function filterOperations(url) {
  const params = url.searchParams
  const accountId = Number(params.get('account_id') || 0)
  const venue = params.get('venue') || ''
  const status = params.get('status') || ''
  const limit = Math.min(Math.max(Number(params.get('limit') || 50), 1), 200)
  const offset = Math.max(Number(params.get('offset') || 0), 0)
  const matched = operations.filter(
    (operation) =>
      (!accountId || operation.account_id === accountId) &&
      (!venue || operation.venue === venue) &&
      (!status || operation.status === status),
  )
  return { items: matched.slice(offset, offset + limit), total: matched.length, limit, offset }
}

const mime = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.json': 'application/json; charset=utf-8',
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url || '/', `http://${req.headers.host || '127.0.0.1'}`)
  try {
    if (url.pathname === '/api/v1/auth/status') return json(res, 200, { required: false, authenticated: true })
    if (url.pathname === '/api/v1/health') {
      return json(res, 200, {
        status: 'ok',
        service: 'pennyharvester-e2e',
        mode: 'paper',
        default_account_mode: 'paper',
        live_configured: false,
        live_ready: false,
        live_error: '',
        time: now,
      })
    }
    if (url.pathname === '/api/v1/accounts') return json(res, 200, { items: [account], total: 1 })
    if (url.pathname === '/api/v1/dashboard') {
      return json(res, 200, {
        account,
        metrics,
        engine: { running: false, last_scan_at: null, last_error: '', scan_count: 0, mode: 'paper' },
        source: { name: 'Polymarket Gamma', last_sync_at: now },
        equity: [{ time: now, value: 1000, venue: 'polymarket', currency: 'pUSD' }],
      })
    }
    if (url.pathname === '/api/v1/opportunities') {
      return json(res, 200, { items: [], total: 0, scanned_at: now, source: 'E2E', venues: ['polymarket'] })
    }
    if (url.pathname === '/api/v1/positions') return json(res, 200, { items: [], total: 0 })
    if (url.pathname === '/api/v1/orders') return json(res, 200, { items: [], total: 0 })
    if (url.pathname === '/api/v1/venues') {
      return json(res, 200, {
        venues: [
          {
            id: 'polymarket',
            name: 'Polymarket',
            currency: 'pUSD',
            quote_decimals: 6,
            min_notional: 1,
            min_sell_shares: 0,
            order_types: ['FAK'],
            supports_settlement: true,
            supports_redeem: true,
            testnet: false,
            read_only: false,
          },
        ],
      })
    }
    if (url.pathname === '/api/v1/chain-operations' && req.method === 'GET') {
      return json(res, 200, filterOperations(url))
    }
    const resolveMatch = url.pathname.match(/^\/api\/v1\/chain-operations\/(\d+)\/resolve$/)
    if (resolveMatch && req.method === 'POST') {
      const operation = operations.find((item) => item.id === Number(resolveMatch[1]))
      if (!operation) return json(res, 404, { error: '链上操作不存在' })
      const input = await body(req)
      if (operation.status !== 'unknown') return json(res, 409, { error: '该操作已结束，不能重复核对' })
      if (input.status !== 'confirmed' && input.status !== 'reverted') {
        return json(res, 400, { error: '核对结果必须是 confirmed 或 reverted' })
      }
      if (!input.actor?.trim() || !input.reason?.trim()) {
        return json(res, 400, { error: '核对人与核对说明不能为空' })
      }
      const txHash = input.tx_hash?.trim() || operation.tx_hash
      if (input.status === 'confirmed' && !txHash) return json(res, 400, { error: '确认上链必须填写交易哈希' })
      if (input.tx_hash && operation.tx_hash && input.tx_hash.toLowerCase() !== operation.tx_hash.toLowerCase()) {
        return json(res, 409, { error: '已记录其他交易哈希' })
      }
      operation.status = input.status
      operation.tx_hash = txHash || null
      operation.resolved_by = input.actor.trim()
      operation.resolution_note = input.reason.trim()
      operation.resolved_at = new Date().toISOString()
      operation.updated_at = operation.resolved_at
      resolves.push({ id: operation.id, ...input })
      return json(res, 200, operation)
    }
    if (url.pathname === '/api/v1/__state') return json(res, 200, { operations, resolves })
    if (url.pathname === '/api/v1/__reset' && req.method === 'POST') {
      operations = [initialOperation(), venueOperation()]
      resolves = []
      return json(res, 200, { ok: true })
    }
    if (url.pathname.startsWith('/api/')) return json(res, 404, { error: `no mock route for ${url.pathname}` })

    let file = path.join(dist, decodeURIComponent(url.pathname))
    const relative = path.relative(dist, file)
    if (relative.startsWith('..') || path.isAbsolute(relative)) return json(res, 403, { error: 'forbidden' })
    if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(dist, 'index.html')
    const payload = fs.readFileSync(file)
    res.writeHead(200, {
      'Content-Type': mime[path.extname(file)] || 'application/octet-stream',
      'Cache-Control': 'no-store',
    })
    res.end(payload)
  } catch (error) {
    json(res, 500, { error: error instanceof Error ? error.message : String(error) })
  }
})

server.listen(port, '127.0.0.1', () => {
  console.log(`E2E mock server listening on http://127.0.0.1:${port}`)
})
