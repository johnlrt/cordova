<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Overview from '$lib/components/Overview.svelte';
  import Nodes from '$lib/components/Nodes.svelte';
  import Models from '$lib/components/Models.svelte';
  import Docs from '$lib/components/Docs.svelte';
  import ConnectionDialog from '$lib/components/ConnectionDialog.svelte';
  import { Pilot } from '$lib/state/pilot.svelte';
  import { messages, type Language } from '$lib/i18n';

  const pilot = new Pilot();
  let lang = $state<Language>('en');
  const text = $derived(messages[lang].shell);
  const pages = $derived([
    { id: 'overview', title: text.pages.overview, icon: 'grid', description: text.descriptions.overview },
    { id: 'nodes', title: text.pages.nodes, icon: 'server', description: text.descriptions.nodes },
    { id: 'models', title: text.pages.models, icon: 'layers', description: text.descriptions.models },
    { id: 'docs', title: text.pages.docs, icon: 'book', description: text.descriptions.docs }
  ]);
  let active = $state('overview');
  let dialog: ConnectionDialog;
  const current = $derived(pages.find((page) => page.id === active) || pages[0]);
  function navigate(page: string) { active = page; window.location.hash = page; }
  function syncHash() { const page = window.location.hash.slice(1); if (page === 'main-content') return; active = pages.some((item) => item.id === page) ? page : 'overview'; }
  function connect() { dialog.open(); }
  function setLanguage(next: Language) {
    lang = next;
    document.documentElement.lang = next;
    try { localStorage.setItem('cordova-language', next); } catch { /* Language preference is optional. */ }
  }
  function refresh() { void pilot.refreshHealth(); if (pilot.connected) void pilot.refreshCatalog(); }
  onMount(() => {
    try {
      const saved = localStorage.getItem('cordova-language');
      if (saved === 'en' || saved === 'ru') lang = saved;
    } catch { /* Use English when browser storage is unavailable. */ }
    document.documentElement.lang = lang;
    syncHash();
    refresh();
    window.addEventListener('hashchange', syncHash);
    const timer = window.setInterval(() => { if (document.visibilityState === 'visible') void pilot.refreshHealth(); }, 20000);
    const visible = () => { if (document.visibilityState === 'visible') void pilot.refreshHealth(); };
    document.addEventListener('visibilitychange', visible);
    return () => { clearInterval(timer); window.removeEventListener('hashchange', syncHash); document.removeEventListener('visibilitychange', visible); pilot.dispose(); };
  });
</script>

<svelte:head><title>{current.title} · Cordova</title><meta name="description" content={lang === 'en' ? 'Cordova local pilot dashboard: one Ultra host and one generation channel.' : 'Панель локального пилота Cordova: хост Ultra и один канал генерации.'} /></svelte:head>

<a class="skip-link" href="#main-content">{text.skip}</a>
<div class="app-shell">
  <aside class="sidebar">
    <a class="brand" href="#overview" aria-label={text.brand}><svg width="34" height="34" viewBox="0 0 48 48" aria-hidden="true"><rect width="48" height="48" rx="12" fill="#4567fa" /><path d="M32 15a13 13 0 1 0 0 18M31 18a9 9 0 1 0 0 12" stroke="white" stroke-width="3" fill="none" stroke-linecap="round" /></svg><span>cordova<span class="brand-dot">.</span></span></a>
    <div class="workspace"><span class="workspace-symbol">C</span><div><strong>{text.workspace}</strong><span>{text.oneWorkspace}</span></div><span class="workspace-chevron">⌄</span></div>
    <span class="nav-label">{text.workArea}</span>
    <nav aria-label={text.workArea}>{#each pages as page}<a id={page.id} href={`#${page.id}`} class:active={active === page.id} aria-current={active === page.id ? 'page' : undefined}><Icon name={page.icon} size={19} /><span>{page.title}</span>{#if page.id === 'nodes'}<span class="nav-count">1</span>{/if}</a>{/each}</nav>
    <div class="future-nav"><span class="nav-label">{text.next}</span><div><Icon name="key" size={18} /><span>{text.keys}</span><span class="soon">{text.later}</span></div><div><Icon name="wallet" size={18} /><span>{text.payments}</span><span class="soon">{text.later}</span></div></div>
    <div class="sidebar-bottom"><div class="pilot-card"><span class="tiny-dot"></span><strong>{text.oneHost}</strong><p>{text.hostRam}<br />{text.i7Client}</p></div><div class="sidebar-footer"><span class="avatar">C</span><div><strong>{text.pilot}</strong><small>{text.stage}</small></div><span class="version">v0.1</span></div></div>
  </aside>
  <div class="main-area">
    <header class="topbar"><div class="breadcrumb">{text.breadcrumb} <span>/</span> <strong>{current.title}</strong></div><div class="topbar-tools"><div class="language-switch" role="group" aria-label={text.language}><button class:chosen={lang === 'en'} aria-label={text.languageEnglish} aria-pressed={lang === 'en'} onclick={() => setLanguage('en')}>EN</button><button class:chosen={lang === 'ru'} aria-label={text.languageRussian} aria-pressed={lang === 'ru'} onclick={() => setLanguage('ru')}>RU</button></div><div class="topbar-status"><span class="status-dot" class:online={pilot.status === 'online'}></span>{text.gateway[pilot.status]}<span class="badge amber">MOCK</span></div></div></header>
    <main id="main-content" tabindex="-1"><div class="page-heading"><div><span class="eyebrow muted">{text.dashboard}</span><h1>{current.title}</h1><p>{current.description}</p></div><div class="heading-actions"><button class="icon-button refresh-button" aria-label={text.refresh} disabled={pilot.refreshing} onclick={refresh}><Icon name="refresh" size={19} /></button>{#if pilot.connected}<button class="secondary" onclick={() => pilot.disconnect()}><Icon name="check" size={17} /> {text.disconnect}</button>{:else}<button class="secondary" onclick={connect}><Icon name="link" size={17} /> {text.connect}</button>{/if}</div></div>
      <div class="mode-banner"><span class="mode-symbol"><Icon name="info" size={17} /></span><p><strong>{text.mockTitle}</strong> {text.mockText}</p><button class="text-button" onclick={() => navigate('docs')}>{text.how} <Icon name="arrow" size={15} /></button></div>
      {#if pilot.status === 'offline'}<div class="offline-banner" role="status">{text.offline}</div>{/if}
      {#if active === 'overview'}<Overview {pilot} {navigate} {lang} />{:else if active === 'nodes'}<Nodes {pilot} {lang} />{:else if active === 'models'}<Models {pilot} {connect} {lang} />{:else}<Docs {connect} {lang} />{/if}
      <footer class="content-footer"><span>CORDOVA <span class="footer-separator">/</span> {text.footer}</span><a href="#docs">{text.docsLink} <Icon name="arrow" size={14} /></a></footer>
    </main>
  </div>
</div>
<ConnectionDialog bind:this={dialog} {pilot} {lang} />
