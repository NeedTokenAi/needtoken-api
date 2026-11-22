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
import { QueryClient } from '@tanstack/react-query'
import { isRedirect } from '@tanstack/react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import * as authSession from '@/lib/auth-session'
import { STATUS_QUERY_KEY, STATUS_STORAGE_KEY } from '@/lib/status-query'
import { Route as IndexRoute } from '@/routes/index'
import { useAuthStore } from '@/stores/auth-store'

type ApiMethod = (url: string) => Promise<{ data: unknown }>
const apiClient = api as unknown as { get: ApiMethod }
const originalGet = apiClient.get

async function runHomeBeforeLoad(queryClient: QueryClient): Promise<unknown> {
  const beforeLoad = IndexRoute.options.beforeLoad
  if (!beforeLoad) {
    throw new Error('Index route must define beforeLoad')
  }
  try {
    await beforeLoad({
      context: { queryClient },
    } as never)
    return null
  } catch (thrown) {
    return thrown
  }
}

afterEach(() => {
  apiClient.get = originalGet
  vi.restoreAllMocks()
  window.localStorage.clear()
  useAuthStore.getState().auth.reset()
})

describe('root index route beforeLoad home guard', () => {
  it('redirects authenticated users to /dashboard when HeaderNavModules.home is false', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    queryClient.setQueryData(STATUS_QUERY_KEY, {
      HeaderNavModules: JSON.stringify({ home: false }),
    })

    const futureExp = Math.floor(Date.now() / 1000) + 3600
    useAuthStore.getState().auth.setBundle({
      access_token: 'valid-token',
      token_type: 'Bearer',
      access_expires_at: futureExp,
      session: {
        sid: 'sid-1',
        current: true,
        login_method: 'password',
        ip: '127.0.0.1',
        user_agent: 'vitest',
        created_at: futureExp - 3600,
        last_active_at: futureExp - 3600,
        expires_at: futureExp + 3600,
      },
      user: { id: 1, username: 'alice', role: 1 },
    })

    const thrown = await runHomeBeforeLoad(queryClient)
    expect(isRedirect(thrown)).toBe(true)
    expect((thrown as { options?: { to?: string } }).options?.to).toBe(
      '/dashboard'
    )
  })

  it('redirects anonymous users to /sign-in when HeaderNavModules.home is false', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    queryClient.setQueryData(STATUS_QUERY_KEY, {
      HeaderNavModules: JSON.stringify({ home: false }),
    })
    const resolveSpy = vi
      .spyOn(authSession, 'resolveAuthentication')
      .mockResolvedValue({ kind: 'anonymous' })

    const thrown = await runHomeBeforeLoad(queryClient)
    expect(resolveSpy).toHaveBeenCalledTimes(1)
    expect(isRedirect(thrown)).toBe(true)
    expect((thrown as { options?: { to?: string } }).options?.to).toBe(
      '/sign-in'
    )
  })

  it('keeps default home page untouched when HeaderNavModules.home is not false', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    queryClient.setQueryData(STATUS_QUERY_KEY, {
      HeaderNavModules: JSON.stringify({ home: true }),
    })
    const resolveSpy = vi.spyOn(authSession, 'resolveAuthentication')

    const thrown = await runHomeBeforeLoad(queryClient)
    expect(thrown).toBeNull()
    expect(resolveSpy).not.toHaveBeenCalled()
  })

  it('falls back to cached status snapshot when /api/status fails', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    window.localStorage.setItem(
      STATUS_STORAGE_KEY,
      JSON.stringify({ HeaderNavModules: JSON.stringify({ home: false }) })
    )
    apiClient.get = async () => {
      throw new Error('network error')
    }
    vi.spyOn(authSession, 'resolveAuthentication').mockResolvedValue({
      kind: 'anonymous',
    })

    const thrown = await runHomeBeforeLoad(queryClient)
    expect(isRedirect(thrown)).toBe(true)
    expect((thrown as { options?: { to?: string } }).options?.to).toBe(
      '/sign-in'
    )
  })
})
