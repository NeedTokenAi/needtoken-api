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
import { createFileRoute, redirect } from '@tanstack/react-router'

import { Home } from '@/features/home'
import { resolveAuthentication } from '@/lib/auth-session'
import { parseHeaderNavModulesFromStatus } from '@/lib/nav-modules'
import { readCachedStatus, statusQueryOptions } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'

export const Route = createFileRoute('/')({
  beforeLoad: async ({ context }) => {
    let status: Record<string, unknown> | null = null
    try {
      status = await context.queryClient.fetchQuery(statusQueryOptions)
    } catch {
      status = readCachedStatus()
    }
    const modules = parseHeaderNavModulesFromStatus(status)
    if (modules.home === false) {
      await resolveAuthentication()
      const { auth } = useAuthStore.getState()
      if (auth.user && auth.accessToken) {
        throw redirect({ to: '/dashboard', replace: true })
      }
      throw redirect({ to: '/sign-in', replace: true })
    }
  },
  component: Home,
})
