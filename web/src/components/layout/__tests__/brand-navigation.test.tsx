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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it } from 'vitest'

import { STATUS_QUERY_KEY } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

import { PublicHeader } from '../components/public-header'
import { SystemBrand } from '../components/system-brand'

function renderWithRouter(element: ReactNode, status: Record<string, unknown>) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(STATUS_QUERY_KEY, status)

  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        {element}
        <Outlet />
      </QueryClientProvider>
    ),
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const dashboardRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/dashboard',
    component: () => null,
  })
  const signInRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/sign-in',
    component: () => null,
  })
  const pricingRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/pricing',
    component: () => null,
  })
  const rankingsRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/rankings',
    component: () => null,
  })

  const router = createRouter({
    routeTree: rootRoute.addChildren([
      indexRoute,
      dashboardRoute,
      signInRoute,
      pricingRoute,
      rankingsRoute,
    ]),
    history: createMemoryHistory({ initialEntries: ['/pricing'] }),
  })

  return render(<RouterProvider router={router} />)
}

afterEach(() => {
  cleanup()
  window.localStorage.clear()
  useAuthStore.getState().auth.reset()
  useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
})

describe('brand logo navigation target', () => {
  it('routes inline SystemBrand to /dashboard when home is disabled and user is signed in', async () => {
    useAuthStore.getState().auth.setUser({
      id: 7,
      username: 'member',
      role: 1,
    })

    renderWithRouter(<SystemBrand variant='inline' />, {
      system_name: 'NeedToken API',
      HeaderNavModules: JSON.stringify({ home: false }),
    })

    await waitFor(() => {
      const link = screen.getByRole('link', { name: 'Go to Dashboard' })
      expect(link).toHaveAttribute('href', '/dashboard')
    })
  })

  it('routes inline SystemBrand to /sign-in when home is disabled and user is anonymous', async () => {
    renderWithRouter(<SystemBrand variant='inline' />, {
      system_name: 'NeedToken API',
      HeaderNavModules: JSON.stringify({ home: false }),
    })

    await waitFor(() => {
      const link = screen.getByRole('link', { name: 'Sign in' })
      expect(link).toHaveAttribute('href', '/sign-in')
    })
  })

  it('keeps inline SystemBrand pointing to / when home module is enabled', async () => {
    renderWithRouter(<SystemBrand variant='inline' />, {
      system_name: 'New API',
      HeaderNavModules: JSON.stringify({ home: true }),
    })

    await waitFor(() => {
      const link = screen.getByRole('link', { name: 'Go to home' })
      expect(link).toHaveAttribute('href', '/')
    })
  })

  it('routes PublicHeader logo to /dashboard or /sign-in when home is disabled and keeps / when enabled', async () => {
    useSystemConfigStore.getState().setConfig({ systemName: 'NeedToken API' })
    useSystemConfigStore.getState().setLoading(false)

    renderWithRouter(
      <PublicHeader
        showThemeSwitch={false}
        showLanguageSwitcher={false}
        showNotifications={false}
        showAuthButtons={false}
      />,
      {
        system_name: 'NeedToken API',
        HeaderNavModules: JSON.stringify({ home: false }),
      }
    )

    await waitFor(() => {
      const link = screen.getByRole('link', { name: /NeedToken API/ })
      expect(link).toHaveAttribute('href', '/sign-in')
    })

    cleanup()
    useAuthStore.getState().auth.setUser({
      id: 8,
      username: 'member',
      role: 1,
    })

    renderWithRouter(
      <PublicHeader
        showThemeSwitch={false}
        showLanguageSwitcher={false}
        showNotifications={false}
        showAuthButtons={false}
      />,
      {
        system_name: 'NeedToken API',
        HeaderNavModules: JSON.stringify({ home: false }),
      }
    )

    await waitFor(() => {
      const link = screen.getByRole('link', { name: /NeedToken API/ })
      expect(link).toHaveAttribute('href', '/dashboard')
    })

    cleanup()
    renderWithRouter(
      <PublicHeader
        showThemeSwitch={false}
        showLanguageSwitcher={false}
        showNotifications={false}
        showAuthButtons={false}
      />,
      {
        system_name: 'NeedToken API',
        HeaderNavModules: JSON.stringify({ home: true }),
      }
    )

    await waitFor(() => {
      const link = screen.getByRole('link', { name: /NeedToken API/ })
      expect(link).toHaveAttribute('href', '/')
    })
  })
})
