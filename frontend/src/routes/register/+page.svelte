<script lang="ts">
  import { goto } from '$app/navigation';
  import { api, ApiFailure, type SessionView } from '$lib/api/client';

  let username = '';
  let password = '';
  let acknowledged = false;
  let pending = false;
  let session: SessionView | null = null;
  let recoverySaved = false;
  let error = '';

  async function submit(event: SubmitEvent) {
    event.preventDefault(); error = ''; pending = true;
    try { session = await api.register({ username, password, privacy_notice_version: 'v1' }); }
    catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 409) error = 'An account with that username already exists.';
      else if (cause instanceof ApiFailure && cause.status === 429) error = 'Too many attempts. Try again in a minute.';
      else if (cause instanceof ApiFailure && cause.status === 400) error = 'Check the username, password length, and privacy notice acknowledgement.';
      else error = 'The account service is unavailable. Try again later.';
    } finally { pending = false; }
  }
</script>

<svelte:head><title>Create an account · Cordova</title></svelte:head>

<section class="narrow" aria-labelledby="register-title">
  <p class="eyebrow">Account setup</p>
  <h1 id="register-title">Create your account</h1>
  <p class="subtle">Use a username and a password with at least 12 characters. Email and phone are not required.</p>

  {#if session}
    <div class="card" aria-live="polite">
      <h2>Save these recovery codes</h2>
      <p class="subtle">Each code works once. This is the only time these codes are shown. Keep them somewhere private.</p>
      <div class="recovery-codes">{#each session.recovery_codes ?? [] as code}<code>{code}</code>{/each}</div>
      <label class="check"><input type="checkbox" bind:checked={recoverySaved} /> I saved these recovery codes somewhere private.</label>
      <button class="button" disabled={!recoverySaved} onclick={() => goto('/dashboard')}>Continue to dashboard</button>
    </div>
  {:else}
    <form class="card" onsubmit={submit}>
      <label class="field">Username
        <input bind:value={username} name="username" autocomplete="username" minlength="3" maxlength="64" required />
      </label>
      <label class="field">Password
        <input bind:value={password} name="password" type="password" autocomplete="new-password" minlength="12" maxlength="128" required />
      </label>
      <label class="check"><input type="checkbox" bind:checked={acknowledged} required /> I understand the M1 mock flow uses synthetic output, stores only usage metadata, and does not provide real credits or a real model.</label>
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      <button class="button" type="submit" disabled={pending || !acknowledged}>{pending ? 'Creating account…' : 'Create account'}</button>
      <p class="subtle" style="margin-top: 16px">Already registered? <a href="/login" style="color: var(--accent)">Sign in</a></p>
    </form>
  {/if}
</section>
