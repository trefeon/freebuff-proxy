<script>
  import { onMount, onDestroy } from 'svelte';
  import { RefreshCw, Save, X } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { fetchAPI, postForm } from '../api/client.js';
  import { tr } from '../i18n.js';
  import { formatTime } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  let envContent = $state('');
  let originalContent = $state('');
  let saving = $state(false);
  let result = $state(null); // { ok, message } — save/validate outcome
  let lastSavedTime = $state(null);

  let hasUnsavedChanges = $derived(envContent !== originalContent);

  // Live client-side .env parse — the page's validator (same rules as before:
  // separators, key syntax, duplicates). The server has no validate-only mode;
  // Save posts the raw content and surfaces the server's decision.
  let validationErrors = $derived.by(() => {
    const errors = [];
    const lines = envContent.split('\n');
    const seenKeys = new Set();
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line || line.startsWith('#')) continue;
      const eqIdx = line.indexOf('=');
      if (eqIdx === -1) { errors.push(`Line ${i + 1}: Missing '=' separator`); continue; }
      const key = line.substring(0, eqIdx).trim();
      if (!key) { errors.push(`Line ${i + 1}: Empty key name`); continue; }
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) errors.push(`Line ${i + 1}: Invalid key "${key}" (use A-Z, a-z, 0-9, _)`);
      if (seenKeys.has(key)) errors.push(`Line ${i + 1}: Duplicate key "${key}"`);
      seenKeys.add(key);
    }
    return errors;
  });
  let envValid = $derived(validationErrors.length === 0 && envContent.trim().length > 0);
  let lastSavedTimeStr = $derived(lastSavedTime ? formatTime(lastSavedTime) : '');

  let changedKeysCount = $derived.by(() => {
    const parseKeys = (c) => c.split('\n')
      .filter(l => l.trim() && !l.trim().startsWith('#') && l.includes('='))
      .map(l => l.split('=')[0].trim());
    const origKeys = new Set(parseKeys(originalContent));
    const curKeys = parseKeys(envContent);
    let count = 0;
    for (const key of curKeys) {
      const regex = new RegExp(`^\\s*${key}=(.*)$`, 'm');
      const o = originalContent.match(regex)?.[1]?.trim();
      const c = envContent.match(regex)?.[1]?.trim();
      if (o !== c) count++;
    }
    for (const key of origKeys) {
      if (!curKeys.includes(key)) count++;
    }
    return count;
  });
  let lineCount = $derived.by(() => envContent.split('\n').filter(l => l.trim()).length);
  let keyCount = $derived.by(() => envContent.split('\n').filter(l => l.trim() && !l.trim().startsWith('#') && l.includes('=')).length);

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/config');
      envContent = data.env_content || '';
      originalContent = envContent;
      error = '';
    } catch (e) {
      error = e.message || $tr('Failed to fetch configuration');
    } finally {
      loading = false;
    }
  }

  function validateConfig() {
    if (saving) return;
    if (!envContent.trim()) {
      result = { ok: false, message: $tr('Configuration is empty — nothing to save.') };
      return;
    }
    if (validationErrors.length === 0) {
      result = { ok: true, message: $tr('Configuration is valid — {count} key(s) parsed.', { count: keyCount }) };
    } else {
      const shown = validationErrors.slice(0, 5).join(' · ');
      const more = validationErrors.length > 5 ? ` (+${validationErrors.length - 5} more)` : '';
      result = { ok: false, message: $tr('Configuration invalid ({count}): {detail}', { count: validationErrors.length, detail: `${shown}${more}` }) };
    }
  }

  async function saveConfig(e, opts = {}) {
    e?.preventDefault();
    if (saving || !hasUnsavedChanges) return;
    if (opts.confirm !== false && !window.confirm($tr('Save the .env file and reload the proxy with these changes?'))) {
      return;
    }
    saving = true;
    result = null;

    try {
      const res = await postForm('/admin/config', { content: envContent });
      const json = await res.json();
      result = {
        ok: res.ok && json.ok,
        message: json.message || (res.ok ? $tr('Configuration saved and reloaded.') : $tr('Save failed')),
      };
      if (result.ok) {
        lastSavedTime = new Date();
        await fetchData();
      }
    } catch (e) {
      result = { ok: false, message: e.message || $tr('Network error saving configuration') };
    } finally {
      saving = false;
    }
  }

  function handleBeforeUnload(e) {
    if (hasUnsavedChanges) {
      e.preventDefault();
      e.returnValue = '';
    }
  }

  function handleKeyDown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
      e.preventDefault();
      if (hasUnsavedChanges && !saving) {
        saveConfig(null, { confirm: false });
      }
    }
  }

  onMount(() => {
    fetchData();
    window.addEventListener('beforeunload', handleBeforeUnload);
    window.addEventListener('keydown', handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener('beforeunload', handleBeforeUnload);
    window.removeEventListener('keydown', handleKeyDown);
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader title={$tr('Config')} description={$tr('Runtime .env editor — Save writes the file and reloads the running proxy.')}>
    {#snippet actions()}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr('Reload')}
      </Button>
      <Button
        variant="primary"
        onclick={saveConfig}
        disabled={saving || !hasUnsavedChanges}
        loading={saving}
      >
        <Save size={15} />
        {$tr('Save')}
      </Button>
    {/snippet}
  </PageHeader>

  {#if loading}
    <div class="space-y-6">
      <div class="skeleton skeleton-card"></div>
      <div class="skeleton skeleton-card"></div>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Alert tone="error">{error}</Alert>
      <div>
        <Button variant="secondary" onclick={fetchData}>
          <RefreshCw size={15} />
          {$tr('Retry')}
        </Button>
      </div>
    </div>
  {:else}
    {#if result}
      <Alert tone={result.ok ? 'success' : 'error'}>
        <div class="flex items-center justify-between gap-3">
          <span>{result.message}</span>
          <button
            type="button"
            onclick={() => result = null}
            class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors shrink-0"
            aria-label={$tr('Dismiss alert')}
          >
            <X size={14} />
          </button>
        </div>
      </Alert>
    {/if}

    <div class="grid grid-cols-1 lg:grid-cols-12 gap-6">
      <!-- Editor -->
      <div class="lg:col-span-7">
        <Card
          title={$tr('.env Editor')}
          description={$tr('Edit environment variables. Save validates server-side and reloads; rejected writes are rolled back.')}
        >
          {#snippet actions()}
            {#if data}
              <StatusBadge
                status={data.has_env_file ? $tr('env loaded') : $tr('no env file')}
                tone={data.has_env_file ? 'good' : 'warn'}
              />
            {/if}
            {#if hasUnsavedChanges}
              <StatusBadge
                status={$tr('{count} changed', { count: changedKeysCount })}
                tone="warn"
                pulse
              />
            {/if}
          {/snippet}

          <form onsubmit={saveConfig}>
            <label for="config-env" class="sr-only">{$tr('Environment file content')}</label>
            <textarea
              id="config-env"
              bind:value={envContent}
              rows="18"
              spellcheck="false"
              class="fp-input fp-mono w-full text-[13px] p-3.5
                {validationErrors.length > 0 ? 'border-[var(--fp-error)]/60' : envValid ? 'border-[var(--fp-success)]/40' : ''}"
              placeholder="# Configuration variables..."
            ></textarea>

            {#if validationErrors.length > 0}
              <div role="alert" aria-live="polite" class="mt-3 p-3 rounded-[var(--fp-radius-sm)] fp-inset border-[var(--fp-error)]/30 space-y-1">
                <p class="text-xs font-semibold text-[var(--fp-error)]">
                  {$tr('{count} validation error(s):', { count: validationErrors.length })}
                </p>
                {#each validationErrors.slice(0, 5) as err}
                  <p class="text-[11px] font-mono text-[var(--fp-error)]/80">{err}</p>
                {/each}
                {#if validationErrors.length > 5}
                  <p class="text-[11px] text-[var(--fp-dim)]">… {$tr('and {count} more', { count: validationErrors.length - 5 })}</p>
                {/if}
              </div>
            {/if}

            <div class="mt-4 flex flex-col sm:flex-row sm:items-center justify-between gap-3">
              <div class="flex items-center gap-3 text-[11px] text-[var(--fp-dim)] font-mono">
                <span>{lineCount} {$tr('lines')}</span>
                <span class="text-[var(--fp-border-bright)]">|</span>
                <span>{keyCount} {$tr('keys')}</span>
                {#if lastSavedTimeStr}
                  <span class="text-[var(--fp-border-bright)]">|</span>
                  <span>{$tr('saved {time}', { time: lastSavedTimeStr })}</span>
                {/if}
              </div>
              <div class="flex items-center gap-2">
                <Button variant="secondary" onclick={validateConfig} disabled={saving}>
                  {$tr('Validate')}
                </Button>
              </div>
          </form>
          <p class="mt-3 text-[11px] text-[var(--fp-dim)]">
            {$tr('Changes take effect after save.')} <kbd class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]">Ctrl+S</kbd> {$tr('saves from the keyboard')}.
          </p>
        </Card>
      </div>

      <!-- Effective config -->
      <div class="lg:col-span-5">
        <Card title={$tr('Effective Configuration')} description={$tr('Read-only view of the running configuration. Secret values are masked.')}>
          {#snippet actions()}
            <StatusBadge
              status={`${data?.effective?.length || 0} ${$tr('keys')}`}
              tone={data?.effective?.length ? 'good' : 'warn'}
            />
          {/snippet}

          {#if data?.effective?.length}
            <table class="fp-table">
              <thead>
                <tr>
                  <th>{$tr('Key')}</th>
                  <th>{$tr('Value')}</th>
                </tr>
              </thead>
              <tbody>
                {#each data.effective as kv}
                  <tr>
                    <td>
                      <div class="flex items-center gap-2 min-w-0">
                        <span class="fp-num text-[11px] font-semibold text-[var(--fp-text)] truncate">{kv.key}</span>
                        {#if kv.secret}
                          <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-error)]/40 bg-[var(--fp-error)]/15 text-[#FCA5A5] font-semibold uppercase tracking-wider shrink-0">{$tr('secret')}</span>
                        {/if}
                      </div>
                    </td>
                    <td>
                      <div class="flex items-center gap-2 min-w-0">
                        <span class="fp-num text-[11px] text-[var(--fp-muted)] truncate max-w-[180px]">
                          {kv.secret ? '••••••••' : (kv.value || '—')}
                        </span>
                        {#if kv.value}
                          <span class="shrink-0">
                            <CopyButton text={kv.value} label={$tr('copy')} />
                          </span>
                        {/if}
                      </div>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          {:else}
            <EmptyState title={$tr('No effective configuration')} description={$tr('Start the proxy to populate this view.')}>
              {#snippet action()}
                <Button variant="secondary" onclick={fetchData}>
                  <RefreshCw size={15} />
                  {$tr('Refresh')}
                </Button>
              {/snippet}
            </EmptyState>
          {/if}
        </Card>
      </div>
    </div>
  {/if}
</div>
