<script lang="ts">
  import Icon from './Icon.svelte';
  let { connect }: { connect: () => void } = $props();
  const catalogExample = 'curl.exe http://127.0.0.1:8080/v1/models -H "Authorization: Bearer YOUR_API_KEY"';
  let copied = $state(false);
  let copyError = $state(false);
  async function copy() {
    try { await navigator.clipboard.writeText(catalogExample); copied = true; copyError = false; }
    catch { copyError = true; }
  }
</script>

<div class="section-intro"><div><span class="eyebrow muted">БЫСТРЫЙ СТАРТ</span><h2>От шлюза к вашему приложению</h2><p>Рабочие возможности текущего пилота и следующий шаг.</p></div><span class="badge blue">API v1 · mock</span></div>
<div class="two-columns docs-grid"><section class="panel prose"><span class="doc-number">01</span><h3>Проверьте соединение</h3><p>Запустите шлюз на сервере. Индикатор наверху показывает результат запроса <code>GET /healthz</code>.</p><p>Если интерфейс открыт на i7, его сервер должен направлять API-запросы к Ultra. Адрес <code>127.0.0.1</code> относится к компьютеру, на котором работает соответствующий сервер.</p></section><section class="panel prose"><span class="doc-number">02</span><h3>Подключите API-ключ</h3><p>Владелец хоста получает ключ из <code>.local/api-key.txt</code>. Введите его в окне подключения. Ключ не сохраняется в браузерном хранилище.</p><button class="text-button" onclick={connect}>Открыть подключение <Icon name="arrow" size={16} /></button></section></div>
<section class="panel code-panel"><div class="panel-head"><div><h3>Получить каталог моделей</h3><p>Пример для PowerShell на самом сервере</p></div><button class="secondary compact" onclick={copy}><Icon name="copy" size={16} />{copied ? 'Скопировано' : 'Копировать'}</button></div><pre><code>{catalogExample}</code></pre>{#if copyError}<p role="status" class="muted">Не удалось скопировать. Выделите команду вручную.</p>{/if}</section>
<section class="panel prose"><div class="panel-head"><h3>Что поддерживается</h3><span class="badge neutral">Текущий API</span></div><div class="endpoint-list"><div><code>GET /healthz</code><span>Состояние шлюза без ключа</span></div><div><code>GET /v1/models</code><span>Каталог с Bearer-ключом</span></div><div><code>POST /v1/chat/completions</code><span>Текстовые сообщения, JSON или SSE; ответы mock</span></div></div><div class="subtle-note"><Icon name="info" size={18} /><span>При занятом единственном канале chat-запрос получает 429. Очередь, реальная генерация, аккаунты и платежи ещё не реализованы.</span></div></section>
<section class="privacy-note"><Icon name="info" size={22} /><div><strong>О приватности будущих запросов</strong><p>При подключении реальной ноды текст запросов будет доступен хосту, выполняющему генерацию. Владелец этого пилота — truekant. Не передавайте данные, которые не готовы раскрыть владельцу хоста.</p></div></section>
