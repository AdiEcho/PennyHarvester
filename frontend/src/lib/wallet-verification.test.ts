import { expect, it } from 'vitest'
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

it('EOA mode does not require proxy wallet verification', () => {
  const result = assessDetectedWallet(null, 'eoa', '', 0)
  expect(result.status).toBe('not_required')
  expect(result.canSubmit).toBe(true)
})

it('EOA mode rejects a funder because funds would not be on the signer', () => {
  const result = assessDetectedWallet(null, 'eoa', funder, 0)
  expect(result.status).toBe('unverified')
  expect(result.canSubmit).toBe(false)
  expect(result.reason).toContain('不应填写 Funder')
})

it('bytecode-derived wallet type without evidence is only a candidate', () => {
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
  expect(result.status).toBe('candidate')
  expect(result.label).toBe('未验证，请核对')
  expect(result.canSubmit).toBe(false)
  expect(result.reason).toMatch(/字节码形态本身不能证明/)
})

it('wrong chain or owner mismatch stays unverified', () => {
  const wrongChain = assessDetectedWallet({ ...evidence, chain_id: 1 }, 'proxy', funder, 3)
  expect(wrongChain.status).toBe('candidate')
  expect(wrongChain.canSubmit).toBe(false)

  const wrongOwner = assessDetectedWallet(
    { ...evidence, owner: '0x4444444444444444444444444444444444444444' },
    'proxy',
    funder,
    3,
  )
  expect(wrongOwner.status).toBe('candidate')
  expect(wrongOwner.reason).toMatch(/owner 与 signer 不一致/)
})

it('funder and signature type must match the form being saved', () => {
  const otherFunder = assessDetectedWallet(
    { ...evidence, funder: '0x5555555555555555555555555555555555555555' },
    'proxy',
    funder,
    3,
  )
  expect(otherFunder.status).toBe('candidate')
  expect(otherFunder.reason).toMatch(/候选地址与当前 Funder 不一致/)

  const otherType = assessDetectedWallet(evidence, 'proxy', funder, 1)
  expect(otherType.status).toBe('candidate')
  expect(otherType.reason).toMatch(/钱包类型与当前签名类型不一致/)
})

it('an undeployed wallet is never accepted', () => {
  const result = assessDetectedWallet({ ...evidence, deployed: false }, 'proxy', funder, 3)
  expect(result.status).toBe('candidate')
  expect(result.canSubmit).toBe(false)
  expect(result.reason).toMatch(/尚未在链上部署/)
})

it('only explicit complete evidence verifies a proxy wallet', () => {
  const result = assessDetectedWallet(evidence, 'proxy', funder, 3)
  expect(result.status).toBe('verified')
  expect(result.canSubmit).toBe(true)
})
