<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  import { messages, type Language } from '$lib/i18n';
  let { pilot, lang }: { pilot: Pilot; lang: Language } = $props();
  const text = $derived(messages[lang].nodes);
</script>

<div class="section-intro"><div><span class="eyebrow muted">{text.eyebrow}</span><h2>{text.heading}</h2><p>{text.intro}</p></div><span class="badge neutral">{text.teamConfig}</span></div>
<section class="panel node-detail">
  <div class="panel-head"><div class="node-title"><div class="square-icon large"><Icon name="server" size={30} /></div><div><h3>Ultra · truekant</h3><p>{text.mainHost}</p></div></div><span class="badge" class:green={pilot.status === 'online'} class:neutral={pilot.status !== 'online'}>{pilot.status === 'online' ? text.gatewayOnline : text.gatewayUnconfirmed}</span></div>
  <div class="spec-grid"><div><span>{text.processor}</span><strong>Intel Ultra 5 125</strong></div><div><span>{text.memory}</span><strong>64 GB DDR5</strong></div><div><span>{text.graphics}</span><strong>Intel graphics + NPU</strong></div><div><span>{text.dataSource}</span><strong>{text.owner}</strong></div></div>
  <div class="node-state-grid"><article><Icon name="globe" size={22} /><h4>{text.gateway}</h4><p>{pilot.status === 'online' ? text.healthOk : text.waitHealth}</p></article><article><Icon name="chip" size={22} /><h4>{text.model}</h4><p>{text.modelUnconnected}</p></article><article><Icon name="layers" size={22} /><h4>{text.channel}</h4><p>{text.singleLimit}</p></article></div>
</section>
<div class="two-columns">
  <section class="panel prose"><div class="panel-head"><h3>{text.requestFlow}</h3><Icon name="arrow" /></div><ol class="flow-list"><li><span>01</span><div><strong>{text.client}</strong><p>{text.clientInfo}</p></div></li><li><span>02</span><div><strong>{text.goGateway}</strong><p>{text.gatewayInfo}</p></div></li><li><span>03</span><div><strong>{text.localNode}</strong><p>{text.nextGeneration}</p></div></li></ol></section>
  <section class="panel prose"><div class="panel-head"><h3>{text.measure}</h3><Icon name="activity" /></div><p>{text.measureIntro}</p><ul class="checklist"><li>{text.memoryRun}</li><li>{text.speed}</li><li>{text.accelerator}</li><li>{text.cancel}</li></ul><div class="subtle-note"><Icon name="info" size={18} /><span>{text.ramNote}</span></div></section>
</div>
