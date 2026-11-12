import test from 'node:test'
import assert from 'node:assert/strict'
import type { FunderDiscoveryResult } from '@/types'
import { assessDetectedWallet } from './wallet-verification.ts'

const funder = '0x1111111111111111111111111111111111111111'
const signer = '0x2222222222222222222222222222222222222222'
const implementation = '0x3333333333333333333333333333333333333333'

const evidence: FunderDiscoveryResult = {
  ok: true,
  funder,
  signer,
  wallet_type: 3,
  wallet_type_name: 'Deposit',
  deployed: true,
  source: 'polymarket-profile',
  verified: true,
  chain_id: 137,
  implementation,
  owner: signer,
  evidence: 'create2-derivation',
}

test('EOA mode does not require proxy wallet verification', () => {
  const result = assessDetectedWallet(null, 'eoa', '', 0)
  assert.equal(result.status, 'not_required')
  assert.equal(result.canSubmit, true)
})

test('bytecode-derived wallet type without evidence is only a candidate', () => {
  const result = assessDetectedWallet(
    {
      ok: true,
      funder,
      signer,
      wallet_type: 3,
      wallet_type_name: 'Deposit',
      deployed: true,
      source: 'clob-proxy-wallets',
    },
    'proxy',
    funder,
    3,
  )
  assert.equal(result.status, 'candidate')
  assert.equal(result.label, '未验证，请核对')
  assert.equal(result.canSubmit, false)
  assert.match(result.reason, /字节码形态本身不能证明/)
})

test('wrong chain or owner mismatch stays unverified', () => {
  const wrongChain = assessDetectedWallet({ ...evidence, chain_id: 1 }, 'proxy', funder, 3)
  assert.equal(wrongChain.status, 'candidate')
  assert.equal(wrongChain.canSubmit, false)

  const wrongOwner = assessDetectedWallet(
    { ...evidence, owner: '0x4444444444444444444444444444444444444444' },
    'proxy',
    funder,
    3,
  )
  assert.equal(wrongOwner.status, 'candidate')
  assert.match(wrongOwner.reason, /owner 与 signer 不一致/)
})

test('only explicit complete evidence verifies a proxy wallet', () => {
  const result = assessDetectedWallet(evidence, 'proxy', funder, 3)
  assert.equal(result.status, 'verified')
  assert.equal(result.canSubmit, true)
})
