<script lang="ts">
  import Icon from './Icon.svelte';
  import { messages, type Language } from '$lib/i18n';
  let { connect, lang }: { connect: () => void; lang: Language } = $props();
  const text = $derived(messages[lang].docs);
  const catalogExample = 'curl.exe http://127.0.0.1:8080/v1/models -H "Authorization: Bearer YOUR_API_KEY"';
  let copied = $state(false);
  let copyError = $state(false);
  async function copy() {
    try { await navigator.clipboard.writeText(catalogExample); copied = true; copyError = false; }
    catch { copyError = true; }
  }
</script>

<div class="section-intro"><div><span class="eyebrow muted">{text.eyebrow}</span><h2>{text.heading}</h2><p>{text.intro}</p></div><span class="badge blue">API v1 · mock</span></div>
<div class="two-columns docs-grid"><section class="panel prose"><span class="doc-number">01</span><h3>{text.checkConnection}</h3><p>{text.runGateway} <code>GET /healthz</code>.</p><p>{text.localAddress} <code>127.0.0.1</code> {text.belongsToComputer}</p></section><section class="panel prose"><span class="doc-number">02</span><h3>{text.connectKey}</h3><p>{text.obtainKey} <code>.local/api-key.txt</code>. {text.enterDialog}</p><button class="text-button" onclick={connect}>{text.openConnection} <Icon name="arrow" size={16} /></button></section></div>
<section class="panel code-panel"><div class="panel-head"><div><h3>{text.getCatalog}</h3><p>{text.powershell}</p></div><button class="secondary compact" onclick={copy}><Icon name="copy" size={16} /> {copied ? text.copied : text.copy}</button></div><pre><code>{catalogExample}</code></pre>{#if copyError}<p role="status" class="muted">{text.copyError}</p>{/if}</section>
<section class="panel prose"><div class="panel-head"><h3>{text.supported}</h3><span class="badge neutral">{text.currentApi}</span></div><div class="endpoint-list"><div><code>GET /healthz</code><span>{text.health}</span></div><div><code>GET /v1/models</code><span>{text.catalog}</span></div><div><code>POST /v1/chat/completions</code><span>{text.chat}</span></div></div><div class="subtle-note"><Icon name="info" size={18} /><span>{text.busy}</span></div></section>
<section class="privacy-note"><Icon name="info" size={22} /><div><strong>{text.privacy}</strong><p>{text.privacyInfo}</p></div></section>
