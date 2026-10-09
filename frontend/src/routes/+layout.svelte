<script lang="ts">
  import '../app.css';
  let { children } = $props();
</script>

<a class="skip-link" href="#main-content">Skip to content</a>
<div class="shell">
  <header class="topbar">
    <a class="brand" href="/">cordova<span>.</span></a>
    <nav class="links" aria-label="Main navigation">
      <a href="/docs">Docs</a>
      <a href="/login">Sign in</a>
    </nav>
  </header>
  <main class="content" id="main-content" tabindex="-1">{@render children()}</main>
  <footer class="footer"><span>Cordova · M1 mock network</span><span>Synthetic usage only · No real credits</span></footer>
</div>

<style>
  .skip-link { position: fixed; left: 16px; top: -60px; z-index: 10; padding: 10px 14px; border-radius: 7px; background: white; color: #10141b; }
  .skip-link:focus { top: 12px; }
</style>
