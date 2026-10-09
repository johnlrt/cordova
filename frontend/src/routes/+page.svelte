<svelte:head>
  <title>Cordova · Community inference, with clear controls</title>
  <meta name="description" content="Account dashboard for the Cordova mock inference network." />
</svelte:head>

<section aria-labelledby="welcome-title">
  <p class="eyebrow">M1 · Mock network</p>
  <h1 id="welcome-title">A small, inspectable inference network.</h1>
  <p class="lead">Create an account, issue a test API key, and follow request usage from the dashboard. This milestone runs a local mock node; it does not load a language model or handle money.</p>
  <div class="notice" role="note">
    <strong>Privacy notice</strong>
    The M1 stack accepts text only for the local mock-node flow. Real community nodes and real XMR credits are not enabled. Future community inference will disclose prompts to the selected node owner.
  </div>
  <div class="actions">
    <a class="button" href="/register">Create an account</a>
    <a class="button secondary" href="/login">Sign in</a>
    <a class="button secondary" href="/docs">Read the API guide</a>
  </div>
</section>

<section class="card" aria-labelledby="what-runs">
  <h2 id="what-runs">What runs in this milestone</h2>
  <p class="subtle">nginx serves this dashboard and forwards HTTPS requests to Go. Go sends synthetic requests to a private WSS mock node, streams the response over SSE, and publishes usage metadata through Redis and Kafka. The dashboard reads the Kafka projection; request text is not stored in that projection.</p>
</section>
