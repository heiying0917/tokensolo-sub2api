import type { Account } from '@/types'

/** 判定是否 SIWC（Sign in with ChatGPT）账号：platform=openai, type=oauth, credentials.auth_mode="siwc"。 */
export function isSiwcAccount(
  account: Pick<Account, 'platform' | 'type' | 'credentials'> | null | undefined
): boolean {
  if (!account) return false
  const authMode = account.credentials?.auth_mode
  return (
    account.platform === 'openai' &&
    account.type === 'oauth' &&
    typeof authMode === 'string' &&
    authMode.trim().toLowerCase() === 'siwc'
  )
}
