<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  let { pilot, navigate }: { pilot: Pilot; navigate: (page: string) => void } = $props();
  const successful = $derived(pilot.samples.filter((sample) => sample.online));
  const points = $derived.by(() => {
    if (successful.length < 2) return '';
    const max = Math.max(...successful.map((sample) => sample.latency), 1);
    return successful.map((sample, i) => `${i / (successful.length - 1) * 600},${100 - sample.latency / max * 75}`).join(' ');
  });
  const time = (value: number) => new Date(value).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
</script>

<section class="hero">
  <div class="hero-copy">
    <span class="eyebrow"><span class="tiny-dot"></span> ВАША ЛОКАЛЬНАЯ СЕТЬ</span>
    <h2>Мощность рядом.<br /><span>Всё под контролем.</span></h2>
    <p>Один хост управляет запросами и будет выполнять генерацию. Ваш ноутбук подключается как обычный клиент.</p>
    <button class="primary" onclick={() => navigate('nodes')}>Посмотреть ноду <Icon name="arrow" size={17} /></button>
  </div>
  <div class="hero-counter"><strong>01</strong><span>КАНАЛ ГЕНЕРАЦИИ</span><small>Ultra · 64 ГБ RAM</small></div>
</section>

<div class="metric-grid">
  <article class="metric"><span class="metric-label">Связь со шлюзом <Icon name="activity" size={17} /></span><strong class:success-text={pilot.status === 'online'}>{pilot.status === 'online' ? 'На связи' : pilot.status === 'checking' ? 'Проверка…' : 'Нет связи'}</strong><small>{pilot.status === 'online' ? 'Подтверждено через API' : 'Последняя проверка соединения'}</small></article>
  <article class="metric"><span class="metric-label">Отклик шлюза <Icon name="bolt" size={17} /></span><strong>{pilot.latency ?? '—'} <em>мс</em></strong><small>HTTP-задержка, не скорость модели</small></article>
  <article class="metric"><span class="metric-label">Каналы генерации <Icon name="layers" size={17} /></span><strong>01 <em>/ 01</em></strong><small>Настроенная ёмкость пилота</small></article>
  <article class="metric"><span class="metric-label">Память хоста <Icon name="chip" size={17} /></span><strong>64 <em>ГБ</em></strong><small>Заявлено владельцем ноды</small></article>
</div>

<div class="two-columns overview-lower">
  <section class="panel">
    <div class="panel-head"><div><h3>Ваша нода</h3><p>Один исполнитель для локального пилота</p></div><button class="text-button" onclick={() => navigate('nodes')}>Подробнее <Icon name="arrow" size={16} /></button></div>
    <div class="node-row"><div class="square-icon"><Icon name="server" size={23} /></div><div class="node-name"><strong>Ultra · truekant</strong><span>Шлюз и будущая AI-нода</span></div><span class="badge neutral">Модель не подключена</span></div>
    <div class="node-specs"><span><b>64 ГБ</b> RAM</span><span><b>Intel</b> Ultra 5</span><span><b>1</b> канал</span></div>
    <div class="subtle-note"><Icon name="info" size={17} /><span>API работает в mock-режиме. Нода и ускорители ещё не проверены.</span></div>
  </section>
  <section class="panel">
    <div class="panel-head"><div><h3>Задержка шлюза</h3><p>Живые проверки этой сессии</p></div><span class="graph-legend"><span class="tiny-dot"></span> API</span></div>
    <div class="chart">
      {#if points}
        <svg viewBox="0 0 600 120" preserveAspectRatio="none" role="img" aria-label="График задержки шлюза; численные значения доступны ниже"><path class="chart-guide" d="M0 25h600M0 65h600M0 105h600" /><polygon points={`0,120 ${points} 600,120`} fill="url(#chart-fill)" /><polyline points={points} fill="none" stroke="#4567fa" stroke-width="2.5" vector-effect="non-scaling-stroke" /><defs><linearGradient id="chart-fill" x1="0" y1="0" x2="0" y2="1"><stop stop-color="#4567fa" stop-opacity=".13" /><stop offset="1" stop-color="#4567fa" stop-opacity="0" /></linearGradient></defs></svg>
      {:else}<div class="chart-empty"><Icon name="activity" size={28} /><span>Собираем измерения</span><small>График появится после двух успешных проверок</small></div>{/if}
    </div>
    <details class="measurement-list"><summary>Измерения · {pilot.samples.length}</summary><table><thead><tr><th>Время</th><th>Результат</th></tr></thead><tbody>{#each pilot.samples as sample}<tr><td>{time(sample.time)}</td><td>{sample.online ? `${sample.latency} мс` : 'Нет связи'}</td></tr>{/each}</tbody></table></details>
  </section>
</div>

<section class="panel journey">
  <div class="panel-head"><div><h3>Путь к настоящему AI</h3><p>Текущее состояние проекта</p></div><span class="badge blue">Этап 1 / API + интерфейс</span></div>
  <div class="steps"><div class="step complete"><span class="step-number"><Icon name="check" size={17} /></span><div><strong>Шлюз и интерфейс</strong><small>API, каталог и проверка связи</small></div></div><div class="step"><span class="step-number">02</span><div><strong>Модель на Ultra</strong><small>Запуск и измерение мощности</small></div></div><div class="step"><span class="step-number">03</span><div><strong>Полный путь запроса</strong><small>Нода, канал и клиент i7</small></div></div></div>
</section>
