<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { session } from '$lib/state/session.svelte.ts';
  import { language } from '$lib/state/language.svelte.ts';
  let { children } = $props();
  let signingOut = $state(false);
  const path = $derived(page.url.pathname);
  const ru = $derived(language.value === 'ru');

  $effect(() => {
    if (session.status === 'signed_out' && page.url.pathname.startsWith('/dashboard')) {
      const returnTo = encodeURIComponent(`${page.url.pathname}${page.url.search}`);
      void goto(`/login?returnTo=${returnTo}`, { replaceState: true });
    }
  });

  onMount(() => {
    language.load();
    let active = true;
    void session.ensureLoaded().then((loaded) => {
      if (active && !loaded && session.status === 'signed_out') {
        const returnTo = encodeURIComponent(`${page.url.pathname}${page.url.search}`);
        void goto(`/login?returnTo=${returnTo}`, { replaceState: true });
      }
    });
    return () => { active = false; };
  });

  async function signOut() {
    if (signingOut) return;
    signingOut = true;
    await session.logout();
    await goto('/login', { replaceState: true });
  }
</script>

<svelte:head><title>{ru ? 'Панель аккаунта · Cordova' : 'Account dashboard · Cordova'}</title></svelte:head>
{#if session.status === 'loading'}
  <main class="account-loading" aria-live="polite"><span class="spinner" aria-hidden="true"></span><p>{ru ? 'Проверяем сессию аккаунта…' : 'Checking your account session…'}</p></main>
{:else if session.status === 'error'}
  <main class="account-loading"><section class="auth-card"><h1>{ru ? 'Не удалось проверить сессию' : 'Can’t verify your session'}</h1><p class="auth-intro">{ru ? 'Сервис аккаунтов не ответил. Закрытая панель не загружена.' : 'The account service did not respond. Your private dashboard has not been loaded.'}</p><button class="primary" onclick={() => { session.status = 'loading'; void session.ensureLoaded(); }}>{ru ? 'Повторить' : 'Try again'}</button></section></main>
{:else if session.status === 'signed_in' && session.session}
  <div class="account-shell">
    <aside class="account-sidebar"><a class="brand" href="/dashboard"><span class="brand-mark">C</span><span>cordova<span class="brand-dot">.</span></span></a><p class="account-nav-label">{ru ? 'АККАУНТ' : 'ACCOUNT'}</p><nav aria-label={ru ? 'Навигация аккаунта' : 'Account navigation'}><a href="/dashboard" class:active={path === '/dashboard'} aria-current={path === '/dashboard' ? 'page' : undefined}>{ru ? 'Обзор' : 'Overview'}</a></nav><div class="account-sidebar-bottom"><span class="avatar">{session.session.account.username.slice(0, 1).toUpperCase()}</span><div><strong>{session.session.account.username}</strong><small>{ru ? 'Аккаунт' : 'Account'}</small></div><button class="signout-link" disabled={signingOut} onclick={() => void signOut()}>{ru ? 'Выйти' : 'Sign out'}</button></div></aside>
    <div class="account-main"><header class="account-topbar"><span>{ru ? 'Рабочая область' : 'Workspace'} <span>/</span> <strong>{ru ? 'Обзор аккаунта' : 'Account overview'}</strong></span><div class="account-top-actions"><div class="language-switch" role="group" aria-label={ru ? 'Язык' : 'Language'}><button class:chosen={!ru} aria-pressed={!ru} onclick={() => language.set('en')}>EN</button><button class:chosen={ru} aria-pressed={ru} onclick={() => language.set('ru')}>RU</button></div><a class="secondary" href="/pilot">{ru ? 'Открыть пилот' : 'Open pilot'}</a></div></header>
      <main id="main-content" tabindex="-1">{@render children()}</main>
    </div>
  </div>
{/if}
