<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiFailure } from '$lib/api/client.ts';
  import { approveNodeEnrollment } from '$lib/api/account.ts';
  import { language } from '$lib/state/language.svelte.ts';
  import { session } from '$lib/state/session.svelte.ts';

  let active = false;
  let generation = -1;
  let request: AbortController | undefined;
  let removeSessionCleanup = () => {};
  let userCode = $state('');
  let confirmedDevice = $state(false);
  let processing = $state(false);
  let errorMessage = $state('');
  let success = $state(false);
  const ru = $derived(language.value === 'ru');

  function clearTransientState() {
    active = false;
    request?.abort();
    request = undefined;
    userCode = '';
    confirmedDevice = false;
    processing = false;
    errorMessage = '';
    success = false;
  }

  onMount(() => {
    language.load();
    active = true;
    void session.ensureLoaded().then((loaded) => {
      if (!active) return;
      if (!loaded) { clearTransientState(); return; }
      generation = session.generation;
      removeSessionCleanup = session.addLiveCleanup(clearTransientState);
    });
    return () => {
      removeSessionCleanup();
      clearTransientState();
    };
  });

  function currentSession() {
    return active && session.generation === generation && session.session !== null;
  }

  function describeError(value: unknown): string {
    if (!(value instanceof ApiFailure)) return ru ? 'Сервис временно недоступен.' : 'The service is temporarily unavailable.';
    if (value.status === 401) {
      session.expireIfCurrent(generation);
      return ru ? 'Сессия завершена. Войдите снова.' : 'Your session ended. Sign in again.';
    }
    if (value.status === 410) return ru
      ? 'Срок действия кода истёк. Запустите вход ноды заново и используйте новый код.'
      : 'This code expired. Restart node sign-in and use the new code.';
    if (value.status === 409) return ru
      ? 'Этот код уже подтверждён или больше недоступен.'
      : 'This code was already approved or is no longer available.';
    if (value.status === 404) return ru
      ? 'Шлюз не включил API подтверждения C3. Администратору нужно настроить ключи C3.'
      : 'This gateway has not enabled C3 enrollment. Its operator must configure the C3 keys.';
    if (value.status === 429) return ru
      ? `Слишком много попыток. Повторите позже${value.retryAfter !== null ? ` (через ${value.retryAfter} с)` : ''}.`
      : `Too many attempts. Retry later${value.retryAfter !== null ? ` (in ${value.retryAfter}s)` : ''}.`;
    if (value.code === 'invalid_request') return ru
      ? 'Код должен состоять из трёх групп символов A–Z или 2–7.'
      : 'Enter the three-part code using letters A–Z or digits 2–7.';
    return ru ? 'Не удалось подтвердить код. Проверьте его и повторите попытку.' : 'The code could not be approved. Check it and try again.';
  }

  async function approve(event: SubmitEvent) {
    event.preventDefault();
    if (processing || !confirmedDevice || !currentSession()) return;
    const current = session.session;
    if (!current) return;
    const controller = new AbortController();
    request = controller;
    processing = true;
    errorMessage = '';
    success = false;
    try {
      await approveNodeEnrollment(userCode, current.csrf_token, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      userCode = '';
      confirmedDevice = false;
      success = true;
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) errorMessage = describeError(value);
    } finally {
      if (request === controller) request = undefined;
      if (currentSession()) processing = false;
    }
  }
</script>

<svelte:head><title>{ru ? 'Доступ к нодам · Cordova' : 'Node access · Cordova'}</title></svelte:head>
<section class="account-content node-access-page">
  <p class="eyebrow">{ru ? 'УПРАВЛЕНИЕ НОДАМИ' : 'NODE MANAGEMENT'}</p>
  <h1>{ru ? 'Подтвердить вход ноды' : 'Approve node sign-in'}</h1>
  <p class="account-intro">{ru ? 'Когда CLI ноды покажет одноразовый код, введите его здесь. После подтверждения нода завершит вход автоматически.' : 'When the node CLI shows a one-time code, enter it here. The node will finish signing in automatically after approval.'}</p>

  {#if errorMessage}<p class="key-error" role="alert">{errorMessage}</p>{/if}
  {#if success}<p class="key-message" role="status">{ru ? 'Вход подтверждён. CLI ноды получит доступ и завершит настройку.' : 'Sign-in approved. The node CLI can now finish setup.'}</p>{/if}

  <section class="account-panel node-approval-panel" aria-labelledby="approval-heading">
    <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'ОДНОРАЗОВЫЙ КОД' : 'ONE-TIME CODE'}</p><h2 id="approval-heading">{ru ? 'Код подтверждения' : 'Verification code'}</h2></div></div>
    <ol class="node-approval-steps"><li>{ru ? 'На устройстве, которым вы владеете, запустите ' : 'On a device you control, run '}<code>cordova-node login</code>.</li><li>{ru ? 'Сверьте показанный в CLI код и введите его ниже до истечения срока.' : 'Check the code shown by the CLI and enter it below before it expires.'}</li></ol>
    <form class="node-approval-form" onsubmit={approve}>
      <label class="key-field">{ru ? 'Код из CLI ноды' : 'Code shown by the node CLI'}<input name="user-code" bind:value={userCode} oninput={(event) => { userCode = event.currentTarget.value.toUpperCase(); }} maxlength="15" pattern={'[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}'} required autocomplete="off" autocapitalize="characters" spellcheck="false" placeholder="ABCDE-FGHIJ-KLM" aria-describedby="node-code-help" /></label>
      <small id="node-code-help" class="node-code-help">{ru ? 'Ввод кода только на этой странице; он не добавляется в URL или хранилище браузера.' : 'The code stays on this page and is never put in the URL or browser storage.'}</small>
      <label class="privacy-accept"><input type="checkbox" bind:checked={confirmedDevice} /><span>{ru ? 'Я проверил(а) CLI-код и подтверждаю вход ноды на устройстве, которым управляю.' : 'I checked the CLI code and approve a node on a device I control.'}</span></label>
      <button class="primary" type="submit" disabled={processing || !confirmedDevice}>{processing ? (ru ? 'Подтверждаем…' : 'Approving…') : (ru ? 'Подтвердить вход' : 'Approve sign-in')}</button>
    </form>
    <p class="node-privacy-note">{ru ? 'Одобренная нода получает постоянный секрет устройства. Позже она может обрабатывать текст ваших запросов. Подтверждайте только устройство, которому доверяете.' : 'An approved node receives a persistent device secret and may later process your request text. Approve only a device you trust.'}</p>
  </section>

  <aside class="node-management-boundary"><strong>{ru ? 'Управление подключёнными нодами ещё не доступно' : 'Connected-node management is not available yet'}</strong><p>{ru ? 'Этот экран подтверждает только вход устройства. Список, состояние, переименование и отзыв нод требуют следующих API-этапов.' : 'This screen only approves device sign-in. Node inventory, status, rename and revocation need later API work.'}</p></aside>
</section>
