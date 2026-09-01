<script>
  import { onMount } from 'svelte';
  import { RefreshCw, Key, Eye, EyeOff, Trash2 } from '@lucide/svelte';
  import Card from './Card.svelte';
  import Button from './Button.svelte';
  import CopyButton from './CopyButton.svelte';
  import Alert from './Alert.svelte';
  import GeneratedKeyModal from './GeneratedKeyModal.svelte';
  import { fetchAPI, postForm } from '../api/client.js';
  import { adminApi, adminActions } from '../api/paths.js';
  import { generateRandomApiKey } from '../utils/format.js';
  import { getEnvValue, setEnvValue } from '../utils/env.js';
  import { tr } from '../i18n.js';

  /**
   * ApiKeysEditor - the Client API Keys card (sk-fb- credentials stored in
   * API_KEYS in .env). Split out of Overview.svelte (issue #287) so the
   * overview page owns KPIs/risk cards while this component owns the API-key
   * generate/delete/reveal flow and the generated-key modal.
   */
  let apiKeys = $state([]);
  let clientKeyMessage = $state('');
  let clientKeyOK = $state(true);
  let generatingKey = $state(false);
  let generatedKey = $state('');
  let deletingKey = $state('');
  let showGeneratedModal = $state(false);
  let visibleKeys = $state({});

  function toggleKeyVisibility(key) {
    visibleKeys = { ...visibleKeys, [key]: !visibleKeys[key] };
  }
  function maskKey(key) {
    if (visibleKeys[key]) return key;
    if (!key) return '';
    if (key.length <= 10) return '••••••••';
    const prefix = key.startsWith('sk-fb-') ? 'sk-fb-' : key.slice(0, 6);
    const suffix = key.slice(-4);
    const padding = '•'.repeat(Math.max(0, key.length - prefix.length - suffix.length));
    return `${prefix}${padding}${suffix}`;
  }

  function openGeneratedKeyModal(key) {
    generatedKey = key;
    showGeneratedModal = true;
  }

  function closeGeneratedKeyModal() {
    showGeneratedModal = false;
  }

  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    generatedKey = '';
    clientKeyMessage = '';
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const existing = getEnvValue(envContent, 'API_KEYS') || '';
      const updated = existing ? `${existing},${newKey}` : newKey;
      const newContent = setEnvValue(envContent, 'API_KEYS', updated);
      const save = await postForm(adminActions.configSave, { content: newContent });
      const result = await save.json();
      const isSaved = save.ok;
      const isOverridden = result?.message && String(result.message).includes('overridden by the process environment');
      clientKeyOK = isSaved;
      if (clientKeyOK) {
        openGeneratedKeyModal(newKey);
        clientKeyMessage = isOverridden
          ? $tr('Generated & saved client API key (environment notice: server process environment takes precedence until restart)')
          : $tr('Generated & saved client API key');
        fetchConfig();
      } else {
        clientKeyMessage = result?.message || $tr('Failed to save client API key');
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || $tr('Network error generating client key');
    } finally {
      generatingKey = false;
    }
  }

  async function deleteApiKey(target) {
    if (deletingKey) return;
    deletingKey = target;
    clientKeyMessage = '';
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const val = getEnvValue(envContent, 'API_KEYS') || '';
      const keys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
      const filtered = keys.filter((k) => k !== target);
      const updated = filtered.join(',');
      const newContent = setEnvValue(envContent, 'API_KEYS', updated);
      const save = await postForm(adminActions.configSave, { content: newContent });
      const result = await save.json();
      const isSaved = save.ok;
      const isOverridden = result?.message && String(result.message).includes('overridden by the process environment');
      clientKeyOK = isSaved;
      if (clientKeyOK) {
        clientKeyMessage = isOverridden
          ? $tr('Deleted client API key (environment notice: server process environment takes precedence until restart)')
          : $tr('Deleted client API key');
        apiKeys = filtered;
        fetchConfig();
      } else {
        clientKeyMessage = result?.message || $tr('Failed to delete client API key');
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || $tr('Network error deleting client key');
    } finally {
      deletingKey = '';
    }
  }

  // Config-derived display field (apiKeys) changes only on save, so it is
  // fetched once on mount instead of on every 15s overview poll.
  async function fetchConfig() {
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
      const val = m ? m[1].trim() : '';
      apiKeys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
    } catch {
      apiKeys = [];
    }
  }

  onMount(() => {
    fetchConfig();
  });
</script>

<Card
  title={$tr('Client API Keys')}
  description={$tr('sk-fb-… credentials for clients (omp, Cursor, Claude Code, curl) to authenticate against this gateway. Stored in API_KEYS in .env.')}
>
  {#snippet actions()}
    <Button variant="primary" size="sm" onclick={generateClientKey} disabled={generatingKey}>
      {#if generatingKey}
        <RefreshCw size={14} class="animate-spin" />
        <span>{$tr('Generating…')}</span>
      {:else}
        <Key size={14} />
        <span>{$tr('Generate API Key')}</span>
      {/if}
    </Button>
  {/snippet}

  {#if apiKeys.length > 0}
    <div class="flex flex-col gap-2 mb-3">
      {#each apiKeys as key (key)}
        <div class="fp-inset rounded flex items-center justify-between gap-2 px-3 py-2">
          <code class="fp-num text-xs truncate flex-1 select-all font-mono">{maskKey(key)}</code>
          <div class="flex items-center gap-1 shrink-0">
            <Button
              variant="ghost"
              size="sm"
              onclick={() => toggleKeyVisibility(key)}
              aria-label={visibleKeys[key] ? $tr('Hide API key') : $tr('Show API key')}
              title={visibleKeys[key] ? $tr('Hide API key') : $tr('Show API key')}
            >
              {#if visibleKeys[key]}
                <EyeOff size={14} />
              {:else}
                <Eye size={14} />
              {/if}
            </Button>
            <CopyButton text={key} label="Copy" />
            <Button
              variant="ghost"
              size="sm"
              onclick={() => deleteApiKey(key)}
              disabled={deletingKey === key}
              aria-label={$tr('Delete API key')}
              title={$tr('Delete API key')}
            >
              <Trash2 size={14} />
              <span>{$tr('Delete')}</span>
            </Button>
          </div>
        </div>
      {/each}
    </div>
  {:else}
    <p class="text-xs text-[var(--fp-dim)] mb-3">
      {$tr('No client API keys configured. In open mode, clients can authenticate with any key or leave it unset.')}
    </p>
  {/if}

  {#if clientKeyMessage}
    <Alert tone={clientKeyOK ? 'success' : 'error'} title={clientKeyMessage} />
  {/if}
</Card>

<GeneratedKeyModal bind:open={showGeneratedModal} key={generatedKey} onClose={closeGeneratedKeyModal} />
