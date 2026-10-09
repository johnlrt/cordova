<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from '$lib/components/Icon.svelte';
  import Overview from '$lib/components/Overview.svelte';
  import Nodes from '$lib/components/Nodes.svelte';
  import Models from '$lib/components/Models.svelte';
  import Docs from '$lib/components/Docs.svelte';
  import ConnectionDialog from '$lib/components/ConnectionDialog.svelte';
  import { Pilot } from '$lib/state/pilot.svelte';

  const pilot = new Pilot();
  const pages = [
    { id: 'overview', title: 'Обзор', icon: 'grid', description: 'Ваша сеть в одном месте' },
    { id: 'nodes', title: 'Нода', icon: 'server', description: 'Конфигурация единственного хоста' },
    { id: 'models', title: 'Модели', icon: 'layers', description: 'Каталог вашего шлюза' },
    { id: 'docs', title: 'Документация', icon: 'book', description: 'Подключение и возможности API' }
  ];
  let active = $state('overview');
  let dialog: ConnectionDialog;
  const current = $derived(pages.find((page) => page.id === active) || pages[0]);
  function navigate(page: string) { active = page; window.location.hash = page; }
  function syncHash() { const page = window.location.hash.slice(1); if (page === 'main-content') return; active = pages.some((item) => item.id === page) ? page : 'overview'; }
  function connect() { dialog.open(); }
  function refresh() { void pilot.refreshHealth(); if (pilot.connected) void pilot.refreshCatalog(); }
  onMount(() => {
    syncHash();
    refresh();
    window.addEventListener('hashchange', syncHash);
    const timer = window.setInterval(() => { if (document.visibilityState === 'visible') void pilot.refreshHealth(); }, 20000);
    const visible = () => { if (document.visibilityState === 'visible') void pilot.refreshHealth(); };
    document.addEventListener('visibilitychange', visible);
    return () => { clearInterval(timer); window.removeEventListener('hashchange', syncHash); document.removeEventListener('visibilitychange', visible); pilot.dispose(); };
  });
</script>

<svelte:head><title>{current.title} · Cordova</title><meta name="description" content="Панель локального пилота Cordova: шлюз Ultra, одна нода и один канал генерации." /></svelte:head>

<a class="skip-link" href="#main-content">Перейти к содержимому</a>
<div class="app-shell">
  <aside class="sidebar">
    <a class="brand" href="#overview" aria-label="Cordova — обзор"><svg width="34" height="34" viewBox="0 0 48 48" aria-hidden="true"><rect width="48" height="48" rx="12" fill="#4567fa" /><path d="M32 15a13 13 0 1 0 0 18M31 18a9 9 0 1 0 0 12" stroke="white" stroke-width="3" fill="none" stroke-linecap="round" /></svg><span>cordova<span class="brand-dot">.</span></span></a>
    <div class="workspace"><span class="workspace-symbol">C</span><div><strong>Локальный пилот</strong><span>Одно пространство</span></div><span class="workspace-chevron">⌄</span></div>
    <span class="nav-label">РАБОЧЕЕ ПРОСТРАНСТВО</span>
    <nav aria-label="Основная навигация">{#each pages as page}<a id={page.id} href={`#${page.id}`} class:active={active === page.id} aria-current={active === page.id ? 'page' : undefined}><Icon name={page.icon} size={19} /><span>{page.title}</span>{#if page.id === 'nodes'}<span class="nav-count">1</span>{/if}</a>{/each}</nav>
    <div class="future-nav"><span class="nav-label">СЛЕДУЮЩИЕ ЭТАПЫ</span><div><Icon name="key" size={18} /><span>Управление ключами</span><span class="soon">Позже</span></div><div><Icon name="wallet" size={18} /><span>Платежи</span><span class="soon">Позже</span></div></div>
    <div class="sidebar-bottom"><div class="pilot-card"><span class="tiny-dot"></span><strong>Один хост, один канал</strong><p>Ultra · 64 ГБ RAM<br />i7 подключается как клиент</p></div><div class="sidebar-footer"><span class="avatar">C</span><div><strong>Cordova Pilot</strong><small>Mock API · этап 1</small></div><span class="version">v0.1</span></div></div>
  </aside>
  <div class="main-area">
    <header class="topbar"><div class="breadcrumb">Рабочее пространство <span>/</span> <strong>{current.title}</strong></div><div class="topbar-status"><span class="status-dot" class:online={pilot.status === 'online'}></span>{pilot.status === 'online' ? 'Шлюз на связи' : pilot.status === 'checking' ? 'Проверяем шлюз' : 'Нет связи'}<span class="badge amber">MOCK</span></div></header>
    <main id="main-content" tabindex="-1"><div class="page-heading"><div><span class="eyebrow muted">ПАНЕЛЬ СЕТИ</span><h1>{current.title}</h1><p>{current.description}</p></div><div class="heading-actions"><button class="icon-button refresh-button" aria-label="Обновить состояние шлюза" disabled={pilot.refreshing} onclick={refresh}><Icon name="refresh" size={19} /></button>{#if pilot.connected}<button class="secondary" onclick={() => pilot.disconnect()}><Icon name="check" size={17} /> Отключить ключ</button>{:else}<button class="secondary" onclick={connect}><Icon name="link" size={17} /> Подключить шлюз</button>{/if}</div></div>
      <div class="mode-banner"><span class="mode-symbol"><Icon name="info" size={17} /></span><p><strong>Пилот в mock-режиме.</strong> Связь со шлюзом реальная; генерация AI и платежи пока не подключены.</p><button class="text-button" onclick={() => navigate('docs')}>Как это работает <Icon name="arrow" size={15} /></button></div>
      {#if pilot.status === 'offline'}<div class="offline-banner" role="status">Шлюз недоступен или вернул неподдерживаемый ответ. Запустите сервер и проверьте настройку API. Показанные характеристики хоста — конфигурация команды.</div>{/if}
      {#if active === 'overview'}<Overview {pilot} {navigate} />{:else if active === 'nodes'}<Nodes {pilot} />{:else if active === 'models'}<Models {pilot} {connect} />{:else}<Docs {connect} />{/if}
      <footer class="content-footer"><span>CORDOVA <span class="footer-separator">/</span> Локальная мощность. Прозрачный статус.</span><a href="#docs">Документация <Icon name="arrow" size={14} /></a></footer>
    </main>
  </div>
</div>
<ConnectionDialog bind:this={dialog} {pilot} />
