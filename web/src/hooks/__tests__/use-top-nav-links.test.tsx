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
import { cleanup, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it } from 'vitest'

import enLocale from '@/i18n/locales/en.json'
import frLocale from '@/i18n/locales/fr.json'
import jaLocale from '@/i18n/locales/ja.json'
import ruLocale from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTWLocale from '@/i18n/locales/zh-TW.json'
import zhLocale from '@/i18n/locales/zh.json'
import { STATUS_QUERY_KEY } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'

import { useTopNavLinks } from '../use-top-nav-links'

function renderNavLinks(status: Record<string, unknown>) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(STATUS_QUERY_KEY, status)

  function Wrapper(props: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        {props.children}
      </QueryClientProvider>
    )
  }

  return renderHook(() => useTopNavLinks(), { wrapper: Wrapper })
}

afterEach(() => {
  cleanup()
  window.localStorage.clear()
  useAuthStore.getState().auth.reset()
})

describe('useTopNavLinks', () => {
  it('renders only Model Square, Rankings, and external API Docs when home/console/about are disabled', () => {
    const { result } = renderNavLinks({
      HeaderNavModules: JSON.stringify({
        home: false,
        console: false,
        about: false,
        pricing: { enabled: true, requireAuth: false },
        rankings: { enabled: true, requireAuth: false },
        docs: true,
      }),
      docs_link: '  https://needtoken.ai/docs  ',
    })

    expect(result.current).toEqual([
      { title: 'Model Square', href: '/pricing', requiresAuth: false },
      { title: 'Rankings', href: '/rankings', requiresAuth: false },
      {
        title: 'API Docs',
        href: 'https://needtoken.ai/docs',
        external: true,
      },
    ])
  })

  it('keeps default upstream modules and routes internal docs to /docs when docs_link is absent', () => {
    const { result } = renderNavLinks({})

    expect(result.current).toEqual([
      { title: 'Home', href: '/' },
      { title: 'Console', href: '/dashboard' },
      { title: 'Model Square', href: '/pricing', requiresAuth: false },
      { title: 'Rankings', href: '/rankings', requiresAuth: false },
      { title: 'API Docs', href: '/docs' },
      { title: 'About', href: '/about' },
    ])
  })

  it('provides API Docs translations across all seven locales', () => {
    const translations = [
      enLocale.translation['API Docs'],
      zhLocale.translation['API Docs'],
      zhTWLocale.translation['API Docs'],
      frLocale.translation['API Docs'],
      jaLocale.translation['API Docs'],
      ruLocale.translation['API Docs'],
      viLocale.translation['API Docs'],
    ]

    expect(translations).toEqual([
      'API Docs',
      'API 文档',
      'API 文件',
      'Docs API',
      'API ドキュメント',
      'Документация API',
      'Tài liệu API',
    ])
  })
})
