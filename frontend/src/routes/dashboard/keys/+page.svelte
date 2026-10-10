<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiFailure } from '$lib/api/client.ts';
  import { createAPIKey, listAPIKeys, revokeAPIKey, type APIKeySettings } from '$lib/api/account.ts';
  import { formatXmrCap, parseXmrCap } from '$lib/api/key-money.ts';
  import type { APIKey } from '$lib/api/generated/contracts.ts';
  import { language } from '$lib/state/language.svelte.ts';
  import { session } from '$lib/state/session.svelte.ts';

  type Permission = APIKeySettings['permissions'][number];
  const modelID = 'cordova-chat-small-v1';
  const scopes: Array<{ id: Permission; en: string; ru: string }> = [
    { id: 'chat:write', en: 'Create chat completions', ru: 'Создавать ответы чата' },
    { id: 'models:read', en: 'Read the model catalog', ru: 'Читать каталог моделей' },
    { id: 'usage:read', en: 'Read request usage', ru: 'Читать историю запросов' }
  ];

  let active = false;
  let accountID = '';
  let generation = -1;
  let controllers = new Set<AbortController>();
  let removeSessionCleanup = () => {};
  let keys = $state<APIKey[]>([]);
  let listState = $state<'loading' | 'ready' | 'error'>('loading');
  let loadError = $state('');
  let operationError = $state('');
  let message = $state('');
  let loading = $state(false);
  let creating = $state(false);
  let revokingID = $state('');
  let revokeTarget = $state<APIKey | null>(null);
  let name = $state('');
  let dailyCapXMR = $state('');
  let selectedPermissions = $state<Permission[]>([]);
  let modelSelected = $state(false);
  let privacyAccepted = $state(false);
  let createdSecret = $state('');
  let copied = $state(false);
  let secretInput: HTMLInputElement | undefined = $state();
  const ru = $derived(language.value === 'ru');

  function controller() {
    const value = new AbortController();
    controllers.add(value);
    return value;
  }

  function release(value: AbortController) { controllers.delete(value); }

  function clearTransientState() {
    active = false;
    for (const request of controllers) request.abort();
    controllers.clear();
    createdSecret = '';
    copied = false;
    keys = [];
  }

  onMount(() => {
    language.load();
    active = true;
    accountID = session.session?.account.id ?? '';
    generation = session.generation;
    removeSessionCleanup = session.addLiveCleanup(clearTransientState);
    void refreshKeys();
    return () => {
      removeSessionCleanup();
      clearTransientState();
    };
  });

  function belongsToCurrentSession() {
    return active && session.generation === generation && session.session?.account.id === accountID;
  }

  function errorMessage(error: unknown, operation: 'load' | 'mutate'): string {
    const prefix = ru ? 'Запрос не выполнен' : 'The request could not be completed';
    if (!(error instanceof ApiFailure)) return ru ? 'Сервис временно недоступен.' : 'The service is temporarily unavailable.';
    if (error.status === 401) {
      session.expireIfCurrent(generation);
      return ru ? 'Сессия завершена. Войдите снова.' : 'Your session ended. Sign in again.';
    }
    if (error.code === 'verification_required') return ru
      ? 'Перед созданием ключа подтвердите актуальное уведомление о приватности.'
      : 'Accept the current privacy notice before creating a key.';
    if (error.code === 'invalid_request') return ru
      ? 'Проверьте название, разрешения, модель и лимит расходов.'
      : 'Check the key name, permissions, model and spend cap.';
    if (error.status === 429) return ru
      ? `Слишком много операций. Повторите позже${error.retryAfter !== null ? ` (через ${error.retryAfter} с)` : ''}.`
      : `Too many operations. Retry later${error.retryAfter !== null ? ` (in ${error.retryAfter}s)` : ''}.`;
    if (error.status === 404) return ru ? 'Ключ уже недоступен. Обновите список.' : 'The key is no longer available. Refresh the list.';
    if (error.status >= 500) return ru ? 'Сервис временно недоступен. Попробуйте позже.' : 'The service is temporarily unavailable. Try again later.';
    return `${prefix}${operation === 'load' ? (ru ? ' при загрузке списка.' : ' while loading the list.') : (ru ? '.' : '.')}`;
  }

  async function refreshKeys() {
    if (!belongsToCurrentSession() || loading) return;
    const request = controller();
    loading = true;
    if (!keys.length) listState = 'loading';
    loadError = '';
    try {
      const result = await listAPIKeys(request.signal);
      if (!request.signal.aborted && belongsToCurrentSession()) {
        keys = result.data;
        listState = 'ready';
      }
    } catch (error) {
      if (!request.signal.aborted && belongsToCurrentSession()) {
        listState = 'error';
        loadError = errorMessage(error, 'load');
      }
    } finally {
      release(request);
      if (belongsToCurrentSession()) loading = false;
    }
  }

  function spendCap(): string | null {
    return parseXmrCap(dailyCapXMR);
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (creating || !belongsToCurrentSession()) return;
    operationError = '';
    message = '';
    const cap = spendCap();
    if (cap === null) {
      operationError = ru
        ? 'Введите сумму от 0 до 9 000 000 XMR, не более 12 знаков после точки.'
        : 'Enter an amount from 0 to 9,000,000 XMR with up to 12 decimal places.';
      return;
    }
    if (!selectedPermissions.length || !modelSelected || !privacyAccepted) {
      operationError = ru
        ? 'Выберите хотя бы одно разрешение и модель, затем подтвердите уведомление о приватности.'
        : 'Choose at least one permission and model, then accept the privacy notice.';
      return;
    }
    const current = session.session;
    if (!current) return;
    const request = controller();
    creating = true;
    try {
      const settings: APIKeySettings = {
        name: name.trim(),
        permissions: [...selectedPermissions],
        model_ids: [modelID],
        daily_spend_cap_piconero: cap
      };
      const created = await createAPIKey(settings, current.csrf_token, request.signal);
      if (request.signal.aborted || !belongsToCurrentSession()) return;
      createdSecret = created.secret;
      copied = false;
      name = '';
      dailyCapXMR = '';
      selectedPermissions = [];
      modelSelected = false;
      privacyAccepted = false;
      message = ru ? 'Ключ создан. Скопируйте секрет сейчас: позже его нельзя будет получить снова.' : 'Key created. Copy the secret now; it cannot be retrieved again.';
      await refreshKeys();
    } catch (error) {
      if (!request.signal.aborted && belongsToCurrentSession()) operationError = errorMessage(error, 'mutate');
    } finally {
      release(request);
      if (belongsToCurrentSession()) creating = false;
    }
  }

  async function copySecret() {
    if (!createdSecret) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard_unavailable');
      await navigator.clipboard.writeText(createdSecret);
      copied = true;
    } catch {
      selectSecret();
    }
  }

  function selectSecret() {
    secretInput?.focus();
    secretInput?.select();
    copied = false;
  }

  function dismissSecret() {
    createdSecret = '';
    copied = false;
    message = '';
  }

  async function confirmRevoke() {
    const target = revokeTarget;
    const current = session.session;
    if (!target || !current || revokingID) return;
    operationError = '';
    message = '';
    const request = controller();
    revokingID = target.id;
    try {
      await revokeAPIKey(target.id, current.csrf_token, request.signal);
      if (request.signal.aborted || !belongsToCurrentSession()) return;
      revokeTarget = null;
      message = ru ? 'Ключ отозван. Список обновлён с сервера.' : 'Key revoked. The list was refreshed from the server.';
      await refreshKeys();
    } catch (error) {
      if (!request.signal.aborted && belongsToCurrentSession()) operationError = errorMessage(error, 'mutate');
    } finally {
      release(request);
      if (belongsToCurrentSession()) revokingID = '';
    }
  }

  function date(value: string | null) {
    if (!value) return ru ? 'Никогда' : 'Never used';
    const parsed = new Date(value);
    if (!Number.isFinite(parsed.getTime())) return ru ? 'Неизвестно' : 'Unknown';
    return new Intl.DateTimeFormat(ru ? 'ru-RU' : 'en', { dateStyle: 'medium', timeStyle: 'short' }).format(parsed);
  }

  function scopeLabel(scope: Permission) {
    return scopes.find((item) => item.id === scope)?.[ru ? 'ru' : 'en'] ?? scope;
  }
</script>

<svelte:head><title>{ru ? 'API-ключи · Cordova' : 'API keys · Cordova'}</title></svelte:head>
<section class="account-content keys-page">
  <p class="eyebrow">{ru ? 'УЧЁТНЫЕ ДАННЫЕ' : 'CREDENTIALS'}</p>
  <div class="keys-heading"><div><h1>{ru ? 'API-ключи' : 'API keys'}</h1><p class="account-intro">{ru ? 'Создавайте ключи с отдельными разрешениями и ограничением расходов. Секрет показывается только один раз.' : 'Create keys with scoped permissions and a spend limit. Each secret is shown only once.'}</p></div><button class="secondary" disabled={loading} onclick={() => void refreshKeys()}>{ru ? 'Обновить список' : 'Refresh list'}</button></div>

  {#if message}<p class="key-message" role="status">{message}</p>{/if}
  {#if operationError}<p class="key-error" role="alert">{operationError}</p>{/if}

  {#if createdSecret}
    <section class="account-panel secret-panel" aria-labelledby="secret-heading">
      <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'СЕКРЕТ ПОКАЗАН ОДИН РАЗ' : 'ONE-TIME SECRET'}</p><h2 id="secret-heading">{ru ? 'Сохраните ключ в безопасном месте' : 'Store this key securely'}</h2></div></div>
      <p class="secret-warning">{ru ? 'После закрытия этого блока сервер больше не вернёт секрет. Не отправляйте его в чат, не добавляйте в Git и не помещайте в URL.' : 'After you close this panel, the server will never return this secret again. Do not share it, commit it, or put it in a URL.'}</p>
      <div class="secret-controls"><label class="sr-only" for="created-secret">{ru ? 'Новый API-ключ' : 'New API key'}</label><input id="created-secret" bind:this={secretInput} readonly value={createdSecret} autocomplete="off" spellcheck="false" /><button class="secondary" onclick={() => void copySecret()}>{copied ? (ru ? 'Скопировано' : 'Copied') : (ru ? 'Копировать' : 'Copy')}</button><button class="secondary" onclick={selectSecret}>{ru ? 'Выделить вручную' : 'Select manually'}</button><button class="text-button" onclick={dismissSecret}>{ru ? 'Закрыть и очистить' : 'Close and clear'}</button></div>
    </section>
  {/if}

  <section class="account-panel key-create-panel" aria-labelledby="create-key-heading">
    <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'НОВЫЙ КЛЮЧ' : 'NEW KEY'}</p><h2 id="create-key-heading">{ru ? 'Создать API-ключ' : 'Create an API key'}</h2></div><span class="badge neutral">{keys.length}/20</span></div>
    <form class="key-form" onsubmit={submit}>
      <label class="key-field">{ru ? 'Название' : 'Name'}<input name="name" bind:value={name} maxlength="64" required autocomplete="off" placeholder={ru ? 'Например, локальная разработка' : 'For example, local development'} /></label>
      <label class="key-field">{ru ? 'Дневной лимит расходов · XMR' : 'Daily spend cap · XMR'}<input name="daily-cap" bind:value={dailyCapXMR} inputmode="decimal" required autocomplete="off" placeholder="0.01" aria-describedby="key-cap-help" /><small id="key-cap-help">{ru ? 'Точное значение: до 12 знаков после точки; сервер хранит целые пiconero.' : 'Exact decimal input: up to 12 places; the server stores integer piconero.'}</small></label>
      <fieldset class="key-options"><legend>{ru ? 'Разрешения' : 'Permissions'}</legend>{#each scopes as scope}<label><input type="checkbox" bind:group={selectedPermissions} value={scope.id} /><span>{scopeLabel(scope.id)} <code>{scope.id}</code></span></label>{/each}</fieldset>
      <fieldset class="key-options"><legend>{ru ? 'Модели' : 'Models'}</legend><label><input type="checkbox" bind:checked={modelSelected} /><span>{modelID} <small>{ru ? 'Синтетическая модель пилота' : 'Synthetic pilot model'}</small></span></label></fieldset>
      <label class="privacy-accept"><input type="checkbox" bind:checked={privacyAccepted} /><span>{ru ? 'Я прочитал(а) и принимаю актуальное ' : 'I have read and accept the current '}<a href="/#privacy" target="_blank" rel="noreferrer">{ru ? 'уведомление о приватности' : 'privacy notice'}</a>. {ru ? 'Узел, обслуживающий запрос, может видеть его текст.' : 'A serving node may see request text.'}</span></label>
      <div class="key-form-actions"><button class="primary" type="submit" disabled={creating || loading || keys.length >= 20}>{creating ? (ru ? 'Создаём…' : 'Creating…') : (ru ? 'Создать ключ' : 'Create key')}</button><p>{ru ? 'Права можно сузить; лимит применяется сервером к каждому запросу.' : 'Grant only the permissions needed. The server enforces the spend cap.'}</p></div>
    </form>
  </section>

  <section class="account-panel usage-panel keys-list-panel" aria-labelledby="keys-heading">
    <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'КЛЮЧИ АККАУНТА' : 'ACCOUNT KEYS'}</p><h2 id="keys-heading">{ru ? 'Ваши ключи' : 'Your keys'}</h2></div></div>
    {#if listState === 'loading'}
      <p class="usage-message" role="status">{ru ? 'Загружаем ключи…' : 'Loading keys…'}</p>
    {:else if listState === 'error'}
      <div class="usage-empty" role="status"><strong>{loadError}</strong><button class="secondary" onclick={() => void refreshKeys()}>{ru ? 'Повторить' : 'Retry'}</button></div>
    {:else if keys.length === 0}
      <div class="usage-empty"><strong>{ru ? 'API-ключей пока нет' : 'No API keys yet'}</strong><p>{ru ? 'Создайте ключ выше, чтобы подключить клиент к API.' : 'Create a key above to connect a client to the API.'}</p></div>
    {:else}
      <div class="keys-table-wrap"><table class="keys-table"><thead><tr><th>{ru ? 'Ключ' : 'Key'}</th><th>{ru ? 'Разрешения' : 'Permissions'}</th><th>{ru ? 'Лимит в день' : 'Daily cap'}</th><th>{ru ? 'Последнее использование' : 'Last used'}</th><th>{ru ? 'Состояние' : 'State'}</th><th></th></tr></thead><tbody>
        {#each keys as key (key.id)}
          <tr><td><strong>{key.name}</strong><small>{key.prefix}…</small></td><td><div class="key-scope-list">{#each key.permissions as permission}<span>{scopeLabel(permission)}</span>{/each}<small>{key.model_ids.join(', ')}</small></div></td><td>{formatXmrCap(key.daily_spend_cap_piconero, ru ? 'ru-RU' : 'en')}</td><td>{date(key.last_used_at)}</td><td><span class="badge" class:neutral={key.revoked_at !== null} class:green={key.revoked_at === null}>{key.revoked_at ? (ru ? 'Отозван' : 'Revoked') : (ru ? 'Активен' : 'Active')}</span></td><td>{#if key.revoked_at === null}<button class="text-button revoke-button" disabled={Boolean(revokingID)} onclick={() => { revokeTarget = key; operationError = ''; }}>{ru ? 'Отозвать' : 'Revoke'}</button>{/if}</td></tr>
          {#if revokeTarget?.id === key.id}
            <tr class="revoke-row"><td colspan="6"><div class="revoke-confirm" role="group" aria-label={ru ? `Подтвердить отзыв ключа ${key.name}` : `Confirm revocation of ${key.name}`}><div><strong>{ru ? `Отозвать «${key.name}»?` : `Revoke “${key.name}”?`}</strong><p>{ru ? 'Новые запросы перестанут проходить. Активные запросы с этим ключом будут отменены.' : 'New requests will stop working. Active requests using this key will be cancelled.'}</p></div><button class="danger-button" disabled={Boolean(revokingID)} onclick={() => void confirmRevoke()}>{revokingID === key.id ? (ru ? 'Отзываем…' : 'Revoking…') : (ru ? 'Подтвердить отзыв' : 'Confirm revoke')}</button><button class="secondary compact" disabled={Boolean(revokingID)} onclick={() => { revokeTarget = null; }}>{ru ? 'Отмена' : 'Cancel'}</button></div></td></tr>
          {/if}
        {/each}
      </tbody></table></div>
    {/if}
  </section>
</section>
