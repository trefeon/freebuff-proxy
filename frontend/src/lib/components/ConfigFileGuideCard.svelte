<script>
  import { FileText, Terminal } from "@lucide/svelte";
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

  <!-- 1. File Location -->
  <SettingsRow
    first
    label={$tr("Configuration File Location")}
    description={$tr(
      "The gateway reads environment variables from .env at process launch. In container deployments, this is mounted from the host at /app/state/.env.",
    )}
  >
    {#snippet extra()}
      <div
        class="mt-2.5 p-3 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)] flex items-center justify-between gap-3"
      >
        <div class="flex items-center gap-2.5 min-w-0">
          <span class="text-xs text-[var(--fp-muted)] shrink-0 font-medium">
            {$tr("Container Mount Path")}:
          </span>
          <code
            class="fp-num text-xs text-[var(--fp-accent)] font-mono font-semibold truncate select-all"
          >
            /app/state/.env
          </code>
        </div>
        <CopyButton text="/app/state/.env" label={$tr("Copy Path")} />
      </div>
    {/snippet}
  </SettingsRow>

  <!-- 2. Precedence Order (Balanced 4-card grid) -->
  <SettingsRow
    label={$tr("Precedence Order")}
    description={$tr(
      "Configuration values resolve in strict order from highest to lowest precedence:",
    )}
  >
    {#snippet extra()}
      <ol
        class="mt-2.5 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-2 text-xs"
      >
        <li
          class="flex items-center gap-2 p-2.5 rounded-lg bg-[var(--fp-accent)]/10 border border-[var(--fp-accent)]/30"
        >
          <span class="font-mono font-bold text-[var(--fp-accent)]">1.</span>
          <span class="font-mono font-medium text-[var(--fp-text)]"
            >Host Env</span
          >
          <span
            class="text-[10px] text-[var(--fp-accent)] ml-auto font-medium lowercase"
            >{$tr("highest")}</span
          >
        </li>
        <li
          class="flex items-center gap-2 p-2.5 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)]"
        >
          <span class="font-mono font-bold text-[var(--fp-muted)]">2.</span>
          <span class="font-mono text-[var(--fp-text)]">-config JSON</span>
        </li>
        <li
          class="flex items-center gap-2 p-2.5 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)]"
        >
          <span class="font-mono font-bold text-[var(--fp-muted)]">3.</span>
          <span class="font-mono text-[var(--fp-text)]">.env File</span>
        </li>
        <li
          class="flex items-center gap-2 p-2.5 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)]"
        >
          <span class="font-mono font-bold text-[var(--fp-dim)]">4.</span>
          <span class="font-mono text-[var(--fp-muted)]">{$tr("Defaults")}</span
          >
        </li>
      </ol>
    {/snippet}
  </SettingsRow>

  <!-- 3. Host Terminal Commands (Full-width rows, never wrapping) -->
  <SettingsRow
    last
    label={$tr("Host Terminal Commands")}
    description={$tr(
      "To edit variables directly via SSH or host terminal without the web interface:",
    )}
  >
    {#snippet extra()}
      <div class="mt-2.5 flex flex-col gap-2">
        <!-- Edit file command -->
        <div
          class="p-3 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)] flex items-center justify-between gap-3"
        >
          <div class="flex items-center gap-2.5 min-w-0">
            <span class="text-xs text-[var(--fp-muted)] shrink-0 font-medium">
              {$tr("Edit config file")}:
            </span>
            <code
              class="fp-num text-xs text-[var(--fp-text)] font-mono font-semibold truncate select-all"
            >
              nano .env
            </code>
          </div>
          <CopyButton text="nano .env" label={$tr("Copy Command")} />
        </div>

        <!-- Reload container command -->
        <div
          class="p-3 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)] flex items-center justify-between gap-3"
        >
          <div class="flex items-center gap-2.5 min-w-0">
            <span class="text-xs text-[var(--fp-muted)] shrink-0 font-medium">
              {$tr("Restart container")}:
            </span>
            <code
              class="fp-num text-xs text-[var(--fp-accent)] font-mono font-semibold truncate select-all"
            >
              docker compose up -d
            </code>
          </div>
          <CopyButton text="docker compose up -d" label={$tr("Copy Command")} />
        </div>
      </div>
    {/snippet}
  </SettingsRow>
</SettingsCard>
