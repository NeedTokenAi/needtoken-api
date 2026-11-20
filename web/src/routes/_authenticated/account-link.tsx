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
import { useMutation, useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { Link2, ShieldCheck } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { ErrorState } from '@/components/error-state'
import { Main } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

const accountLinkSearchSchema = z.object({
  challenge: z.string().optional(),
})

interface LinkChallengePreview {
  challenge_id: string
  source: string
  member_label: string
  return_origin: string
  expires_at: number
}

interface LinkApprovalResult {
  challenge_id: string
  approval: string
  return_url: string
  expires_at: number
}

export const Route = createFileRoute('/_authenticated/account-link')({
  validateSearch: (search) => accountLinkSearchSchema.parse(search),
  component: AccountLinkPage,
})

async function previewAccountLink(
  challenge: string
): Promise<LinkChallengePreview> {
  const res = await api.post('/api/user/account-link/preview', { challenge })
  return requireServerSuccess(res.data).data as LinkChallengePreview
}

async function approveAccountLink(
  challenge: string
): Promise<LinkApprovalResult> {
  const res = await api.post('/api/user/account-link/approve', { challenge })
  return requireServerSuccess(res.data).data as LinkApprovalResult
}

function AccountLinkPage() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const search = Route.useSearch()
  const user = useAuthStore((s) => s.auth.user)
  const [challengeToken] = useState<string>(() => search.challenge?.trim() ?? '')

  // Strip the signed challenge token from the address bar immediately so it is
  // not retained in browser history or leaked through Referer headers.
  useEffect(() => {
    if (search.challenge && typeof window !== 'undefined') {
      const cleanURL = new URL(window.location.href)
      cleanURL.searchParams.delete('challenge')
      window.history.replaceState(window.history.state, '', cleanURL.toString())
    }
  }, [search.challenge])

  const previewQuery = useQuery({
    queryKey: ['account-link', 'preview', challengeToken],
    queryFn: () => previewAccountLink(challengeToken),
    enabled: challengeToken.length > 0,
    retry: false,
    meta: { errorToast: false },
  })

  const approveMutation = useMutation({
    mutationFn: () => approveAccountLink(challengeToken),
    onSuccess: (result) => {
      // Deliver approval via URL fragment so the short-lived assertion never
      // appears in HTTP request logs or Referer headers.
      const fragment = new URLSearchParams({
        challenge_id: result.challenge_id,
        approval: result.approval,
      }).toString()
      window.location.assign(`${result.return_url}#${fragment}`)
    },
    onError: (err) => {
      handleServerError(err, t('Failed to confirm account linking'))
    },
  })

  const handleCancel = () => {
    void navigate({ to: '/dashboard' })
  }

  if (!challengeToken) {
    return (
      <Main className='flex items-center justify-center'>
        <ErrorState
          title={t('Invalid account link request')}
          description={t(
            'Start account linking from the storefront so both accounts can be securely verified.'
          )}
          action={
            <Button variant='outline' onClick={handleCancel}>
              {t('Back to dashboard')}
            </Button>
          }
        />
      </Main>
    )
  }

  if (previewQuery.isPending) {
    return (
      <Main className='flex items-center justify-center'>
        <LoadingState message={t('Verifying link request...')} />
      </Main>
    )
  }

  if (previewQuery.isError || !previewQuery.data) {
    return (
      <Main className='flex items-center justify-center'>
        <ErrorState
          title={t('Link request expired or invalid')}
          description={t(
            'Please return to the storefront and start a fresh account linking request.'
          )}
          action={
            <Button variant='outline' onClick={handleCancel}>
              {t('Back to dashboard')}
            </Button>
          }
        />
      </Main>
    )
  }

  const preview = previewQuery.data

  return (
    <Main className='flex items-center justify-center py-8'>
      <Card className='w-full max-w-lg'>
        <CardHeader>
          <div className='flex items-center gap-2'>
            <Link2 className='text-primary size-5' />
            <CardTitle>{t('Confirm storefront account link')}</CardTitle>
          </div>
          <CardDescription>
            {t(
              'Link this API console account with your storefront account so purchased API plans activate automatically.'
            )}
          </CardDescription>
        </CardHeader>

        <CardContent className='space-y-4'>
          <div className='bg-muted/40 space-y-2 rounded-lg border p-3 text-sm'>
            <div className='flex items-center justify-between gap-2'>
              <span className='text-muted-foreground'>
                {t('Storefront account')}
              </span>
              <Badge variant='secondary'>{preview.member_label}</Badge>
            </div>
            <div className='flex items-center justify-between gap-2'>
              <span className='text-muted-foreground'>
                {t('Storefront origin')}
              </span>
              <span className='font-mono text-xs'>{preview.return_origin}</span>
            </div>
            <div className='flex items-center justify-between gap-2'>
              <span className='text-muted-foreground'>
                {t('Current API account')}
              </span>
              <span className='font-medium'>
                {user?.username ?? '-'} (#{user?.id ?? '-'})
              </span>
            </div>
          </div>

          <Alert>
            <ShieldCheck className='size-4' />
            <AlertDescription>
              {t(
                'Your existing wallet balance, subscriptions, and API keys will be preserved. You will be redirected back to the storefront to finish verification.'
              )}
            </AlertDescription>
          </Alert>
        </CardContent>

        <CardFooter className='flex justify-end gap-2 border-t py-4'>
          <Button
            type='button'
            variant='outline'
            onClick={handleCancel}
            disabled={approveMutation.isPending}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            onClick={() => approveMutation.mutate()}
            disabled={approveMutation.isPending}
          >
            {approveMutation.isPending
              ? t('Confirming...')
              : t('Confirm and return to storefront')}
          </Button>
        </CardFooter>
      </Card>
    </Main>
  )
}
