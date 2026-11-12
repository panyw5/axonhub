import { useState, useCallback, useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { ProxyType } from '../components/channels-proxy-dialog';

export interface ProxyConfig {
  type: ProxyType;
  url?: string;
  username?: string;
  password?: string;
}

export interface OAuthStartResult {
  session_id: string;
  auth_url: string;
}

export interface OAuthExchangeInput {
  session_id: string;
  callback_url: string;
  proxy?: ProxyConfig;
}

export interface OAuthExchangeResult {
  credentials: string;
}

export interface OAuthFlowOptions {
  /**
   * Function to call the OAuth start endpoint
   */
  startFn: (headers?: Record<string, string>) => Promise<OAuthStartResult>;

  /**
   * Function to call the OAuth exchange endpoint
   */
  exchangeFn: (input: OAuthExchangeInput, headers?: Record<string, string>) => Promise<OAuthExchangeResult>;

  /**
   * Optional proxy configuration for exchange token request
   */
  proxyConfig?: ProxyConfig;

  /**
   * Callback when credentials are successfully obtained
   */
  onSuccess?: (credentials: string) => void;
}

export interface OAuthFlowState {
  sessionId: string | null;
  authUrl: string | null;
  callbackUrl: string;
  isStarting: boolean;
  isExchanging: boolean;
}

export interface OAuthFlowActions {
  start: () => Promise<void>;
  exchange: () => Promise<void>;
  setCallbackUrl: (url: string) => void;
  reset: () => void;
}

/**
 * A reusable hook for managing OAuth flows (e.g., Codex, Claude Code).
 * This eliminates code duplication for different OAuth providers.
 *
 * @example
 * ```tsx
 * const codexOAuth = useOAuthFlow({
 *   startFn: codexOAuthStart,
 *   exchangeFn: codexOAuthExchange,
 *   onSuccess: (credentials) => form.setValue('credentials.apiKey', credentials),
 * });
 *
 * // Later in your component:
 * <Button onClick={codexOAuth.start} disabled={codexOAuth.isStarting}>
 *   {codexOAuth.isStarting ? 'Starting...' : 'Start OAuth'}
 * </Button>
 * ```
 */
export function useOAuthFlow(options: OAuthFlowOptions): OAuthFlowState & OAuthFlowActions {
  const { startFn, exchangeFn, proxyConfig, onSuccess } = options;
  const { t } = useTranslation();
  const generation = useRef(0);
  useEffect(
    () => () => {
      generation.current += 1;
    },
    []
  );

  const [sessionId, setSessionId] = useState<string | null>(null);
  const [authUrl, setAuthUrl] = useState<string | null>(null);
  const [callbackUrl, setCallbackUrl] = useState('');
  const [isStarting, setIsStarting] = useState(false);
  const [isExchanging, setIsExchanging] = useState(false);

  const start = useCallback(async () => {
    const current = ++generation.current;
    setSessionId(null);
    setAuthUrl(null);
    setCallbackUrl('');
    setIsExchanging(false);
    setIsStarting(true);
    try {
      const result = await startFn();
      if (current !== generation.current) return;
      setSessionId(result.session_id);
      setAuthUrl(result.auth_url);
    } catch (error) {
      if (current === generation.current) toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      if (current === generation.current) setIsStarting(false);
    }
  }, [startFn]);

  const exchange = useCallback(async () => {
    const current = generation.current;
    if (!sessionId) {
      toast.error(t('channels.dialogs.oauth.errors.sessionMissing'));
      return;
    }

    if (!callbackUrl.trim()) {
      toast.error(t('channels.dialogs.oauth.errors.callbackUrlRequired'));
      return;
    }

    setIsExchanging(true);
    try {
      const exchangeInput: OAuthExchangeInput = {
        session_id: sessionId,
        callback_url: callbackUrl.trim(),
      };

      // Add proxy config if provided and type is not disabled/environment
      if (proxyConfig && proxyConfig.type === ProxyType.URL) {
        exchangeInput.proxy = {
          type: proxyConfig.type,
          url: proxyConfig.url,
          ...(proxyConfig.username && { username: proxyConfig.username }),
          ...(proxyConfig.password && { password: proxyConfig.password }),
        };
      }

      const result = await exchangeFn(exchangeInput);
      if (current !== generation.current) return;

      if (onSuccess) {
        onSuccess(result.credentials);
      }
      setCallbackUrl('');
      setSessionId(null);
      setAuthUrl(null);

      toast.success(t('channels.dialogs.oauth.messages.credentialsImported'));
    } catch (error) {
      if (current === generation.current) toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      if (current === generation.current) setIsExchanging(false);
    }
  }, [sessionId, callbackUrl, exchangeFn, onSuccess, t, proxyConfig]);

  const reset = useCallback(() => {
    generation.current += 1;
    setSessionId(null);
    setAuthUrl(null);
    setCallbackUrl('');
    setIsStarting(false);
    setIsExchanging(false);
  }, []);

  return {
    sessionId,
    authUrl,
    callbackUrl,
    isStarting,
    isExchanging,
    start,
    exchange,
    setCallbackUrl,
    reset,
  };
}
