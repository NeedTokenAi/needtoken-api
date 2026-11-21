/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { createFileRoute, useNavigate, useSearch } from '@tanstack/react-router'
import i18next from 'i18next'
import { useEffect, useRef } from 'react'

import { exchangeNeedTokenSSO, wechatLoginByCode } from '@/features/auth/api'
import {
  getSavedLanguage,
  sanitizeAuthRedirect,
} from '@/features/auth/lib/auth-redirect'
import { isLoginChallenge } from '@/features/auth/secure-verification/api'
import { applyAuthBundle, isAuthBundle } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { AuthOperationError } from '@/lib/secure-verification'
import { createServerError } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

function extractAndScrubNeedTokenTicket(): {
  present: boolean
  ticket: string
} {
  if (typeof window === 'undefined') {
    return { present: false, ticket: '' }
  }
  const rawHash = window.location.hash
  if (!rawHash || !rawHash.includes('needtoken_ticket')) {
    return { present: false, ticket: '' }
  }
  const params = new URLSearchParams(
    rawHash.startsWith('#') ? rawHash.slice(1) : rawHash
  )
  if (!params.has('needtoken_ticket')) {
    return { present: false, ticket: '' }
  }
  const ticket = (params.get('needtoken_ticket') ?? '').trim()
  window.history.replaceState(
    null,
    '',
    window.location.pathname + window.location.search
  )
  return { present: true, ticket }
}

export function OAuthComponent() {
  const navigate = useNavigate()
  const handledRef = useRef(false)
  const search = useSearch({ from: '/(auth)/oauth' }) as {
    redirect?: string
    provider?: 'github' | 'discord' | 'oidc' | 'linuxdo' | 'telegram' | 'wechat'
    code?: string
    state?: string
  }

  useEffect(() => {
    if (handledRef.current) return
    handledRef.current = true
    ;(async () => {
      try {
        const sso = extractAndScrubNeedTokenTicket()
        if (sso.present) {
          if (!sso.ticket) {
            throw new AuthOperationError(i18next.t('OAuth failed'))
          }
          const res = await exchangeNeedTokenSSO(sso.ticket)
          if (res?.success && isAuthBundle(res.data)) {
            applyAuthBundle(res.data)
            const savedLang = getSavedLanguage(res.data.user)
            if (savedLang && savedLang !== i18next.language) {
              await i18next.changeLanguage(savedLang)
            }
            const target =
              sanitizeAuthRedirect(search?.redirect, window.location.origin) ??
              '/dashboard'
            await navigate({ href: target, replace: true })
            return
          }
          if (res?.success && isLoginChallenge(res.data)) {
            if (res.data.expires_at * 1000 <= Date.now()) {
              throw new AuthOperationError(
                i18next.t('Login flow expired. Please sign in again.')
              )
            }
            useAuthStore.getState().auth.setPendingLoginVerification({
              challenge: res.data,
              redirectTo:
                sanitizeAuthRedirect(
                  search?.redirect,
                  window.location.origin
                ) ?? undefined,
            })
            await navigate({ to: '/otp', replace: true })
            return
          }
          throw createServerError(res, i18next.t('OAuth failed'))
        }

        if (search?.provider === 'wechat' && search.code) {
          const res = await wechatLoginByCode(search.code)
          if (res?.success && isAuthBundle(res.data)) {
            applyAuthBundle(res.data)
            const target =
              sanitizeAuthRedirect(search?.redirect, window.location.origin) ??
              '/dashboard'
            await navigate({ href: target, replace: true })
            return
          }
          throw createServerError(res, i18next.t('OAuth failed'))
        }
        handleServerError(new AuthOperationError(i18next.t('OAuth failed')))
      } catch (error: unknown) {
        handleServerError(
          AuthOperationError.from(error, i18next.t('OAuth failed'))
        )
      }
      await navigate({ to: '/sign-in', replace: true })
    })()
  }, [navigate, search])

  return null
}

export const Route = createFileRoute('/(auth)/oauth')({
  component: OAuthComponent,
})
