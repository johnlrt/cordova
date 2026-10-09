<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  let { pilot }: { pilot: Pilot } = $props();
  let dialog: HTMLDialogElement;
  let draft = $state('');
  let validation = $state('');
  let submitting = $state(false);
  let reveal = $state(false);
  let generation = 0;
  export function open() { generation++; submitting = false; draft = ''; validation = ''; reveal = false; dialog.showModal(); }
  function close() { draft = ''; reveal = false; validation = ''; dialog.close(); }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (draft.trim().length < 32 || draft.trim().length > 4096) {
      validation = 'Ключ должен содержать от 32 до 4096 символов.'; return;
    }
    submitting = true;
    const attempt = ++generation;
    validation = '';
    try {
      await pilot.connect(draft);
      if (attempt !== generation) return;
      draft = '';
      if (pilot.connected) close();
      else validation = pilot.catalogError;
    } finally { if (attempt === generation) submitting = false; }
  }
  function cancel() { generation++; if (submitting) pilot.disconnect(); submitting = false; close(); }
</script>

<dialog bind:this={dialog} aria-labelledby="connection-title" oncancel={(event) => { event.preventDefault(); cancel(); }}>
  <div class="dialog-heading"><div class="empty-icon small"><Icon name="link" size={24} /></div><button class="icon-button" aria-label="Закрыть подключение" onclick={cancel}><Icon name="close" /></button></div>
  <h2 id="connection-title">Подключение к шлюзу</h2><p class="dialog-description">Введите API-ключ, выданный владельцем хоста Ultra. Он нужен для загрузки каталога моделей.</p>
  <form onsubmit={submit}><label for="api-key">API-ключ</label><div class="key-input"><input id="api-key" type={reveal ? 'text' : 'password'} bind:value={draft} autocomplete="off" spellcheck="false" placeholder="cvk_demo_…" maxlength="4096" required disabled={submitting} /><button type="button" class="text-button" onclick={() => reveal = !reveal} aria-pressed={reveal}>{reveal ? 'Скрыть' : 'Показать'}</button></div>{#if validation}<p class="form-error" role="alert">{validation}</p>{/if}<div class="subtle-note"><Icon name="info" size={18} /><span>Ключ хранится только в памяти вкладки. Перезагрузка или отключение удалит его.</span></div><div class="dialog-actions"><button type="button" class="secondary" onclick={cancel}>Отмена</button><button type="submit" class="primary" disabled={submitting}>{submitting ? 'Подключаем…' : 'Подключить'}<Icon name="arrow" size={17} /></button></div></form>
</dialog>
