<svelte:head><title>API guide · Cordova</title></svelte:head>

<section>
  <p class="eyebrow">API guide</p>
  <h1>Make a mock chat request</h1>
  <p class="lead">Create a test API key from your dashboard, then send a Chat Completions request to the local HTTPS edge.</p>
  <div class="notice"><strong>Test data only</strong>Response text and usage counts come from a mock node. Input and output counts are word counts, not model tokens. Charges are always zero; there are no deposits or payouts.</div>

  <section class="card"><h2>List available models</h2><pre class="code-block">curl --cacert .local/dev-ca.crt \
  -H "Authorization: Bearer $CORDOVA_API_KEY" \
  https://localhost:8443/v1/models</pre></section>

  <section class="card"><h2>Stream a completion</h2><pre class="code-block">curl --cacert .local/dev-ca.crt -N \
  -H "Authorization: Bearer $CORDOVA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '&#123;"model":"cordova-chat-small-v1","messages":[&#123;"role":"user","content":"Hello"&#125;],"stream":true,"stream_options":&#123;"include_usage":true&#125;&#125;' \
  https://localhost:8443/v1/chat/completions</pre>
    <p class="subtle" style="margin-top:14px">A successful stream ends with <code>data: [DONE]</code>. A stream error closes without that marker. The final usage chunk is synthetic and not billable.</p>
  </section>

  <section class="card"><h2>Supported request fields</h2><p class="subtle">The mock accepts system, user and assistant text messages; <code>max_tokens</code> from 1 to 2,048; <code>temperature</code>; <code>top_p</code>; up to four stop strings; <code>seed</code>; and <code>stream_options.include_usage</code>. Tool calls and multimodal content are not supported. Unknown fields are rejected.</p></section>

  <section class="card"><h2>Account usage</h2><p class="subtle">The dashboard shows the same completed usage metadata after Kafka projection. It does not show prompts or response text. Request metadata is stored without content for the M1 demo; no charge is recorded.</p></section>
</section>

<style>
  .code-block { overflow-x: auto; white-space: pre; margin: 16px 0 0; padding: 16px; border: 1px solid var(--border); border-radius: 8px; background: #0d1218; color: #d7e4f5; line-height: 1.7; font: 12px/1.7 ui-monospace, SFMono-Regular, Menlo, monospace; }
  code { color: #c8d9f4; }
</style>
