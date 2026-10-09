<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  let { pilot, connect }: { pilot: Pilot; connect: () => void } = $props();
  const number = (value: number) => value.toLocaleString('ru-RU');
</script>

<div class="section-intro"><div><span class="eyebrow muted">КАТАЛОГ API</span><h2>Модели вашего шлюза</h2><p>Список загружается с сервера после проверки API-ключа.</p></div>{#if pilot.connected}<button class="secondary" disabled={pilot.catalog === 'loading'} onclick={() => pilot.refreshCatalog()}><Icon name="refresh" size={17} /> Обновить каталог</button>{/if}</div>
{#if pilot.catalog === 'loading'}
  <section class="panel empty-state" aria-live="polite"><span class="spinner"></span><h3>Загружаем каталог</h3><p>Проверяем ключ и ответ шлюза.</p></section>
{:else if pilot.catalog === 'error'}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="info" size={32} /></div><h3>Не удалось подключиться</h3><p role="alert">{pilot.catalogError}</p><button class="primary" onclick={connect}>Проверить ключ <Icon name="arrow" size={17} /></button></section>
{:else if !pilot.connected}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="key" size={32} /></div><span class="eyebrow muted">ЗАЩИЩЁННЫЙ КАТАЛОГ</span><h3>Подключите API-ключ</h3><p>Шлюз проверит ключ и вернёт доступные модели.<br />Ключ остаётся в памяти этой вкладки до отключения или перезагрузки.</p><button class="primary" onclick={connect}>Подключить шлюз <Icon name="arrow" size={17} /></button></section>
{:else if pilot.models.length === 0}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="layers" size={32} /></div><h3>Каталог пока пуст</h3><p>Шлюз принял ключ, но ещё не опубликовал ни одной модели.</p></section>
{:else}
  <div class="model-grid">{#each pilot.models as model (model.id)}<article class="panel model-card"><div class="model-top"><div class="model-icon"><Icon name="layers" size={26} /></div><span class="badge amber">MOCK</span></div><span class="eyebrow muted">МОДЕЛЬ ИЗ API</span><h3>{model.id}</h3><p>Демонстрационный исполнитель для проверки подключения. Настоящая AI-модель ещё не загружена.</p><dl><div><dt>Контекст каталога</dt><dd>{number(model.context_tokens)}</dd></div><div><dt>Макс. длина ответа</dt><dd>{number(model.max_output_tokens)}</dd></div><div><dt>Статус записи API</dt><dd>{model.availability === 'ready' ? 'Готова' : model.availability}</dd></div></dl><div class="subtle-note"><Icon name="info" size={17} /><span>Счётчики mock-ответов синтетические; списания не выполняются.</span></div></article>{/each}</div>
{/if}
