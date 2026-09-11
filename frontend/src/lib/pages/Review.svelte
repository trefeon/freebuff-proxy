<!-- REVIEW TEMP - TEMPORARY show-all page, WILL BE DELETED. Do not build on this. -->
<script>
  // REVIEW TEMP - TEMPORARY show-all page, WILL BE DELETED. Do not build on this.
  import { onMount } from "svelte";
  import { SvelteMap } from "svelte/reactivity";
  import {
    fetchAPI,
    postAPI,
    putAPI,
    deleteAPI,
    postForm,
  } from "../api/client.js";

  // REVIEW TEMP - every admin endpoint from backend/internal/dashboard/admin_manifest.json
  // (62 rows). Grouped sensibly; each row shows METHOD + PATH + note, param
  // inputs for {id}/{key} placeholders, a body textarea where the endpoint
  // takes JSON, a Try button, and a <pre> raw JSON output.
  const GROUPS = [
    {
      title: "Auth & session",
      rows: [
        {
          m: "GET",
          p: "/admin/login",
          note: "SPA login route (HTML, not JSON)",
        },
        {
          m: "POST",
          p: "/admin/login",
          note: "Sign in (form field: token)",
          form: "login",
        },
        {
          m: "POST",
          p: "/admin/logout",
          note: "Sign out (clears session cookie)",
        },
        {
          m: "POST",
          p: "/admin/login/start",
          note: "OAuth/device login start",
          json: true,
        },
        { m: "GET", p: "/admin/login/status", note: "Login flow status poll" },
        {
          m: "GET",
          p: "/admin/api/auth/status",
          note: "Auth status (default-token flag)",
        },
        {
          m: "POST",
          p: "/admin/api/change-password",
          note: "Change dashboard password",
          json: true,
          body: '{\n  "current_password": "",\n  "new_password": ""\n}',
        },
        {
          m: "POST",
          p: "/admin/api/require-login",
          note: "Toggle require-login gate",
          json: true,
        },
      ],
    },
    {
      title: "Status & overview",
      rows: [
        { m: "GET", p: "/admin/api/overview", note: "Overview snapshot" },
        { m: "GET", p: "/admin/api/version", note: "Version / update check" },
        { m: "GET", p: "/admin/api/notices", note: "Operator notices" },
        { m: "GET", p: "/admin/api/events", note: "Event feed (SSE backing)" },
        { m: "GET", p: "/admin/api/metrics", note: "Gateway metrics" },
        { m: "GET", p: "/admin/api/setup", note: "Setup state" },
        { m: "GET", p: "/admin/api/models", note: "Model catalog" },
      ],
    },
    {
      title: "Tokens & pool",
      rows: [
        { m: "GET", p: "/admin/api/tokens", note: "Pool snapshot" },
        {
          m: "POST",
          p: "/admin/tokens/test-all",
          note: "Probe every pool token",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/tokens/add",
          note: "Add one upstream token",
          json: true,
          body: '{\n  "token": "test-token"\n}',
        },
        {
          m: "POST",
          p: "/admin/tokens/remove",
          note: "DESTRUCTIVE: remove pool token (confirm)",
          json: true,
          danger: true,
          body: '{\n  "token": 0\n}',
        },
        {
          m: "POST",
          p: "/admin/tokens/swap",
          note: "DESTRUCTIVE: swap two pool positions (confirm)",
          json: true,
          danger: true,
          body: '{\n  "from": 0,\n  "to": 0\n}',
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/unlock",
          note: "Unlock pool token {id}",
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/lock",
          note: "Lock pool token {id}",
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/unlock-lock",
          note: "Clear stale lock on {id}",
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/finish",
          note: "DESTRUCTIVE: finish runs on {id} (confirm)",
          danger: true,
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/drop-session",
          note: "DESTRUCTIVE: drop session on {id} (confirm)",
          danger: true,
        },
        { m: "POST", p: "/admin/tokens/{id}/test", note: "Probe token {id}" },
        {
          m: "POST",
          p: "/admin/tokens/{id}/session",
          note: "Admit session on {id}",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/maturity",
          note: "Maturity probe on {id}",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/maturity/touch",
          note: "Touch maturity on {id}",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/tokens/{id}/maturity/warn-reset",
          note: "Reset maturity warning on {id}",
          json: true,
        },
      ],
    },
    {
      title: "Bridge (kept-for-human)",
      rows: [
        {
          m: "POST",
          p: "/admin/bridge-tokens/{key}/lock",
          note: "Lock bridge token {key}",
        },
        {
          m: "POST",
          p: "/admin/bridge-tokens/{key}/unlock",
          note: "Unlock bridge token {key}",
        },
      ],
    },
    {
      title: "Config & settings",
      rows: [
        {
          m: "GET",
          p: "/admin/api/config",
          note: "Effective config + .env document (sensitive)",
        },
        {
          m: "GET",
          p: "/admin/api/config/meta",
          note: "78-key catalog (keycatalog.go)",
        },
        {
          m: "GET",
          p: "/admin/api/settings",
          note: "DB overlay sources (env|db|file|default)",
        },
        {
          m: "POST",
          p: "/admin/api/settings",
          note: "Save one DB overlay key",
          json: true,
          body: '{\n  "key": "",\n  "value": ""\n}',
        },
        {
          m: "DELETE",
          p: "/admin/api/settings/{key}",
          note: "DESTRUCTIVE: drop DB override for {key} (confirm)",
          danger: true,
        },
        {
          m: "POST",
          p: "/admin/config",
          note: "DESTRUCTIVE: rewrite .env + reload (confirm)",
          form: "config",
        },
        { m: "GET", p: "/admin/api/pages/{id}", note: "Per-page stored state" },
        {
          m: "PUT",
          p: "/admin/api/pages/{id}",
          note: "Upsert per-page state",
          json: true,
        },
      ],
    },
    {
      title: "Activity (logs / quota / maturity / traces)",
      rows: [
        { m: "GET", p: "/admin/api/logs", note: "Recent logs (sensitive)" },
        {
          m: "GET",
          p: "/admin/api/logs/history",
          note: "Log history (sensitive)",
        },
        { m: "GET", p: "/admin/api/quota/history", note: "Quota history" },
        {
          m: "GET",
          p: "/admin/api/maturity/history",
          note: "Maturity history",
        },
        { m: "GET", p: "/admin/api/traces", note: "Trace list" },
      ],
    },
    {
      title: "Ops: mode / diag / smoke / playground / lifecycle",
      rows: [
        {
          m: "POST",
          p: "/admin/reload",
          note: "DESTRUCTIVE: reload proxy (confirm)",
          danger: true,
          json: true,
        },
        {
          m: "POST",
          p: "/admin/restart",
          note: "DESTRUCTIVE: restart gateway (confirm)",
          danger: true,
          json: true,
        },
        {
          m: "POST",
          p: "/admin/mode",
          note: "Kept-for-human: switch gateway mode",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/diag",
          note: "Kept-for-human: run diagnostics",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/smoke",
          note: "Kept-for-human: run smoke probe",
          json: true,
        },
        {
          m: "POST",
          p: "/admin/playground/chat",
          note: "Kept-for-human: model chat",
          json: true,
          body: '{\n  "messages": [\n    {\n      "role": "user",\n      "content": "review ping"\n    }\n  ]\n}',
        },
      ],
    },
    {
      title: "SPA routes (page links, no JSON)",
      rows: [
        {
          m: "GET",
          p: "/admin",
          note: "Dashboard root (redirects to /admin/)",
          link: true,
        },
        { m: "GET", p: "/admin/", note: "Dashboard SPA shell", link: true },
        {
          m: "GET",
          p: "/admin/tokens",
          note: "Legacy tokens route",
          link: true,
        },
        {
          m: "GET",
          p: "/admin/models",
          note: "Legacy models route",
          link: true,
        },
        {
          m: "GET",
          p: "/admin/traces",
          note: "Legacy traces route",
          link: true,
        },
        { m: "GET", p: "/admin/setup", note: "Legacy setup route", link: true },
        {
          m: "GET",
          p: "/admin/playground",
          note: "Legacy playground route",
          link: true,
        },
        {
          m: "GET",
          p: "/admin/config",
          note: "Legacy config route (sensitive)",
          link: true,
        },
        {
          m: "GET",
          p: "/admin/logs",
          note: "Legacy logs route (sensitive)",
          link: true,
        },
        {
          m: "GET",
          p: "/admin/metrics",
          note: "Legacy metrics route",
          link: true,
        },
      ],
    },
    {
      title: "Static assets",
      rows: [
        {
          m: "GET",
          p: "/admin/assets/",
          note: "Static asset dir (no JSON fetch; link only)",
          link: true,
        },
      ],
    },
  ];

  // REVIEW TEMP - per-row try-it state, keyed "gi-ri".
  let idVals = $state({});
  let keyVals = $state({});
  let bodyVals = $state({});
  let formVals = $state({});
  let outputs = $state({});
  let pending = $state({});

  function rowKey(gi, ri) {
    return `${gi}-${ri}`;
  }

  function resolvePath(p, k) {
    let out = p;
    if (out.includes("{id}"))
      out = out.replace("{id}", (idVals[k] ?? "0").trim() || "0");
    if (out.includes("{key}"))
      out = out.replace(
        "{key}",
        encodeURIComponent((keyVals[k] ?? "test-key").trim() || "test-key"),
      );
    return out;
  }

  function defaultBody(row) {
    return row.body ?? "{}";
  }

  // REVIEW TEMP - try-it dispatcher reusing lib/api/client.js (CSRF automatic).
  async function tryRow(gi, ri) {
    const row = GROUPS[gi].rows[ri];
    const k = rowKey(gi, ri);
    if (pending[k]) return;
    if (row.danger && typeof confirm === "function") {
      if (!confirm(`REVIEW TEMP: ${row.m} ${row.p} — destructive. Fire it?`))
        return;
    }
    pending[k] = true;
    outputs[k] = "…";
    const path = resolvePath(row.p, k);
    try {
      let res;
      if (row.form === "login") {
        const r = await postForm(path, { token: formVals[k] ?? "" });
        const text = await r.text().catch(() => "");
        try {
          res = { httpStatus: r.status, body: JSON.parse(text) };
        } catch {
          res = { httpStatus: r.status, body: text.slice(0, 2000) };
        }
      } else if (row.form === "config") {
        const r = await postForm(path, { content: formVals[k] ?? "" });
        const text = await r.text().catch(() => "");
        try {
          res = { httpStatus: r.status, body: JSON.parse(text) };
        } catch {
          res = { httpStatus: r.status, body: text.slice(0, 2000) };
        }
      } else if (row.m === "GET") {
        res = await fetchAPI(path);
      } else if (row.m === "POST") {
        let parsed;
        try {
          parsed = JSON.parse(bodyVals[k] ?? defaultBody(row));
        } catch {
          throw new Error("Body is not valid JSON");
        }
        res = await postAPI(path, parsed);
      } else if (row.m === "PUT") {
        let parsed;
        try {
          parsed = JSON.parse(bodyVals[k] ?? defaultBody(row));
        } catch {
          throw new Error("Body is not valid JSON");
        }
        res = await putAPI(path, parsed);
      } else if (row.m === "DELETE") {
        res = await deleteAPI(path);
      }
      outputs[k] = JSON.stringify(res, null, 2);
    } catch (e) {
      outputs[k] = JSON.stringify(
        { error: e?.message ?? String(e), status: e?.status ?? null },
        null,
        2,
      );
    } finally {
      pending[k] = false;
    }
  }

  // REVIEW TEMP - settings source table: same three fetches as Settings.svelte
  // (GET /admin/api/config/meta + GET /admin/api/config + GET /admin/api/settings).
  // Secret values are NEVER shown: secret rows render masked dots only, and the
  // .env document (env_content) is never parsed or displayed here.
  let meta = $state([]);
  let effectiveMap = $state.raw(new SvelteMap());
  let settingSources = $state({});
  let settingsDegraded = $state(false);
  let settingsError = $state("");
  let settingsLoading = $state(true);
  let editVals = $state({});
  let editOut = $state({});
  let tableFilter = $state("");

  function liveEditable(entry) {
    if (entry.secret || entry.restart_only || entry.hidden) return false;
    return ["bool", "text", "int", "select", "list"].includes(entry.kind);
  }

  async function fetchSettingsTable() {
    settingsLoading = true;
    settingsError = "";
    try {
      const [metaRes, cfgRes] = await Promise.all([
        fetchAPI("/admin/api/config/meta"),
        fetchAPI("/admin/api/config"),
      ]);
      meta = Array.isArray(metaRes) ? metaRes : (metaRes?.entries ?? []);
      const m = new SvelteMap();
      for (const kv of cfgRes.effective ?? []) m.set(kv.key, kv);
      effectiveMap = m;
      const nextEdits = {};
      for (const entry of meta) {
        const eff = m.get(entry.key)?.value ?? entry.default ?? "";
        nextEdits[entry.key] =
          entry.kind === "bool" ? String(eff) : String(eff ?? "");
      }
      editVals = nextEdits;
      try {
        const setRes = await fetchAPI("/admin/api/settings");
        const next = {};
        for (const e of setRes.settings ?? []) next[e.key] = e.source;
        settingSources = next;
        settingsDegraded = setRes.degraded === true;
      } catch {
        // Badges hidden; table still renders.
      }
    } catch (e) {
      settingsError = e?.message ?? "Failed to fetch settings table";
    } finally {
      settingsLoading = false;
    }
  }

  // REVIEW TEMP - row save reuses the Settings.svelte serialize pattern:
  // POST /admin/api/settings with {key, value}.
  async function saveSettingKey(entry) {
    const k = entry.key;
    editOut[k] = "…";
    try {
      let v = editVals[k] ?? "";
      if (entry.kind === "bool") v = v === "true" ? "true" : "false";
      if (entry.kind === "list") {
        v = String(v ?? "")
          .split(",")
          .map((s) => s.trim())
          .join(",");
      }
      const res = await postAPI("/admin/api/settings", {
        key: k,
        value: String(v ?? ""),
      });
      editOut[k] = JSON.stringify(res ?? { ok: true }, null, 2);
      await fetchSettingsTable();
    } catch (e) {
      editOut[k] = JSON.stringify({ error: e?.message ?? String(e) }, null, 2);
    }
  }

  let filteredMeta = $derived(
    tableFilter.trim()
      ? meta.filter((e) =>
          e.key.toLowerCase().includes(tableFilter.trim().toLowerCase()),
        )
      : meta,
  );

  onMount(() => {
    fetchSettingsTable();
  });
</script>

<!-- REVIEW TEMP - visible banner: this page WILL BE DELETED. -->
<div
  class="mb-6 rounded border-2 border-dashed border-red-500 bg-red-500/10 p-4"
  role="alert"
>
  <p class="text-lg font-bold text-red-500">
    REVIEW-TEMP: TEMPORARY show-all page — WILL BE DELETED
  </p>
  <p class="mt-1 text-sm text-[var(--fp-muted)]">
    REVIEW TEMP: click-through surface for every admin endpoint (all 62 manifest
    rows) plus the 78-key settings source table. Nothing here is production UI;
    do not link to it or build on it.
  </p>
</div>

<div class="space-y-8">
  <div>
    <h1 class="text-xl font-bold">REVIEW-TEMP: every admin endpoint</h1>
    <p class="text-sm text-[var(--fp-muted)]">
      <!-- REVIEW TEMP -->Read-only where unsure: GETs fire directly;
      POST/PUT/DELETE use minimal safe bodies and confirm() before destructive
      ones (remove/swap/drop-session/finish/restart/reload) but remain try-able.
    </p>
  </div>

  <!-- REVIEW TEMP - try-it sections, one per area -->
  {#each GROUPS as group, gi (group.title)}
    <section
      aria-label={group.title}
      class="rounded border border-[var(--fp-border)] p-4"
    >
      <h2 class="mb-3 text-base font-semibold">
        {group.title} ({group.rows.length})
      </h2>
      <div class="space-y-3">
        {#each group.rows as row, ri (rowKey(gi, ri))}
          {@const k = rowKey(gi, ri)}
          <div class="rounded border border-[var(--fp-border)] p-3">
            <!-- REVIEW TEMP row -->
            <div class="flex flex-wrap items-baseline gap-2">
              <span
                class="rounded bg-[var(--fp-accent)]/15 px-1.5 py-0.5 font-mono text-[11px] font-bold"
                >{row.m}</span
              >
              <code class="font-mono text-xs break-all">{row.p}</code>
            </div>
            <p class="mt-1 text-xs text-[var(--fp-muted)]">{row.note}</p>

            {#if row.link}
              <p class="mt-2 text-xs">
                <a class="underline" href={resolvePath(row.p, k)}
                  >Open {resolvePath(row.p, k)}</a
                >
                <span class="text-[var(--fp-muted)]">
                  (static/page route — no JSON fetch)</span
                >
              </p>
            {:else}
              <div class="mt-2 flex flex-wrap items-center gap-2">
                {#if row.p.includes("{id}")}
                  <label class="text-xs">
                    id
                    <input
                      class="fp-input ml-1 w-24 font-mono text-xs"
                      value={idVals[k] ?? "0"}
                      oninput={(e) => (idVals[k] = e.currentTarget.value)}
                    />
                  </label>
                {/if}
                {#if row.p.includes("{key}") && !row.json}
                  <label class="text-xs">
                    key
                    <input
                      class="fp-input ml-1 w-32 font-mono text-xs"
                      value={keyVals[k] ?? "test-key"}
                      oninput={(e) => (keyVals[k] = e.currentTarget.value)}
                    />
                  </label>
                {/if}
                {#if row.form === "login"}
                  <label class="text-xs">
                    token
                    <input
                      class="fp-input ml-1 w-40 font-mono text-xs"
                      type="password"
                      autocomplete="off"
                      value={formVals[k] ?? ""}
                      oninput={(e) => (formVals[k] = e.currentTarget.value)}
                    />
                  </label>
                {/if}
                <button
                  class="fp-btn fp-btn-sm"
                  disabled={pending[k]}
                  onclick={() => tryRow(gi, ri)}
                >
                  {pending[k] ? "Trying…" : "Try"}
                </button>
              </div>
              {#if row.form === "config"}
                <textarea
                  class="fp-input mt-2 w-full font-mono text-xs"
                  rows="4"
                  placeholder="# .env content (POST /admin/config rewrites the file)"
                  value={formVals[k] ?? ""}
                  oninput={(e) => (formVals[k] = e.currentTarget.value)}
                ></textarea>
              {/if}
              {#if row.json}
                {#if row.p.includes("{key}")}
                  <label class="mt-2 block text-xs">
                    key (also used in body only if you put it there)
                    <input
                      class="fp-input ml-1 w-32 font-mono text-xs"
                      value={keyVals[k] ?? "test-key"}
                      oninput={(e) => (keyVals[k] = e.currentTarget.value)}
                    />
                  </label>
                {/if}
                <textarea
                  class="fp-input mt-2 w-full font-mono text-xs"
                  rows="4"
                  value={bodyVals[k] ?? defaultBody(row)}
                  oninput={(e) => (bodyVals[k] = e.currentTarget.value)}
                ></textarea>
              {/if}
              {#if outputs[k]}
                <pre
                  class="mt-2 max-h-64 overflow-auto rounded bg-black/30 p-2 font-mono text-[11px] whitespace-pre-wrap">{outputs[
                    k
                  ]}</pre>
              {/if}
            {/if}
          </div>
        {/each}
      </div>
    </section>
  {/each}

  <!-- REVIEW TEMP - settings source table -->
  <section
    aria-label="Settings source table"
    class="rounded border border-[var(--fp-border)] p-4"
  >
    <h2 class="mb-1 text-base font-semibold">
      Settings source table (78-key catalog)
    </h2>
    <p class="mb-3 text-xs text-[var(--fp-muted)]">
      <!-- REVIEW TEMP -->GET /admin/api/config/meta + GET /admin/api/config +
      GET /admin/api/settings — same three fetches as Settings.svelte. Secret
      values never displayed (masked dots only); the .env document is never
      parsed here.
    </p>
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <input
        class="fp-input w-64 text-xs"
        placeholder="Filter keys…"
        value={tableFilter}
        oninput={(e) => (tableFilter = e.currentTarget.value)}
      />
      <button class="fp-btn fp-btn-sm" onclick={fetchSettingsTable}
        >Refresh</button
      >
      {#if settingsDegraded}
        <span class="text-xs text-yellow-500"
          >DB overlay unavailable (degraded)</span
        >
      {/if}
    </div>
    {#if settingsLoading}
      <p class="text-xs">Loading…</p>
    {:else if settingsError}
      <p class="text-xs text-red-500">{settingsError}</p>
    {:else}
      <div class="overflow-x-auto">
        <table class="w-full text-left font-mono text-[11px]">
          <thead>
            <tr class="border-b border-[var(--fp-border)]">
              <th class="p-1">key</th>
              <th class="p-1">group</th>
              <th class="p-1">kind</th>
              <th class="p-1">effective value</th>
              <th class="p-1">source</th>
              <th class="p-1">restart-only</th>
              <th class="p-1">edit</th>
            </tr>
          </thead>
          <tbody>
            {#each filteredMeta as entry (entry.key)}
              {@const eff =
                effectiveMap.get(entry.key)?.value ?? entry.default ?? ""}
              <tr class="border-b border-[var(--fp-border)] align-top">
                <td class="p-1 font-bold break-all">{entry.key}</td>
                <td class="p-1">{entry.group}</td>
                <td class="p-1">{entry.kind}</td>
                <td class="max-w-56 p-1 break-all">
                  {#if entry.secret}
                    <!-- REVIEW TEMP: secret masked, never displayed -->
                    <span title="secret value hidden">••••••••</span>
                  {:else}
                    {String(eff)}
                  {/if}
                </td>
                <td class="p-1">{settingSources[entry.key] ?? "—"}</td>
                <td class="p-1">{entry.restart_only ? "yes" : "no"}</td>
                <td class="p-1">
                  {#if liveEditable(entry)}
                    {#if entry.kind === "bool"}
                      <select
                        class="fp-input text-[11px]"
                        value={editVals[entry.key] ?? "false"}
                        onchange={(e) =>
                          (editVals[entry.key] = e.currentTarget.value)}
                      >
                        <option value="true">true</option>
                        <option value="false">false</option>
                      </select>
                    {:else if entry.kind === "select"}
                      <select
                        class="fp-input text-[11px]"
                        value={editVals[entry.key] ?? ""}
                        onchange={(e) =>
                          (editVals[entry.key] = e.currentTarget.value)}
                      >
                        {#each entry.enum ?? [] as opt (opt)}
                          <option value={opt}>{opt}</option>
                        {/each}
                      </select>
                    {:else}
                      <input
                        class="fp-input w-32 text-[11px]"
                        value={editVals[entry.key] ?? ""}
                        oninput={(e) =>
                          (editVals[entry.key] = e.currentTarget.value)}
                      />
                    {/if}
                    <button
                      class="fp-btn fp-btn-sm mt-1"
                      onclick={() => saveSettingKey(entry)}
                    >
                      Save
                    </button>
                    {#if editOut[entry.key]}
                      <pre
                        class="mt-1 max-h-24 overflow-auto whitespace-pre-wrap">{editOut[
                          entry.key
                        ]}</pre>
                    {/if}
                  {:else}
                    <span class="text-[var(--fp-muted)]">
                      read-only{#if entry.secret}
                        (secret){/if}{#if entry.restart_only}
                        (restart-only){/if}{#if entry.hidden}
                        (hidden){/if}
                    </span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
</div>
