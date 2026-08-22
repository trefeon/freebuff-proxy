<script>
  import { onMount } from 'svelte';
  import { Zap } from '@lucide/svelte';
  import Alert from '../components/Alert.svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import Field from '../components/Field.svelte';

  let token = $state('');
  let errorMsg = $state('');
  let loading = $state(false);
  let tokenInput = $state(null);

  const GENERIC_LOGIN_ERROR = 'Invalid password.';

  // The server replies to failed logins with {"error":"..."} JSON, but a
  // proxy or error page in front of it can return HTML or an empty body.
  // Only surface a clean message; never dump the raw response body.
  function cleanLoginError(body) {
    if (!body) return GENERIC_LOGIN_ERROR;
    try {
      const data = JSON.parse(body);
      const err = data?.error;
      const msg = typeof err === 'string' ? err : err?.message;
      if (typeof msg !== 'string' || !msg.trim()) return GENERIC_LOGIN_ERROR;
      return msg.trim();
    } catch {
      return GENERIC_LOGIN_ERROR;
    }
  }

  async function handleLogin(e) {
    e.preventDefault();
    if (!token.trim() || loading) return;
    loading = true;
    errorMsg = '';

    try {
      const res = await fetch('/admin/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ token: token.trim() }),
      });

      if (res.ok || res.redirected) {
        // Return to the tab the user came from (hash carried through
        // /admin/login by App.svelte's banner, or a direct #tab deep link).
        const tab = window.location.hash.replace('#', '');
        window.location.href = tab && tab !== 'login' ? `/admin#${tab}` : '/admin';
      } else {
        errorMsg = cleanLoginError(await res.text());
      }
    } catch (e) {
      errorMsg = 'Could not reach the server. Check the connection and try again.';
    } finally {
      loading = false;
    }
  }

  onMount(() => tokenInput?.focus());
</script>

<div class="instrument-grid page-enter min-h-[80vh] flex items-center justify-center px-4">
  <Card class="w-full max-w-sm">
    <div class="flex flex-col items-center gap-1 mb-7 text-center">
      <div class="flex items-center gap-2.5 mb-1">
        <span
          class="inline-flex items-center justify-center w-8 h-8 rounded-[var(--fp-radius-sm)] border border-[var(--fp-accent)]/30 bg-[var(--fp-accent-dim)] text-[var(--fp-accent)]"
        >
          <Zap size={16} />
        </span>
        <span class="text-lg font-semibold text-[var(--fp-text)]">freebuff-proxy</span>
      </div>
      <span class="text-[11px] font-mono uppercase tracking-[0.25em] text-[var(--fp-dim)]">Admin</span>
    </div>

    {#if errorMsg}
      <div class="mb-5">
        <Alert tone="error">{errorMsg}</Alert>
      </div>
    {/if}

    <form onsubmit={handleLogin} class="space-y-5">
      <Field label="Admin token" id="token">
        <input
          id="token"
          bind:this={tokenInput}
          bind:value={token}
          type="password"
          autocomplete="off"
          required
          placeholder="Enter admin token"
          class="fp-input fp-mono w-full"
        />
      </Field>

      <Button
        variant="primary"
        type="submit"
        class="w-full"
        disabled={loading || !token.trim()}
        loading={loading}
      >
        Sign in
      </Button>
    </form>

    <div class="pt-5 mt-6 border-t border-[var(--fp-border)] text-center space-y-1">
      <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed">
        Enter your admin token to access the dashboard.
      </p>
      <p class="text-[10px] text-[var(--fp-dim)]">
        Set <code class="fp-mono text-[var(--fp-muted)]">ADMIN_TOKEN</code> in your
        <code class="fp-mono text-[var(--fp-muted)]">.env</code> file to configure access.
      </p>
    </div>
  </Card>
</div>
