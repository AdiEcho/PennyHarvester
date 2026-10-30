import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { CircleDollarSign, KeyRound, LoaderCircle, ShieldCheck } from 'lucide-react'
import { api } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

type GateState = 'loading' | 'locked' | 'open'

// AuthGate keeps the workbench behind a password when one has been set in the
// system settings page. Without a password the API stays open, which preserves
// the original localhost-only behaviour of existing installs.
export function AuthGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<GateState>('loading')

  const check = useCallback(async () => {
    try {
      const status = await api.authStatus()
      setState(status.required && !status.authenticated ? 'locked' : 'open')
    } catch {
      // The backend may be unreachable; let the app shell surface that error
      // instead of trapping the user behind a login form that cannot work.
      setState('open')
    }
  }, [])

  useEffect(() => {
    void check()
  }, [check])

  useEffect(() => {
    const handler = () => void check()
    window.addEventListener('penny:unauthorized', handler)
    return () => window.removeEventListener('penny:unauthorized', handler)
  }, [check])

  if (state === 'loading') {
    return (
      <div className="grid min-h-dvh place-items-center bg-canvas">
        <div className="flex items-center gap-3 text-sm text-slate-500" role="status">
          <LoaderCircle className="h-5 w-5 animate-spin" /> 正在检查访问状态…
        </div>
      </div>
    )
  }
  if (state === 'locked') {
    return <LoginScreen onSuccess={() => setState('open')} />
  }
  return <>{children}</>
}

function LoginScreen({ onSuccess }: { onSuccess: () => void }) {
  const [password, setPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!password) {
      setError('请输入访问密码。')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      await api.login(password)
      setPassword('')
      onSuccess()
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : '登录失败，请稍后重试。')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="grid min-h-dvh place-items-center bg-navy px-4 py-10">
      <div className="w-full max-w-sm">
        <div className="flex items-center gap-3">
          <div className="relative flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-brand-lime text-navy">
            <CircleDollarSign className="h-6 w-6" strokeWidth={1.8} />
          </div>
          <div>
            <div className="text-[15px] font-semibold tracking-[-0.02em] text-white">PennyHarvester</div>
            <div className="mt-0.5 text-[11px] tracking-wide text-slate-400">Polymarket 尾盘策略工作台</div>
          </div>
        </div>

        <form onSubmit={(event) => void submit(event)} className="mt-6 rounded-2xl border border-white/10 bg-white/[0.04] p-6 backdrop-blur">
          <div className="flex items-center gap-2 text-brand-lime">
            <KeyRound className="h-4 w-4" />
            <span className="text-xs font-semibold uppercase tracking-[0.14em]">访问受限</span>
          </div>
          <h1 className="mt-3 text-lg font-semibold text-white">输入访问密码</h1>
          <p className="mt-1.5 text-xs leading-5 text-slate-400">此实例已在系统设置中启用访问密码。登录状态将在此浏览器保留 7 天。</p>
          <label htmlFor="access-password" className="mt-5 block text-sm font-medium text-slate-300">访问密码</label>
          <Input
            id="access-password"
            type="password"
            value={password}
            autoFocus
            autoComplete="current-password"
            onChange={(event) => { setPassword(event.target.value); setError('') }}
            className="mt-2 border-white/10 bg-white/[0.06] text-white placeholder:text-slate-500"
            aria-invalid={Boolean(error)}
            aria-describedby={error ? 'access-password-error' : undefined}
          />
          {error && <p id="access-password-error" className="mt-2 text-xs leading-5 text-rose-300" role="alert">{error}</p>}
          <Button type="submit" variant="lime" className="mt-4 w-full" loading={submitting}>
            {submitting ? '正在验证…' : '进入工作台'}
          </Button>
          <p className="mt-4 flex items-start gap-1.5 text-[11px] leading-5 text-slate-500">
            <ShieldCheck className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            密码以 bcrypt 哈希形式保存在本机数据库中，连续输错会被临时限制。
          </p>
        </form>
      </div>
    </div>
  )
}
