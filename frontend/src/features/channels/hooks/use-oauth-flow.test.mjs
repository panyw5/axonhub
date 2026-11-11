import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import ts from 'typescript';

// Exercise async lifecycle logic without a DOM; state/ref slots persist across renders.
function harness(options) {
  const source = readFileSync(new URL('./use-oauth-flow.ts', import.meta.url), 'utf8');
  const ast = ts.createSourceFile('hook.ts', source, ts.ScriptTarget.Latest, true);
  const body = ast.statements.filter((node) => !ts.isImportDeclaration(node)).map((node) => node.getText(ast)).join('\n');
  const js = ts.transpileModule(body, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023 } }).outputText;
  let cursor = 0;
  const slots = [];
  const cleanups = [];
  const notifications = [];
  const exports = {};
  vm.runInNewContext(js, {
    exports,
    useState(initial) {
      const index = cursor++;
      slots[index] ??= { value: initial };
      return [slots[index].value, (value) => { slots[index].value = value; }];
    },
    useRef(initial) { return slots[cursor++] ??= { current: initial }; },
    useEffect(effect) { const index = cursor++; if (!slots[index]) { slots[index] = {}; cleanups.push(effect()); } },
    useCallback: (fn) => fn,
    useTranslation: () => ({ t: (key) => key }),
    toast: { error: (value) => notifications.push(['error', value]), success: (value) => notifications.push(['success', value]) },
    ProxyType: { URL: 'url' },
  });
  return {
    render() { cursor = 0; return exports.useOAuthFlow(options); },
    unmount() { for (const cleanup of cleanups) cleanup?.(); },
    notifications,
  };
}

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test('OAuth code exchange fills credentials and clears the consumed session', async () => {
  const successes = [];
  let submitted;
  const hooks = harness({
    startFn: async () => ({ session_id: 'session', auth_url: 'https://example.invalid/auth' }),
    exchangeFn: async (input) => { submitted = input; return { credentials: 'token' }; },
    onSuccess: (value) => successes.push(value),
  });
  await hooks.render().start();
  hooks.render().setCallbackUrl(' code ');
  await hooks.render().exchange();
  assert.equal(submitted.session_id, 'session');
  assert.equal(submitted.callback_url, 'code');
  assert.deepEqual(successes, ['token']);
  assert.equal(hooks.render().sessionId, null);
  assert.equal(hooks.render().callbackUrl, '');
});

test('reset discards a late OAuth start response', async () => {
  const pending = deferred();
  const hooks = harness({ startFn: () => pending.promise });
  const running = hooks.render().start();
  hooks.render().reset();
  pending.resolve({ session_id: 'old', auth_url: 'https://example.invalid/auth' });
  await running;
  assert.equal(hooks.render().sessionId, null);
  assert.equal(hooks.render().authUrl, null);
  assert.equal(hooks.render().isStarting, false);
});

test('unmount discards late credentials instead of filling another channel', async () => {
  const pending = deferred();
  const successes = [];
  const hooks = harness({ startFn: async () => ({ session_id: 'session', auth_url: 'url' }), exchangeFn: () => pending.promise, onSuccess: (token) => successes.push(token) });
  await hooks.render().start();
  hooks.render().setCallbackUrl('code');
  const running = hooks.render().exchange();
  hooks.unmount();
  pending.resolve({ credentials: 'late-token' });
  await running;
  assert.deepEqual(successes, []);
  assert.deepEqual(hooks.notifications, []);
});

test('a fresh authorization supersedes a pending old exchange', async () => {
  const pending = deferred();
  let session = 0;
  const successes = [];
  const hooks = harness({ startFn: async () => ({ session_id: String(++session), auth_url: 'url' }), exchangeFn: () => pending.promise, onSuccess: (token) => successes.push(token) });
  await hooks.render().start();
  hooks.render().setCallbackUrl('code');
  const running = hooks.render().exchange();
  await hooks.render().start();
  pending.resolve({ credentials: 'old-token' });
  await running;
  assert.equal(hooks.render().sessionId, '2');
  assert.deepEqual(successes, []);
});

test('Devin has a localized native login section and saves tokens in channel credentials', () => {
  const component = readFileSync(new URL('../components/devin-oauth-flow.tsx', import.meta.url), 'utf8');
  const dialog = readFileSync(new URL('../components/channels-action-dialog.tsx', import.meta.url), 'utf8');
  const api = readFileSync(new URL('../data/devin.ts', import.meta.url), 'utf8');
  assert.match(api, /\/admin\/devin\/oauth\/start/);
  assert.match(api, /\/admin\/devin\/oauth\/exchange/);
  assert.match(dialog, /selectedProvider === 'devin'[\s\S]*<DevinOAuthFlow/);
  assert.match(dialog, /<DevinOAuthFlow[\s\S]*form\.setValue\('credentials\.apiKeys', \[token\]/);
  for (const locale of ['en', 'zh-CN']) {
    const strings = JSON.parse(readFileSync(new URL(`../../../locales/${locale}/channels.json`, import.meta.url), 'utf8'));
    for (const key of [...component.matchAll(/'(channels\.dialogs\.devin\.oauth\.[^']+)'/g)].map((match) => match[1])) assert.ok(strings[key], `${locale} missing ${key}`);
  }
});
