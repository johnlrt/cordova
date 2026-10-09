<script lang="ts">
  import { goto } from '$app/navigation';
  import { api, ApiFailure, safeReturnPath } from '$lib/api/client';

  let username = '';
  let password = '';
  let pending = false;
  let error = '';

  async function submit(event: SubmitEvent) {
    event.preventDefault(); error = ''; pending = true;
    try {
      await api.login({ username, password });
      const next = safeReturnPath(new URLSearchParams(window.location.search).get('next'));
      await goto(next);
    } catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 429) error = 'Too many attempts. Try again in a minute.';
      else error = 'Username or password is incorrect.';
    } finally { pending = false; }
  }
</script>

<svelte:head><title>Sign in · Cordova</title></svelte:head>

<section class="narrow" aria-labelledby="login-title">
  <p class="eyebrow">Welcome back</p>
  <h1 id="login-title">Sign in</h1>
  <form class="card" onsubmit={submit}>
    <label class="field">Username
      <input bind:value={username} name="username" autocomplete="username" minlength="3" maxlength="64" required />
    </label>
    <label class="field">Password
      <input bind:value={password} name="password" type="password" autocomplete="current-password" required />
    </label>
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    <button class="button" type="submit" disabled={pending}>{pending ? 'Signing in…' : 'Sign in'}</button>
    <div class="inline-links"><a href="/register">Create an account</a><a href="/recover">Use a recovery code</a></div>
  </form>
</section>
