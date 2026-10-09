<script lang="ts">
  import { onMount } from 'svelte';

  let lang = $state<'en' | 'ru'>('en');
  const copy = $derived(lang === 'ru' ? {
    eyebrow: 'ЛОКАЛЬНАЯ AI-ИНФРАСТРУКТУРА', title: 'Один хост. Один канал.',
    body: 'Cordova направляет запросы к локальному исполнителю. Клиент подключается как обычный пользователь; нагрузка модели остаётся на хосте команды.',
    privacy: 'Узлы сообщества получают промпты в открытом виде и могут их копировать. Cordova не может технически предотвратить это на потребительском оборудовании. Не отправляйте секреты или данные, которые нельзя раскрыть обслуживающему узлу. Небольшая раскрытая проверочная выборка также может попасть второму узлу.',
    login: 'Войти', register: 'Создать аккаунт', pilot: 'Открыть пилот', docs: 'Документация'
  } : {
    eyebrow: 'LOCAL AI INFRASTRUCTURE', title: 'One host. One channel.',
    body: 'Cordova routes requests to a local executor. Clients connect as regular users while model work stays on the team host.',
    privacy: 'Community nodes receive prompts in plaintext and can copy them. Cordova cannot technically prevent this on consumer hardware. Do not submit secrets or data you cannot disclose to the serving node. A small disclosed verification sample may also reach a second community node.',
    login: 'Sign in', register: 'Create account', pilot: 'Open pilot', docs: 'Documentation'
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
</script>

<svelte:head><title>Cordova · Local AI network</title><meta name="description" content="A transparent dashboard for a single-host local AI pilot." /></svelte:head>
<main class="marketing-page">
  <header class="marketing-top"><a class="brand" href="/" aria-label="Cordova home"><span class="brand-mark">C</span><span>cordova<span class="brand-dot">.</span></span></a><div class="auth-actions"><div class="language-switch" role="group" aria-label={lang === 'ru' ? 'Язык' : 'Language'}><button class:chosen={lang === 'en'} aria-pressed={lang === 'en'} onclick={() => setLanguage('en')}>EN</button><button class:chosen={lang === 'ru'} aria-pressed={lang === 'ru'} onclick={() => setLanguage('ru')}>RU</button></div><a class="secondary" href="/login">{copy.login}</a><a class="primary" href="/register">{copy.register}</a></div></header>
  <section class="marketing-hero" aria-labelledby="hero-title"><p class="eyebrow">{copy.eyebrow}</p><h1 id="hero-title">{copy.title}</h1><p class="marketing-copy">{copy.body}</p><div class="marketing-links"><a class="primary" href="/register">{copy.register}</a><a class="secondary" href="/pilot">{copy.pilot}</a><a class="text-link" href="/pilot#docs">{copy.docs} ↗</a></div></section>
  <aside class="privacy-note" aria-label={lang === 'ru' ? 'Предупреждение о приватности' : 'Privacy notice'}><strong>{lang === 'ru' ? 'ВАЖНО О ПРИВАТНОСТИ' : 'PRIVACY NOTICE'}</strong><p>{copy.privacy}</p></aside>
  <footer class="marketing-footer">CORDOVA <span>/</span> {lang === 'ru' ? 'Локальная мощность. Прозрачный статус.' : 'Local capacity. Transparent status.'}</footer>
</main>
