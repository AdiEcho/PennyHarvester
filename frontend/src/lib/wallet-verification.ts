import type { FunderDiscoveryResult } from '@/types'

export type WalletVerificationStatus = 'not_required' | 'verified' | 'candidate' | 'unverified'

export interface WalletVerification {
  status: WalletVerificationStatus
  label: string
  /** Only an explicitly verified proxy/deposit wallet may be saved as live. */
  canSubmit: boolean
  reason: string
}

const ADDRESS = /^0x[0-9a-fA-F]{40}$/

function address(value: string | undefined): string | null {
  return value && ADDRESS.test(value) ? value.toLowerCase() : null
}

// The backend currently returns bytecode-derived wallet types for discovery.
// A type alone is not proof: it does not include the chain, implementation,
// decoded owner or a signer/owner match. Treat that output as a candidate and
// only accept it when the backend explicitly returns all of the evidence below.
export function assessDetectedWallet(
  result: FunderDiscoveryResult | null,
  mode: 'eoa' | 'proxy',
  expectedFunder: string,
  expectedSignatureType: number,
): WalletVerification {
  if (mode === 'eoa') {
    if (expectedFunder.trim()) {
      return {
        status: 'unverified',
        label: '未验证，请核对',
        canSubmit: false,
        reason: '自托管 EOA 模式不应填写 Funder；请清除 Funder 或改用代理钱包模式。',
      }
    }
    return {
      status: 'not_required',
      label: '自托管 EOA',
      canSubmit: true,
      reason: '资金与签名都在私钥地址上，不需要验证代理钱包归属。',
    }
  }

  if (!result?.ok || !result.funder) {
    return {
      status: 'unverified',
      label: '未验证，请核对',
      canSubmit: false,
      reason: '尚未完成钱包识别，无法核对链、实现合约、owner 与 signer。',
    }
  }

  const missing: string[] = []
  const funder = address(result.funder)
  const signer = address(result.signer)
  const owner = address(result.owner)
  const expected = address(expectedFunder)

  if (result.verified !== true) missing.push('后端未明确返回 verified=true')
  if (result.chain_id !== 137) missing.push('链 ID 不是 Polygon 137')
  if (!result.evidence) missing.push('未返回工厂推导或 owner/signer 验证证据')
  if (!owner) missing.push('未返回解码后的 owner')
  if (!signer) missing.push('未返回 signer')
  if (owner && signer && owner !== signer) missing.push('owner 与 signer 不一致')
  if (result.deployed === false) missing.push('钱包尚未在链上部署')
  if (!funder || !expected || funder !== expected) missing.push('候选地址与当前 Funder 不一致')
  if (result.wallet_type == null || result.wallet_type !== expectedSignatureType) missing.push('钱包类型与当前签名类型不一致')

  if (missing.length === 0) {
    return {
      status: 'verified',
      label: '已验证',
      canSubmit: true,
      reason: `Polygon 137 · ${result.evidence}${result.implementation ? ` · implementation ${result.implementation}` : ''} · owner/signer 一致。`,
    }
  }

  return {
    status: 'candidate',
    label: '未验证，请核对',
    canSubmit: false,
    reason: `识别结果为候选钱包：${missing.join('；')}。字节码形态本身不能证明归属或签名兼容性。`,
  }
}
