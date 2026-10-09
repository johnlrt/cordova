<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  import { apiErrorMessage } from '../api/client';
  import { messages, type Language } from '$lib/i18n';
  let { pilot, lang }: { pilot: Pilot; lang: Language } = $props();
  const text = $derived(messages[lang].dialog);
  let dialog: HTMLDialogElement;
  let draft = $state('');
  let validation = $state<'key_length' | ''>('');
  let submitting = $state(false);
  let reveal = $state(false);
  let generation = 0;
  const formError = $derived(validation === 'key_length'
    ? text.invalidKey
    : pilot.catalogError ? apiErrorMessage(pilot.catalogError.code, pilot.catalogError.status, lang) : '');
  export function open() { generation++; submitting = false; draft = ''; validation = ''; reveal = false; dialog.showModal(); }
  function close() { draft = ''; reveal = false; validation = ''; dialog.close(); }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (draft.trim().length < 32 || draft.trim().length > 4096) {
      validation = 'key_length'; return;
    }
    submitting = true;
    const attempt = ++generation;
    validation = '';
    try {
      await pilot.connect(draft);
      if (attempt !== generation) return;
      draft = '';
      if (pilot.connected) close();
    } finally { if (attempt === generation) submitting = false; }
  }
  function cancel() { generation++; if (submitting) pilot.disconnect(); submitting = false; close(); }
</script>

<dialog bind:this={dialog} aria-labelledby="connection-title" oncancel={(event) => { event.preventDefault(); cancel(); }}>
  <div class="dialog-heading"><div class="empty-icon small"><Icon name="link" size={24} /></div><button class="icon-button" aria-label={text.close} onclick={cancel}><Icon name="close" /></button></div>
  <h2 id="connection-title">{text.title}</h2><p class="dialog-description">{text.intro}</p>
  <form onsubmit={submit}><label for="api-key">{text.key}</label><div class="key-input"><input id="api-key" type={reveal ? 'text' : 'password'} bind:value={draft} autocomplete="off" spellcheck="false" placeholder="cvk_demo_…" maxlength="4096" required disabled={submitting} /><button type="button" class="text-button" onclick={() => reveal = !reveal} aria-pressed={reveal}>{reveal ? text.hide : text.show}</button></div>{#if formError}<p class="form-error" role="alert">{formError}</p>{/if}<div class="subtle-note"><Icon name="info" size={18} /><span>{text.memory}</span></div><div class="dialog-actions"><button type="button" class="secondary" onclick={cancel}>{text.cancel}</button><button type="submit" class="primary" disabled={submitting}>{submitting ? text.connecting : text.connect}<Icon name="arrow" size={17} /></button></div></form>
</dialog>
