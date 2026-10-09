<script lang="ts">
  import Icon from './Icon.svelte';
  import { messages, type Language } from '$lib/i18n';
  import type { Pilot } from '../state/pilot.svelte';
  let { pilot, navigate, lang }: { pilot: Pilot; navigate: (page: string) => void; lang: Language } = $props();
  const text = $derived(messages[lang].overview);
  const successful = $derived(pilot.samples.filter((sample) => sample.online));
  const points = $derived.by(() => {
    if (successful.length < 2) return '';
    const max = Math.max(...successful.map((sample) => sample.latency), 1);
    return successful.map((sample, i) => `${i / (successful.length - 1) * 600},${100 - sample.latency / max * 75}`).join(' ');
  });
  const time = (value: number) => new Date(value).toLocaleTimeString(lang === 'ru' ? 'ru-RU' : 'en-US', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
</script>

<section class="hero">
  <div class="hero-copy">
    <span class="eyebrow"><span class="tiny-dot"></span> {text.heroEyebrow}</span>
    <h2>{text.headlineOne}<br /><span>{text.headlineTwo}</span></h2>
    <p>{text.intro}</p>
    <button class="primary" onclick={() => navigate('nodes')}>{text.seeNode} <Icon name="arrow" size={17} /></button>
  </div>
  <div class="hero-counter"><strong>01</strong><span>{text.channel}</span><small>{messages[lang].shell.hostRam}</small></div>
</section>

<div class="metric-grid">
  <article class="metric"><span class="metric-label">{text.gateway} <Icon name="activity" size={17} /></span><strong class:success-text={pilot.status === 'online'}>{text[pilot.status]}</strong><small>{pilot.status === 'online' ? text.confirmed : text.lastCheck}</small></article>
  <article class="metric"><span class="metric-label">{text.gatewayLatency} <Icon name="bolt" size={17} /></span><strong>{pilot.latency ?? '—'} <em>{lang === 'ru' ? 'мс' : 'ms'}</em></strong><small>{text.httpNotModel}</small></article>
  <article class="metric"><span class="metric-label">{text.channels} <Icon name="layers" size={17} /></span><strong>01 <em>/ 01</em></strong><small>{text.configuredCapacity}</small></article>
  <article class="metric"><span class="metric-label">{text.hostMemory} <Icon name="chip" size={17} /></span><strong>64 <em>{lang === 'ru' ? 'ГБ' : 'GB'}</em></strong><small>{text.ownerReported}</small></article>
</div>

<div class="two-columns overview-lower">
  <section class="panel">
    <div class="panel-head"><div><h3>{text.yourNode}</h3><p>{text.oneExecutor}</p></div><button class="text-button" onclick={() => navigate('nodes')}>{text.details} <Icon name="arrow" size={16} /></button></div>
    <div class="node-row"><div class="square-icon"><Icon name="server" size={23} /></div><div class="node-name"><strong>Ultra · truekant</strong><span>{text.futureNode}</span></div><span class="badge neutral">{text.modelMissing}</span></div>
    <div class="node-specs"><span><b>64 {lang === 'ru' ? 'ГБ' : 'GB'}</b> RAM</span><span><b>Intel</b> Ultra 5</span><span><b>1</b> {lang === 'ru' ? 'канал' : 'channel'}</span></div>
    <div class="subtle-note"><Icon name="info" size={17} /><span>{text.mockNote}</span></div>
  </section>
  <section class="panel">
    <div class="panel-head"><div><h3>{text.latencyChart}</h3><p>{text.liveChecks}</p></div><span class="graph-legend"><span class="tiny-dot"></span> API</span></div>
    <div class="chart">
      {#if points}
        <svg viewBox="0 0 600 120" preserveAspectRatio="none" role="img" aria-label={text.chartAlt}><path class="chart-guide" d="M0 25h600M0 65h600M0 105h600" /><polygon points={`0,120 ${points} 600,120`} fill="url(#chart-fill)" /><polyline points={points} fill="none" stroke="#4567fa" stroke-width="2.5" vector-effect="non-scaling-stroke" /><defs><linearGradient id="chart-fill" x1="0" y1="0" x2="0" y2="1"><stop stop-color="#4567fa" stop-opacity=".13" /><stop offset="1" stop-color="#4567fa" stop-opacity="0" /></linearGradient></defs></svg>
      {:else}<div class="chart-empty"><Icon name="activity" size={28} /><span>{text.collecting}</span><small>{text.chartAfterTwo}</small></div>{/if}
    </div>
    <details class="measurement-list"><summary>{text.measurements} · {pilot.samples.length}</summary><table><thead><tr><th>{text.time}</th><th>{text.result}</th></tr></thead><tbody>{#each pilot.samples as sample}<tr><td>{time(sample.time)}</td><td>{sample.online ? `${sample.latency} ${lang === 'ru' ? 'мс' : 'ms'}` : text.noConnection}</td></tr>{/each}</tbody></table></details>
  </section>
</div>

<section class="panel journey">
  <div class="panel-head"><div><h3>{text.projectPath}</h3><p>{text.projectStatus}</p></div><span class="badge blue">{text.stage1}</span></div>
  <div class="steps"><div class="step complete"><span class="step-number"><Icon name="check" size={17} /></span><div><strong>{text.gatewayAndUi}</strong><small>{text.apiCatalogHealth}</small></div></div><div class="step"><span class="step-number">02</span><div><strong>{text.modelOnUltra}</strong><small>{text.measureCapacity}</small></div></div><div class="step"><span class="step-number">03</span><div><strong>{text.fullRequest}</strong><small>{text.nodeChannelClient}</small></div></div></div>
</section>
