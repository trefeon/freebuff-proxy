<script>
  import Alert from "./Alert.svelte";
  import Button from "./Button.svelte";
  import { tr } from "../i18n.js";

  /** @prop {() => void} onChangePassword */
  let { onChangePassword } = $props();
</script>

<Alert tone="warning" title={$tr("Default admin token in use")}>
  <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
    <p class="text-sm text-[var(--fp-muted)]">
      {$tr(
        "The proxy is using the built-in default admin token. Change it to secure the dashboard.",
      )}
    </p>
    <Button variant="secondary" size="sm" onclick={onChangePassword}>
      {$tr("Change password")}
    </Button>
  </div>
</Alert>
