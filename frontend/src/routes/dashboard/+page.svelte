<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { api, ApiFailure, type Account, type UsageRow } from '$lib/api/client';

  let account: Account | null = null;
  let rows: UsageRow[] = [];
  let loading = true;
  let refreshing = false;
  let error = '';
  let keyName = 'M1 test key';
  let keyPending = false;
  let keyError = '';
  let revealedSecret = '';
  let copied = false;
  let interval: ReturnType<typeof setInterval>;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let invalidationTimer: ReturnType<typeof setTimeout> | undefined;
  let liveSocket: WebSocket | null = null;
  let liveEventID: string | null = null;
  let liveSessionID: string | null = null;
  let clientSequence = 0;
  let serverSequence = 0;
  let mounted = true;

  async function refresh() {
    if (refreshing || !mounted) return;
    refreshing = true;
    try { rows = await api.usage(); error = ''; }
    catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 401) { await goto('/login?next=%2Fdashboard'); return; }
      error = 'Usage metadata is temporarily unavailable.';
    } finally { refreshing = false; loading = false; }
  }

  async function createKey(event: SubmitEvent) {
    event.preventDefault(); keyPending = true; keyError = ''; copied = false;
    try {
      const result = await api.createKey({name:keyName,permissions:['chat:write','models:read','usage:read'],model_ids:['cordova-chat-small-v1'],daily_spend_cap_piconero:'0'});
      if (typeof result !== 'object' || result === null || !('secret' in result) || typeof result.secret !== 'string') throw new Error('invalid key response');
      revealedSecret = result.secret;
    } catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 403) keyError = 'Accept the current privacy notice before creating a key.';
      else keyError = 'The test API key could not be created. Try again later.';
    } finally { keyPending = false; }
  }

  async function copySecret() {
    if (!revealedSecret) return;
    try { await navigator.clipboard.writeText(revealedSecret); copied = true; }
    catch { keyError = 'Clipboard access is unavailable. Select and copy the key manually.'; }
  }

  function clearSecret() { revealedSecret = ''; copied = false; }

  function sendLive(type: string, payload: unknown, requestID?: string | null) {
    if (!liveSocket || liveSocket.readyState !== WebSocket.OPEN || clientSequence >= 0xffffffff) return;
    const messageID = crypto.randomUUID();
    clientSequence += 1;
    liveSocket.send(JSON.stringify({
      v: 1, type, message_id: messageID, request_id: requestID === undefined ? messageID : requestID,
      session_id: liveSessionID, seq: clientSequence, sent_at: new Date().toISOString(), payload, error: null
    }));
  }

  async function connectLive() {
    if (!mounted || !account || (liveSocket && liveSocket.readyState < WebSocket.CLOSING)) return;
    try {
      const { ticket } = await api.liveTicket();
      if (!mounted) return;
      const socket = new WebSocket(api.liveURL());
      liveSocket = socket;
      clientSequence = 0;
      serverSequence = 1;
      liveSessionID = null;
      socket.onopen = () => sendLive('live.auth', { ticket }, crypto.randomUUID());
      socket.onmessage = (message) => {
        if (typeof message.data !== 'string' || message.data.length > 1_048_576) { socket.close(1009, 'invalid frame'); return; }
        let frame: unknown;
        try { frame = JSON.parse(message.data); } catch { socket.close(1002, 'invalid frame'); return; }
        if (typeof frame !== 'object' || frame === null || !('v' in frame) || !('type' in frame) || !('seq' in frame) ||
          frame.v !== 1 || !Number.isSafeInteger(frame.seq) || frame.seq !== serverSequence || typeof frame.type !== 'string') {
          socket.close(1002, 'invalid frame'); return;
        }
        serverSequence += 1;
        if (frame.type === 'live.ready' && 'payload' in frame && typeof frame.payload === 'object' && frame.payload !== null && 'session_id' in frame.payload && typeof frame.payload.session_id === 'string') {
          liveSessionID = frame.payload.session_id;
          sendLive('live.subscribe', { topics: ['usage'], last_event_id: liveEventID });
        } else if (frame.type === 'live.ping' && 'request_id' in frame) {
          sendLive('live.pong', { time: new Date().toISOString() }, typeof frame.request_id === 'string' ? frame.request_id : null);
        } else if (frame.type === 'live.event' && 'payload' in frame && typeof frame.payload === 'object' && frame.payload !== null &&
          'topic' in frame.payload && frame.payload.topic === 'usage' && 'event_id' in frame.payload && typeof frame.payload.event_id === 'string') {
          liveEventID = frame.payload.event_id;
          if (invalidationTimer) clearTimeout(invalidationTimer);
          invalidationTimer = setTimeout(() => { void refresh(); }, 250);
        } else if (frame.type === 'live.resync') {
          liveEventID = null;
          void refresh();
        } else if (frame.type !== 'live.pong') {
          socket.close(1008, 'unsupported frame');
        }
      };
      socket.onclose = () => {
        if (liveSocket === socket) { liveSocket = null; liveSessionID = null; }
        if (mounted) {
          reconnectTimer = setTimeout(() => { void connectLive(); }, 1500 + Math.random() * 1500);
        }
      };
    } catch (cause) {
      if (cause instanceof ApiFailure && cause.status === 401) { await goto('/login?next=%2Fdashboard'); return; }
      if (mounted) reconnectTimer = setTimeout(() => { void connectLive(); }, 3000);
    }
  }

  async function logout() {
    liveSocket?.close(1000, 'logout');
    liveSocket = null;
    try { await api.logout(); } catch { /* The server session may already have expired. */ }
    rows = []; account = null; clearSecret(); await goto('/login');
  }

  onMount(() => {
    mounted = true;
    void (async () => {
      try { const session = await api.session(); account = session.account; await refresh(); void connectLive(); }
      catch { await goto('/login?next=%2Fdashboard'); }
    })();
    interval = setInterval(() => { if (document.visibilityState === 'visible') void refresh(); }, 30000);
    const visibility = () => { if (document.visibilityState === 'visible') void refresh(); };
    document.addEventListener('visibilitychange', visibility);
    return () => {
      mounted = false;
      clearInterval(interval);
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (invalidationTimer) clearTimeout(invalidationTimer);
      document.removeEventListener('visibilitychange', visibility);
      liveSocket?.close(1000, 'page closed');
      liveSocket = null;
      clearSecret();
    };
  });
</script>

<svelte:head><title>Dashboard · Cordova</title></svelte:head>

<section aria-labelledby="dashboard-title">
  <div class="account-row">
    <div><p class="eyebrow">Account dashboard</p><h1 id="dashboard-title">{account ? `Welcome, ${account.username}` : 'Your account'}</h1></div>
    <button class="button secondary" onclick={logout}>Sign out</button>
  </div>
  <div class="notice" role="note"><strong>Test credits only</strong>Requests use a local mock node and synthetic word counts. Every displayed charge is server-reported as zero; no payment or real model inference is active.</div>

  <section class="card" aria-labelledby="key-title">
    <div class="account-row"><div><h2 id="key-title">Test API key</h2><p class="subtle">Create a key to call the OpenAI-compatible endpoint. The secret is shown once and kept only in this view.</p></div></div>
    {#if revealedSecret}
      <div class="success" role="status">Copy this key now. It will disappear when you close this panel or leave the page.</div>
      <label class="field">New API key
        <input class="code" value={revealedSecret} readonly aria-label="New API key secret" onfocus={(event) => event.currentTarget.select()} />
      </label>
      <div class="actions" style="margin-top: 12px"><button class="button" onclick={copySecret}>{copied ? 'Copied' : 'Copy key'}</button><button class="button secondary" onclick={clearSecret}>Close key</button></div>
    {:else}
      <form class="actions" onsubmit={createKey}>
        <label class="field" style="flex:1; min-width:220px; margin:0">Key name
          <input bind:value={keyName} maxlength="64" required />
        </label>
        <button class="button" type="submit" disabled={keyPending}>{keyPending ? 'Creating…' : 'Create test key'}</button>
      </form>
    {/if}
    {#if keyError}<p class="error" role="alert">{keyError}</p>{/if}
  </section>

  <section class="card" aria-labelledby="usage-title">
    <div class="account-row"><div><h2 id="usage-title">Recent usage</h2><p class="subtle">Metadata arrives from the Kafka account projection. Prompts and generated text are not shown here.</p></div><button class="button secondary" onclick={() => void refresh()} disabled={refreshing}>Refresh</button></div>
    {#if error}<p class="error" role="alert">{error}</p>{/if}
    {#if loading}<p class="subtle" style="margin-top:18px">Loading usage…</p>
    {:else if rows.length === 0}<p class="subtle" style="margin-top:18px">No requests yet. Use your test API key with <a href="/docs/api" style="color:var(--accent)">the API guide</a>; completed requests will appear here.</p>
    {:else}
      <div class="table-wrap"><table>
        <caption class="subtle" style="text-align:left; padding:0 12px 10px">Usage metadata. Token fields are synthetic word counts in this mock milestone.</caption>
        <thead><tr><th scope="col">Request</th><th scope="col">Model</th><th scope="col">Input</th><th scope="col">Output</th><th scope="col">Status</th><th scope="col">Charge</th><th scope="col">Time (UTC)</th></tr></thead>
        <tbody>{#each rows as row (row.request_id)}<tr><td class="code">{row.request_id.slice(0, 8)}…</td><td>{row.model_id}</td><td>{row.input_tokens}</td><td>{row.output_tokens}</td><td>{row.status}</td><td class="credits">{row.charged_piconero} piconero</td><td>{new Date(row.created_at).toLocaleString(undefined, {timeZone:'UTC',timeZoneName:'short'})}</td></tr>{/each}</tbody>
      </table></div>
    {/if}
  </section>
</section>
