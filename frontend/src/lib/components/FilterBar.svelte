<script>
  import { Search } from "@lucide/svelte";
  import SegmentedControl from "./SegmentedControl.svelte";
  import ToggleSwitch from "./ToggleSwitch.svelte";
  import Field from "./Field.svelte";

  /**
   * FilterBar — one wrapping row of filter controls. The Logs page
   * (level segment, message search, hide-admin toggle, follow toggle) is the
   * reference usage; other pages declare their own field lists.
   *
   * @prop {Array<{ key: string, label: string, type: 'segment'|'search'|'toggle', options?: Array<{id:string,label:string}>, placeholder?: string }>} [fields=[]]
   * @prop {Record<string, any>} [values={}]
   * @prop {(key: string, value: any) => void} [onChange]
   */
  let { fields = [], values = {}, onChange } = $props();

  function emit(key, value) {
    onChange?.(key, value);
  }
</script>

<div class="flex flex-wrap items-end gap-x-4 gap-y-3">
  {#each fields as f (f.key)}
    {#if f.type === "segment"}
      <div class="flex flex-col gap-1.5">
        <span class="text-xs text-[var(--fp-muted)]">{f.label}</span>
        <SegmentedControl
          options={f.options ?? []}
          value={values[f.key] ?? ""}
          ariaLabel={f.label}
          onchange={(v) => emit(f.key, v)}
        />
      </div>
    {:else if f.type === "search"}
      <Field label={f.label} id="fb-{f.key}">
        <div class="relative">
          <Search
            size={14}
            class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)]"
            aria-hidden="true"
          />
          <input
            id="fb-{f.key}"
            type="search"
            value={values[f.key] ?? ""}
            placeholder={f.placeholder ?? ""}
            oninput={(e) => emit(f.key, e.currentTarget.value)}
            class="w-56 border border-[var(--fp-border)] rounded-[var(--fp-radius-sm)] bg-[var(--fp-inset)] pl-8 pr-3 py-1.5 text-sm font-mono text-[var(--fp-text)] placeholder:text-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)]"
          />
        </div>
      </Field>
    {:else if f.type === "toggle"}
      <div class="flex flex-col gap-1.5">
        <span class="text-xs text-[var(--fp-muted)]">{f.label}</span>
        <ToggleSwitch
          checked={values[f.key] ?? false}
          ariaLabel={f.label}
          onchange={(v) => emit(f.key, v)}
        />
      </div>
    {/if}
  {/each}
</div>
