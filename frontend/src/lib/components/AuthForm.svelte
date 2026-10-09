<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { onMount } from 'svelte';
  import { ApiFailure } from '$lib/api/client.ts';
  import { safeReturnPath } from '$lib/api/navigation.ts';
  import { session } from '$lib/state/session.svelte.ts';

  let { mode }: { mode: 'login' | 'register' | 'recover' } = $props();
  let lang = $state<'en' | 'ru'>('en');
  let username = $state('');
  let password = $state('');
  let confirmPassword = $state('');
  let totpCode = $state('');
  let recoveryCode = $state('');
  let privacyAccepted = $state(false);
  let busy = $state(false);
  let error = $state('');
  let recoveryCodes = $state<string[]>([]);
  let codesAcknowledged = $state(false);
  let copied = $state(false);
  let payoutNotice = $state(false);

  const ru = $derived(lang === 'ru');
  const texts = $derived(ru ? {
    title: mode === 'login' ? 'Войти в Cordova' : mode === 'register' ? 'Создать аккаунт' : 'Восстановить доступ',
    intro: mode === 'login' ? 'Войдите, чтобы открыть панель аккаунта.' : mode === 'register' ? 'Аккаунт нужен для панели и управления своими узлами.' : 'Используйте один из сохранённых одноразовых кодов восстановления.',
    username: 'Имя пользователя', password: mode === 'recover' ? 'Новый пароль' : 'Пароль', confirm: 'Повторите пароль',
    totp: 'Код двухфакторной аутентификации (если включена)', recovery: 'Одноразовый код восстановления',
    privacyLabel: 'Я прочитал и принимаю предупреждение о приватности.', privacyHeading: 'Перед продолжением',
    privacy: 'Узлы сообщества получают промпты в открытом виде и могут их копировать. Cordova не может технически предотвратить это на потребительском оборудовании. Не отправляйте секреты или данные, которые нельзя раскрыть обслуживающему узлу. Небольшая раскрытая проверочная выборка также может попасть второму узлу.',
    submit: mode === 'login' ? 'Войти' : mode === 'register' ? 'Создать аккаунт' : 'Восстановить аккаунт',
    working: 'Подождите…', loginLink: 'Уже есть аккаунт? Войти', registerLink: 'Нет аккаунта? Создать', recoverLink: 'Забыли пароль?',
    back: 'На главную', saveCodes: 'Сохраните эти коды сейчас', codeNote: 'Каждый код можно использовать один раз. Они показаны только сейчас и не сохраняются в браузере.',
    ack: 'Я сохранил коды восстановления в надёжном месте', continue: 'Продолжить в панель', copy: 'Скопировать коды', copied: 'Скопировано',
    payout: 'После восстановления вывод средств заблокирован на 24 часа. Включите TOTP заново в настройках безопасности.',
    errAuth: 'Не удалось войти. Проверьте данные и попробуйте ещё раз.', errGeneric: 'Запрос не выполнен. Проверьте данные и попробуйте снова.',
    errBusy: 'Слишком много попыток. Попробуйте позже.', errWait: 'Сервис пока недоступен. Попробуйте позже.',
    errPassword: 'Пароли не совпадают.', errPrivacy: 'Для создания аккаунта нужно принять предупреждение о приватности.',
    errCodes: 'Не удалось показать коды восстановления. Не обновляйте страницу; обратитесь к поддержке.', errCopy: 'Не удалось скопировать. Выделите коды и скопируйте вручную.'
  } : {
    title: mode === 'login' ? 'Sign in to Cordova' : mode === 'register' ? 'Create an account' : 'Recover your account',
    intro: mode === 'login' ? 'Sign in to open your account dashboard.' : mode === 'register' ? 'Your account gives you access to the dashboard and node ownership.' : 'Use one of your saved one-time recovery codes.',
    username: 'Username', password: mode === 'recover' ? 'New password' : 'Password', confirm: 'Confirm password',
    totp: 'Two-factor code (if enabled)', recovery: 'One-time recovery code',
    privacyLabel: 'I have read and accept the privacy notice.', privacyHeading: 'Before you continue',
    privacy: 'Community nodes receive prompts in plaintext and can copy them. Cordova cannot technically prevent this on consumer hardware. Do not submit secrets or data you cannot disclose to the serving node. A small disclosed verification sample may also reach a second community node.',
    submit: mode === 'login' ? 'Sign in' : mode === 'register' ? 'Create account' : 'Recover account',
    working: 'Please wait…', loginLink: 'Already have an account? Sign in', registerLink: 'New to Cordova? Create an account', recoverLink: 'Forgot password?',
    back: 'Back home', saveCodes: 'Save these codes now', codeNote: 'Each code works once. They are shown only now and are not stored in your browser.',
    ack: 'I saved my recovery codes somewhere safe', continue: 'Continue to dashboard', copy: 'Copy codes', copied: 'Copied',
    payout: 'Payouts are locked for 24 hours after recovery. Re-enable TOTP in security settings.',
    errAuth: 'Sign-in failed. Check your details and try again.', errGeneric: 'The request failed. Check your details and try again.',
    errBusy: 'Too many attempts. Try again later.', errWait: 'The account service is unavailable. Try again later.',
    errPassword: 'The passwords do not match.', errPrivacy: 'Accept the privacy notice to create an account.',
    errCodes: 'Recovery codes could not be displayed. Do not refresh; contact support.', errCopy: 'Could not copy. Select the codes and copy them manually.'
  });

  onMount(() => {
    try { const saved = localStorage.getItem('cordova-language'); if (saved === 'ru' || saved === 'en') lang = saved; } catch { /* Optional preference. */ }
    document.documentElement.lang = lang;
  });

  function setLanguage(value: 'en' | 'ru') {
    lang = value;
    document.documentElement.lang = value;
    try { localStorage.setItem('cordova-language', value); } catch { /* Optional preference. */ }
  }

  function presentError(cause: unknown) {
    if (!(cause instanceof ApiFailure)) { error = texts.errGeneric; return; }
    if (cause.status === 401) error = mode === 'login' ? texts.errAuth : texts.errGeneric;
    else if (cause.status === 429) error = `${texts.errBusy}${cause.retryAfter ? ` (${cause.retryAfter}s)` : ''}`;
    else if (cause.status >= 500 || cause.status === 404 || cause.status === 0) error = texts.errWait;
    else error = texts.errGeneric;
  }

  async function submit() {
    error = '';
    if (mode === 'register' && !privacyAccepted) { error = texts.errPrivacy; return; }
    if ((mode === 'register' || mode === 'recover') && password !== confirmPassword) { error = texts.errPassword; return; }
    busy = true;
    try {
      if (mode === 'login') {
        await session.login(username.trim(), password, totpCode.trim() || undefined);
        password = '';
        totpCode = '';
        await goto(safeReturnPath(page.url.searchParams.get('returnTo')));
      } else if (mode === 'register') {
        const result = await session.register(username.trim(), password);
        password = '';
        confirmPassword = '';
        recoveryCodes = result.recovery_codes;
      } else {
        await session.recover(username.trim(), recoveryCode.trim(), password);
        password = '';
        confirmPassword = '';
        recoveryCode = '';
        payoutNotice = true;
      }
    } catch (cause) { presentError(cause); }
    finally { busy = false; }
  }

  async function copyCodes() {
    try { await navigator.clipboard.writeText(recoveryCodes.join('\n')); copied = true; }
    catch { error = texts.errCopy; }
  }

  async function finishRecoveryCodes() {
    if (!codesAcknowledged) return;
    recoveryCodes = [];
    await goto('/dashboard');
  }
</script>

<svelte:head><title>{texts.title} · Cordova</title><meta name="description" content={texts.intro} /></svelte:head>
<main class="auth-page">
  <header class="auth-top"><a class="brand" href="/" aria-label="Cordova home"><span class="brand-mark">C</span><span>cordova<span class="brand-dot">.</span></span></a><div class="language-switch" role="group" aria-label={ru ? 'Язык' : 'Language'}><button class:chosen={lang === 'en'} aria-pressed={lang === 'en'} onclick={() => setLanguage('en')}>EN</button><button class:chosen={lang === 'ru'} aria-pressed={lang === 'ru'} onclick={() => setLanguage('ru')}>RU</button></div></header>
  <section class="auth-card" aria-labelledby="auth-title">
    {#if recoveryCodes.length}
      <p class="eyebrow">{texts.saveCodes}</p><h1 id="auth-title">{texts.saveCodes}</h1><p class="auth-intro">{texts.codeNote}</p>
      <ol class="recovery-code-list">{#each recoveryCodes as code}<li><code>{code}</code></li>{/each}</ol>
      <button class="secondary auth-copy" type="button" onclick={copyCodes}>{copied ? texts.copied : texts.copy}</button>
      <label class="check-row"><input type="checkbox" bind:checked={codesAcknowledged} /><span>{texts.ack}</span></label>
      <button class="primary auth-submit" type="button" disabled={!codesAcknowledged} onclick={finishRecoveryCodes}>{texts.continue}</button>
    {:else if payoutNotice}
      <p class="eyebrow">{texts.title}</p><h1 id="auth-title">{texts.title}</h1><div class="auth-alert" role="status">{texts.payout}</div><a class="primary auth-submit" href="/dashboard">{texts.continue}</a>
    {:else}
      <p class="eyebrow">CORDOVA ACCOUNT</p><h1 id="auth-title">{texts.title}</h1><p class="auth-intro">{texts.intro}</p>
      {#if mode === 'login' && session.logoutUnconfirmed}<p class="auth-alert" role="status">{ru ? 'Сервер не подтвердил выход. Локальная сессия очищена; закройте эту вкладку, если сервис недоступен.' : 'The server did not confirm sign-out. Local session data is cleared; close this tab if the service is offline.'}</p>{/if}
      <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
        <label for="username">{texts.username}</label><input id="username" name="username" autocomplete="username" minlength="3" maxlength="64" required bind:value={username} />
        {#if mode === 'recover'}
          <label for="recovery-code">{texts.recovery}</label><input id="recovery-code" name="recovery_code" autocomplete="off" required bind:value={recoveryCode} />
        {/if}
        <label for="password">{texts.password}</label><input id="password" name="password" type="password" autocomplete={mode === 'login' ? 'current-password' : 'new-password'} minlength="12" maxlength="128" required bind:value={password} />
        {#if mode !== 'login'}
          <label for="confirm-password">{texts.confirm}</label><input id="confirm-password" name="confirm_password" type="password" autocomplete="new-password" minlength="12" maxlength="128" required bind:value={confirmPassword} />
        {/if}
        {#if mode === 'login'}
          <label for="totp">{texts.totp}</label><input id="totp" name="totp_code" inputmode="numeric" autocomplete="one-time-code" maxlength="16" bind:value={totpCode} />
        {/if}
        {#if mode === 'register'}
          <aside class="auth-privacy"><strong>{texts.privacyHeading}</strong><p>{texts.privacy}</p></aside>
          <label class="check-row"><input type="checkbox" bind:checked={privacyAccepted} required /><span>{texts.privacyLabel}</span></label>
        {/if}
        {#if error}<p class="auth-error" role="alert">{error}</p>{/if}
        <button class="primary auth-submit" type="submit" disabled={busy}>{busy ? texts.working : texts.submit}</button>
      </form>
      <nav class="auth-links" aria-label={ru ? 'Ссылки аккаунта' : 'Account links'}>
        {#if mode === 'login'}<a href="/recover">{texts.recoverLink}</a><a href="/register">{texts.registerLink}</a>
        {:else if mode === 'register'}<a href="/login">{texts.loginLink}</a>
        {:else}<a href="/login">{texts.loginLink}</a>{/if}
        <a href="/">{texts.back}</a>
      </nav>
    {/if}
  </section>
</main>
