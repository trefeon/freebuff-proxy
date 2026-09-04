<script>
  import { onMount } from "svelte";
  import {
    Terminal,
    Power,
    RefreshCw,
    ArrowUpCircle,
    ExternalLink,
  } from "@lucide/svelte";
  import SettingsCard from "./SettingsCard.svelte";
  import SettingsRow from "./SettingsRow.svelte";
  import Button from "./Button.svelte";
  import CopyButton from "./CopyButton.svelte";
  import Alert from "./Alert.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, adminActions } from "../api/paths.js";
  import { confirmAction } from "../stores/confirm.js";
  import { tr } from "../i18n.js";

  /**
   * CommandCenterCard — Lifecycle, software updates, and version rollback.
   * Built using the SettingsCard and SettingsRow template components.
   * (docs/command-center-plan.md)
   */
  let versionInfo = $state({
    current_version: "",
    latest_version: "",
    has_update: false,
    update_url: "",
  });

  let checking = $state(false);
  let restarting = $state(false);
  let restartMsg = $state("");
  let checkMsg = $state("");

  async function loadVersion(force = false) {
    try {
      const url = force ? `${adminApi.version}?force=true` : adminApi.version;
      const data = await fetchAPI(url);
      if (data) {
        versionInfo = {
          current_version: data.current_version || "",
          latest_version: data.latest_version || "",
          has_update: Boolean(data.has_update),
          update_url: data.update_url || "",
        };
      }
    } catch {
      // Degrade gracefully
    }
  }

  onMount(() => {
    loadVersion();
  });

  async function handleCheckUpdates() {
    checking = true;
    checkMsg = "";
    try {
      await loadVersion(true);
      if (versionInfo.has_update) {
        checkMsg = $tr("New version available: {ver}", {
          ver: versionInfo.latest_version,
        });
      } else {
        checkMsg = $tr("Gateway is up to date ({ver})", {
          ver: versionInfo.current_version || "latest",
        });
      }
    } catch (e) {
      checkMsg = e.message || $tr("Failed to check for updates.");
    } finally {
      checking = false;
    }
  }

  function promptRestart() {
    confirmAction({
      title: $tr("Restart Gateway Process"),
      message: $tr(
        "Restart the proxy process? In-flight requests will finish or drop. Session and quota state will resume automatically from disk.",
      ),
      confirmText: $tr("Restart Now"),
      tone: "warn",
      onConfirm: async () => {
        restarting = true;
        restartMsg = "";
        try {
          const res = await postAPI(adminActions.restart, {});
          restartMsg =
            res.message ||
            $tr("Gateway restart initiated. Reconnecting in a few seconds…");
          // Poll /healthz until the server recovers
          setTimeout(() => {
            window.location.reload();
          }, 3500);
        } catch (err) {
          restarting = false;
          restartMsg = err.message || $tr("Failed to initiate restart.");
        }
      },
    });
  }
</script>

<SettingsCard
  title={$tr("Command Center")}
  description={$tr(
    "Process lifecycle, version updates, and rollbacks. Restarts are managed safely by your container supervisor.",
  )}
>
  {#snippet icon()}
    <Terminal size={20} />
  {/snippet}

  {#if restartMsg}
    <div class="mb-4">
      <Alert tone={restarting ? "warning" : "info"}>
        <div class="flex items-center gap-2">
          {#if restarting}
            <span class="inline-block animate-spin text-sm">⏳</span>
          {/if}
          <span class="text-xs">{restartMsg}</span>
        </div>
      </Alert>
    </div>
  {/if}

  <!-- Gateway Process Lifecycle -->
  <SettingsRow
    first
    label={$tr("Gateway Process")}
    description={$tr(
      "Gracefully stops the proxy process. Container supervisors (restart: unless-stopped) or systemd restart it immediately with persisted state intact.",
    )}
  >
    {#snippet badge()}
      <span
        class="text-[10px] px-2 py-0.5 rounded-[var(--fp-radius-sm)] border border-emerald-500/30 bg-emerald-500/10 text-emerald-400 font-semibold uppercase tracking-wider shrink-0 flex items-center gap-1.5"
      >
        <span class="led led-good" aria-hidden="true"></span>
        {$tr("running")}
      </span>
      {#if versionInfo.current_version}
        <span
          class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
        >
          {versionInfo.current_version}
        </span>
      {/if}
    {/snippet}

    <Button
      variant="secondary"
      size="sm"
      onclick={promptRestart}
      disabled={restarting}
    >
      <Power size={14} class="text-[var(--fp-warning)]" />
      {$tr("Restart Gateway")}
    </Button>
  </SettingsRow>

  <!-- Software Update Check -->
  <SettingsRow
    label={$tr("Software Updates")}
    description={$tr(
      "Checks the latest published GitHub release. Updates can be pulled and deployed via Docker Compose or your host package manager.",
    )}
  >
    {#snippet badge()}
      {#if versionInfo.has_update}
        <span
          class="text-[10px] px-2 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-warning)]/50 bg-[var(--fp-warning)]/15 text-[var(--fp-warning)] font-semibold uppercase tracking-wider shrink-0 flex items-center gap-1"
        >
          <ArrowUpCircle size={12} />
          {$tr("Update Available: {ver}", { ver: versionInfo.latest_version })}
        </span>
      {:else if checkMsg}
        <span class="text-[11px] text-[var(--fp-dim)] font-mono">
          {checkMsg}
        </span>
      {/if}
    {/snippet}

    <div class="flex items-center gap-2">
      <Button
        variant="secondary"
        size="sm"
        onclick={handleCheckUpdates}
        loading={checking}
      >
        <RefreshCw size={14} />
        {$tr("Check for Updates")}
      </Button>
      {#if versionInfo.update_url}
        <a
          href={versionInfo.update_url}
          target="_blank"
          rel="noopener noreferrer"
          class="fp-btn fp-btn-ghost !text-xs !py-1.5 !px-2.5 flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)]"
          title={$tr("View release notes")}
        >
          <ExternalLink size={13} />
          {$tr("Releases")}
        </a>
      {/if}
    </div>

    {#snippet extra()}
      {#if versionInfo.has_update}
        <div
          class="mt-2.5 p-3 rounded-lg bg-[var(--fp-surface-2)]/60 border border-[var(--fp-border)] flex flex-col gap-2"
        >
          <div class="flex items-center justify-between gap-2">
            <span class="text-xs font-medium text-[var(--fp-text)]">
              {$tr("Host Upgrade Command (Docker)")}:
            </span>
            <CopyButton
              text="docker compose pull && docker compose up -d"
              label={$tr("Copy Command")}
            />
          </div>
          <code
            class="fp-num text-xs text-[var(--fp-accent)] font-mono select-all bg-[var(--fp-surface)] px-2.5 py-1.5 rounded border border-[var(--fp-border)]"
          >
            docker compose pull && docker compose up -d
          </code>
        </div>
      {/if}
    {/snippet}
  </SettingsRow>

  <!-- Version Rollback Reference -->
  <SettingsRow
    last
    label={$tr("Version Rollback")}
    description={$tr(
      "To roll back or pin a specific version, set VERSION=vX.Y.Z in your deployment environment and re-run docker compose up -d.",
    )}
  >
    <div class="flex items-center gap-2">
      <CopyButton
        text="VERSION=v1.8.8 docker compose up -d"
        label={$tr("Copy Rollback Example")}
      />
    </div>
  </SettingsRow>
</SettingsCard>
