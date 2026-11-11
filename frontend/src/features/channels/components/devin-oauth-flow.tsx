import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { devinOAuthExchange, devinOAuthStart } from '../data/devin';
import { useOAuthFlow, type ProxyConfig } from '../hooks/use-oauth-flow';

// Open synchronously from the click event to avoid popup blocking after the
// asynchronous PKCE session request. The link remains available as a fallback.
async function startDevinLogin(headers?: Record<string, string>) {
  const popup = window.open('about:blank', '_blank');
  if (popup) popup.opener = null;
  try {
    const session = await devinOAuthStart(headers);
    if (popup && !popup.closed) popup.location.replace(session.auth_url);
    return session;
  } catch (error) {
    popup?.close();
    throw error;
  }
}

export function DevinOAuthFlow({ proxyConfig, onSuccess }: { proxyConfig?: ProxyConfig; onSuccess: (token: string) => void }) {
  const { t } = useTranslation();
  const [authorized, setAuthorized] = useState(false);
  const oauth = useOAuthFlow({
    startFn: startDevinLogin,
    exchangeFn: (input, headers) => devinOAuthExchange({ ...input, ...(proxyConfig ? { proxy: proxyConfig } : {}) }, headers),
    proxyConfig,
    onSuccess: (token) => {
      onSuccess(token);
      setAuthorized(true);
    },
  });

  return (
    <div className='space-y-3 rounded-md border p-3' data-testid='devin-oauth-flow'>
      <p className='text-muted-foreground text-xs'>{t('channels.dialogs.devin.oauth.description')}</p>
      <div className='flex flex-wrap items-center gap-2'>
        <Button
          type='button'
          variant='secondary'
          disabled={oauth.isStarting || oauth.isExchanging}
          onClick={() => {
            setAuthorized(false);
            void oauth.start();
          }}
        >
          {t(oauth.isStarting ? 'channels.dialogs.oauth.buttons.starting' : 'channels.dialogs.devin.oauth.login')}
        </Button>
        {oauth.authUrl && (
          <Button type='button' variant='ghost' asChild>
            <a href={oauth.authUrl} target='_blank' rel='noopener noreferrer'>
              {t('channels.dialogs.oauth.buttons.openOAuthLink')}
            </a>
          </Button>
        )}
      </div>
      {oauth.authUrl && (
        <div className='space-y-2'>
          <Label htmlFor='devin-authorization-code'>{t('channels.dialogs.devin.oauth.codeLabel')}</Label>
          <Input
            id='devin-authorization-code'
            value={oauth.callbackUrl}
            onChange={(event) => oauth.setCallbackUrl(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === 'Enter') {
                event.preventDefault();
                if (oauth.sessionId && oauth.callbackUrl.trim() && !oauth.isExchanging) void oauth.exchange();
              }
            }}
            placeholder={t('channels.dialogs.devin.oauth.codePlaceholder')}
            autoComplete='off'
            spellCheck={false}
            className='font-mono text-xs'
          />
          <Button
            type='button'
            disabled={oauth.isStarting || oauth.isExchanging || !oauth.sessionId || !oauth.callbackUrl.trim()}
            onClick={() => void oauth.exchange()}
          >
            {t(oauth.isExchanging ? 'channels.dialogs.oauth.buttons.exchanging' : 'channels.dialogs.devin.oauth.authorize')}
          </Button>
        </div>
      )}
      {authorized && (
        <p role='status' className='text-sm text-green-600 dark:text-green-400'>
          {t('channels.dialogs.devin.oauth.success')}
        </p>
      )}
      <p className='text-muted-foreground text-xs'>{t('channels.dialogs.proxy.oauthHint')}</p>
    </div>
  );
}
