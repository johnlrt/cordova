<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  import { messages, type Language } from '$lib/i18n';
  import { apiErrorMessage } from '../api/client';
  let { pilot, connect, lang }: { pilot: Pilot; connect: () => void; lang: Language } = $props();
  const text = $derived(messages[lang].models);
  const number = (value: number) => value.toLocaleString(lang === 'ru' ? 'ru-RU' : 'en-US');
</script>

<div class="section-intro"><div><span class="eyebrow muted">{text.eyebrow}</span><h2>{text.heading}</h2><p>{text.intro}</p></div>{#if pilot.connected}<button class="secondary" disabled={pilot.catalog === 'loading'} onclick={() => pilot.refreshCatalog()}><Icon name="refresh" size={17} /> {text.refresh}</button>{/if}</div>
{#if pilot.catalog === 'loading'}
  <section class="panel empty-state" aria-live="polite"><span class="spinner"></span><h3>{text.loading}</h3><p>{text.checking}</p></section>
{:else if pilot.catalog === 'error'}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="info" size={32} /></div><h3>{text.connectionFailed}</h3><p role="alert">{pilot.catalogError ? apiErrorMessage(pilot.catalogError.code, pilot.catalogError.status, lang) : text.connectionFailed}</p><button class="primary" onclick={connect}>{text.checkKey} <Icon name="arrow" size={17} /></button></section>
{:else if !pilot.connected}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="key" size={32} /></div><span class="eyebrow muted">{text.protected}</span><h3>{text.connectKey}</h3><p>{text.keyInfo}<br />{text.keyMemory}</p><button class="primary" onclick={connect}>{text.connectGateway} <Icon name="arrow" size={17} /></button></section>
{:else if pilot.models.length === 0}
  <section class="panel empty-state"><div class="empty-icon"><Icon name="layers" size={32} /></div><h3>{text.emptyTitle}</h3><p>{text.emptyInfo}</p></section>
{:else}
  <div class="model-grid">{#each pilot.models as model (model.id)}<article class="panel model-card"><div class="model-top"><div class="model-icon"><Icon name="layers" size={26} /></div><span class="badge amber">MOCK</span></div><span class="eyebrow muted">{text.apiModel}</span><h3>{model.id}</h3><p>{text.demo}</p><dl><div><dt>{text.context}</dt><dd>{number(model.context_tokens)}</dd></div><div><dt>{text.maxOutput}</dt><dd>{number(model.max_output_tokens)}</dd></div><div><dt>{text.apiStatus}</dt><dd>{model.availability === 'ready' ? text.ready : model.availability}</dd></div></dl><div class="subtle-note"><Icon name="info" size={17} /><span>{text.synthetic}</span></div></article>{/each}</div>
{/if}
