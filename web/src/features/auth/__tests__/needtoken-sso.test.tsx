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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { OAuthComponent } from '@/routes/(auth)/oauth'
import { useAuthStore, type AuthBundle } from '@/stores/auth-store'

const ssoAuthBundle: AuthBundle = {
  access_token: 'sso-access-token',
  token_type: 'Bearer',
  access_expires_at: 9999999999,
  user: { id: 42, username: 'nt_42_abcd', role: 1 },
  session: {
    sid: 'sso-session-1',
    current: true,
    login_method: 'needtoken_sso',
    ip: '',
    user_agent: '',
    created_at: 1,
    last_active_at: 1,
    expires_at: 9999999999,
  },
}

function renderNeedTokenSSORoute(initialUrl: string, windowHref: string) {
  useAuthStore.getState().auth.reset('complete')
  window.history.replaceState(null, '', windowHref)

  const root = createRootRoute({ component: Outlet })
  const authGroup = createRoute({
    getParentRoute: () => root,
    id: '(auth)',
    component: Outlet,
  })
  const oauthRoute = createRoute({
    getParentRoute: () => authGroup,
    path: '/oauth',
    component: OAuthComponent,
  })
  const dashboardRoute = createRoute({
    getParentRoute: () => root,
    path: '/dashboard',
    component: () => <div>Dashboard destination</div>,
  })
  const keysRoute = createRoute({
    getParentRoute: () => root,
    path: '/keys',
    component: () => <div>Keys destination</div>,
  })
  const otpRoute = createRoute({
    getParentRoute: () => root,
    path: '/otp',
    component: () => <div>OTP verification page</div>,
  })
  const signInRoute = createRoute({
    getParentRoute: () => root,
    path: '/sign-in',
    component: () => <div>Sign-in fallback page</div>,
  })

  const router = createRouter({
    routeTree: root.addChildren([
      authGroup.addChildren([oauthRoute]),
      dashboardRoute,
      keysRoute,
      otpRoute,
      signInRoute,
    ]),
    history: createMemoryHistory({
      initialEntries: [initialUrl],
    }),
  })

  return {
    router,
    ...render(<RouterProvider router={router} />),
  }
}

describe('NeedToken SSO hash ticket exchange on /oauth', () => {
  beforeEach(() => {
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
    useAuthStore.getState().auth.reset('complete')
    window.history.replaceState(null, '', '/')
  })

  it('scrubs #needtoken_ticket immediately, applies AuthBundle, and navigates to safe redirect', async () => {
    const postSpy = vi.spyOn(api, 'post').mockResolvedValueOnce({
      data: { success: true, message: '', data: ssoAuthBundle },
    })

    const { router } = renderNeedTokenSSORoute(
      '/oauth?redirect=%2Fkeys',
      '/oauth?redirect=%2Fkeys#needtoken_ticket=v1.payload.sig'
    )

    await waitFor(() => {
      expect(window.location.hash).toBe('')
    })
    await screen.findByText('Keys destination')

    expect(postSpy).toHaveBeenCalledTimes(1)
    expect(postSpy).toHaveBeenCalledWith(
      '/api/user/needtoken-sso',
      { ticket: 'v1.payload.sig' },
      { skipAuthRefresh: true }
    )
    expect(useAuthStore.getState().auth.user?.id).toBe(42)
    expect(router.state.location.pathname).toBe('/keys')
  })

  it('stores pending LoginChallenge and redirects to /otp when 2FA is required', async () => {
    const expiresAt = Math.floor(Date.now() / 1000) + 300
    vi.spyOn(api, 'post').mockResolvedValueOnce({
      data: {
        success: true,
        message: '',
        data: {
          require_verification: true,
          flow_token: 'mfa-flow-token',
          expires_at: expiresAt,
          methods: [{ method: '2fa', available: true }],
        },
      },
    })

    renderNeedTokenSSORoute(
      '/oauth?redirect=https%3A%2F%2Fevil.example%2Fsteal',
      '/oauth?redirect=https%3A%2F%2Fevil.example%2Fsteal#needtoken_ticket=v1.mfa.sig'
    )

    await screen.findByText('OTP verification page')
    expect(window.location.hash).toBe('')
    expect(useAuthStore.getState().auth.user).toBeNull()
    expect(useAuthStore.getState().auth.pendingLoginVerification).toEqual({
      challenge: {
        require_verification: true,
        flow_token: 'mfa-flow-token',
        expires_at: expiresAt,
        methods: [{ method: '2fa', available: true }],
      },
      redirectTo: undefined,
    })
  })

  it('scrubs hash and falls back to /sign-in when ticket exchange fails', async () => {
    vi.spyOn(api, 'post').mockResolvedValueOnce({
      data: {
        success: false,
        message: 'Invalid or expired SSO ticket',
      },
    })

    renderNeedTokenSSORoute('/oauth', '/oauth#needtoken_ticket=v1.replayed.sig')

    await screen.findByText('Sign-in fallback page')
    expect(window.location.hash).toBe('')
    expect(useAuthStore.getState().auth.user).toBeNull()
  })
})
