import { apiRequest } from '@/lib/api-client';
import type { OAuthExchangeInput, OAuthExchangeResult, OAuthStartResult } from '../hooks/use-oauth-flow';

export function devinOAuthStart(headers?: Record<string, string>): Promise<OAuthStartResult> {
  return apiRequest('/admin/devin/oauth/start', {
    method: 'POST',
    body: {},
    headers,
    requireAuth: true,
  });
}

export function devinOAuthExchange(input: OAuthExchangeInput, headers?: Record<string, string>): Promise<OAuthExchangeResult> {
  return apiRequest('/admin/devin/oauth/exchange', {
    method: 'POST',
    body: input,
    headers,
    requireAuth: true,
  });
}
