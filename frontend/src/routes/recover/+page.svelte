<script lang="ts">
  import { goto } from '$app/navigation';
  import { api, ApiFailure } from '$lib/api/client';

  let username = '';
  let recoveryCode = '';
  let password = '';
  let pending = false;
  let error = '';

  async function submit(event: SubmitEvent) {
    event.preventDefault(); error = ''; pending = true;
    try {
      await api.recover({ username, recovery_code: recoveryCode, new_password: password });
      await goto('/dashboard');
    } catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 429) error = 'Too many attempts. Try again in a minute.';
      else error = 'The username, recovery code, or new password is not valid.';
    } finally { pending = false; }
  }
</script>

<svelte:head><title>Recover account · Cordova</title></svelte:head>

<section class="narrow" aria-labelledby="recover-title">
  <p class="eyebrow">Account recovery</p>
  <h1 id="recover-title">Reset your password</h1>
  <p class="subtle">A recovery code can be used once. Recovery revokes other sessions and applies a 24-hour payout lock.</p>
  <form class="card" onsubmit={submit}>
    <label class="field">Username
      <input bind:value={username} autocomplete="username" minlength="3" maxlength="64" required />
    </label>
    <label class="field">Recovery code
      <input bind:value={recoveryCode} autocomplete="off" spellcheck="false" required />
    </label>
    <label class="field">New password
      <input bind:value={password} type="password" autocomplete="new-password" minlength="12" maxlength="128" required />
    </label>
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    <button class="button" type="submit" disabled={pending}>{pending ? 'Recovering…' : 'Reset password'}</button>
  </form>
</section>
