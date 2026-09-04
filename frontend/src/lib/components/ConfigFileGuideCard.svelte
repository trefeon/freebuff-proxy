<script>
  import { FileText, Copy, Terminal, ExternalLink } from "@lucide/svelte";
  import SettingsCard from "./SettingsCard.svelte";
  import SettingsRow from "./SettingsRow.svelte";
  import CopyButton from "./CopyButton.svelte";
  import { tr } from "../i18n.js";

  /**
   * ConfigFileGuideCard — Guides operators on where configuration files live,
   * precedence rules, and host commands for direct editing.
   */
</script>

<SettingsCard
  title={$tr("Configuration File & Deployment")}
  description={$tr(
    "How environment variables and settings files are loaded, with precedence rules and host CLI commands.",
  )}
>
  {#snippet icon()}
    <FileText size={20} />
  {/snippet}

  <!-- File Location -->
  <SettingsRow
    first
    label={$tr("Configuration File Location")}
    description={$tr(
      "The gateway reads environment variables from .env at process launch. In container deployments, this is mounted from the host at /app/state/.env.",
    )}
  >
    <div class="flex items-center gap-2">
      <code
        class="fp-num text-xs text-[var(--fp-accent)] font-mono bg-[var(--fp-surface-2)] px-2 py-1 rounded border border-[var(--fp-border)] select-all"
      >
        /app/state/.env
      </code>
      <CopyButton text="/app/state/.env" label={$tr("Copy Path")} />
    </div>
  </SettingsRow>

  <!-- Precedence Order -->
  <SettingsRow
    label={$tr("Precedence Order")}
    description={$tr(
      "Configuration resolves in strict order: Host Environment Variables override -config JSON, which overrides the .env file, falling back to built-in catalog defaults.",
    )}
  >
    <div class="flex items-center gap-1.5 flex-wrap">
      <span
        class="text-[11px] px-2 py-0.5 rounded font-mono font-medium bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30"
      >
        1. Host Env
      </span>
      <span class="text-xs text-[var(--fp-dim)]">›</span>
      <span
        class="text-[11px] px-2 py-0.5 rounded font-mono font-medium bg-[var(--fp-surface-2)] text-[var(--fp-text)] border border-[var(--fp-border)]"
      >
        2. -config JSON
      </span>
      <span class="text-xs text-[var(--fp-dim)]">›</span>
      <span
        class="text-[11px] px-2 py-0.5 rounded font-mono font-medium bg-[var(--fp-surface-2)] text-[var(--fp-muted)] border border-[var(--fp-border)]"
      >
        3. .env File
      </span>
      <span class="text-xs text-[var(--fp-dim)]">›</span>
      <span
        class="text-[11px] px-2 py-0.5 rounded font-mono font-medium bg-[var(--fp-surface-2)] text-[var(--fp-dim)] border border-[var(--fp-border)]"
      >
        4. Defaults
      </span>
    </div>
  </SettingsRow>

  <!-- Edit on Host -->
  <SettingsRow
    label={$tr("Host Terminal Commands")}
    description={$tr(
      "To edit variables directly via SSH or terminal without the web interface, open the environment file with a text editor and restart the container.",
    )}
  >
    <div class="flex items-center gap-2">
      <code
        class="fp-num text-xs text-[var(--fp-text)] font-mono bg-[var(--fp-surface-2)] px-2.5 py-1 rounded border border-[var(--fp-border)] select-all"
      >
        nano .env
      </code>
      <CopyButton text="nano .env" label={$tr("Copy Command")} />
    </div>

    {#snippet extra()}
      <div
        class="mt-2.5 p-2.5 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)] flex items-center justify-between gap-2"
      >
        <div class="flex items-center gap-2 min-w-0">
          <Terminal size={14} class="shrink-0 text-[var(--fp-muted)]" />
          <span class="text-xs text-[var(--fp-muted)] truncate">
            {$tr("Restart container after host file edits")}:
          </span>
        </div>
        <div class="flex items-center gap-2 shrink-0">
          <code
            class="fp-num text-xs text-[var(--fp-accent)] font-mono select-all"
          >
            docker compose up -d
          </code>
          <CopyButton text="docker compose up -d" label={$tr("Copy Command")} />
        </div>
      </div>
    {/snippet}
  </SettingsRow>
</SettingsCard>
