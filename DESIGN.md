# DESIGN.md — freebuff-proxy admin dashboard

Single source of truth for the admin dashboard design system. CSS, components, and pages are
projections of this file. If they drift, this file wins.

Changelog:
- 2026-09-07: Full theme swap to Terminal Cyber Ops (Stitch CLI pack):
  phosphor-green accent, obsidian canvas, Geist + JetBrains Mono, 4px radii,
  bracket telemetry badges. Replaces amber instrument panel. Font deps swap
  net-zero (@fontsource/geist + jetbrains-mono in, ibm-plex-* out).
- 2026-09-06: nav truth refresh. Sidebar lists 7 pages (Overview, Tokens,
  Maturity, Quota Tracker, Models, Logs, Settings) + gated Dev Tools;
  Setup/Metrics/Traces stay deep-link-only. New shared components: PageShell,
  KpiGrid, DataTable, FilterBar (ADR-0016 history views). TokenCard desktop +
  mobile fold into one responsive card; PremiumQuotaBar turns presentational.
- 2026-08-21 (audit): slop audit + a11y gate PASS after visual QA (browser screenshots reviewed
  with a vision model). Verified: IBM Plex renders (no fallback), mono tabular KPIs, single
  amber accent, no glass/soft-shadow/radius>8, LED+label status, mobile drawer + 2-up KPIs,
  zero console/page errors. Fixes during QA: duplicate Save removed, config values
  left-aligned, SECRET chip contrast raised, log panel horizontal scroll, backdrop-blur removed.

## Discovery

- **Artifact type**: Dashboard / data tool (operational) hybrid with admin/settings surfaces.
- **Positioning**: utilitarian internal tool for a solo operator running a self-hosted gateway.
  Not a product demo. Not a portfolio piece. Density and legibility beat decoration.
- **Audience**: one technical operator (the repo owner), desktop-first, dark environment.
- **Primary decision the dashboard supports**: "is my pool healthy, and what needs attention?"
- **Adjectives (locked)**: quiet, technical, exact.
- **3-word essence**: "terminal console."
- **Single-minded proposition**: the dashboard must read like a SOC scope — status at a glance,
  data as telemetry glyphs, no marketing flourishes.
 ## Aesthetic commitment

Dark-only terminal cyber-ops console. Obsidian canvas with 1px hairline
enclosures (tonal stacking, NEVER drop shadows). Phosphor signal green
(`#28C244`) as the single accent; cyan reserved for telemetry metadata,
amber/red strictly for warn/crit states. Every datum in JetBrains Mono with
tabular figures; UI labels in Geist. Restrained, flat, precise. Banned:
purple/indigo gradients, glassmorphism, pill buttons, rounded blobs,
gradient text, Inter/system default stacks.

## Signature move

**Bracket telemetry**: status reads as terminal glyphs — `[+] READY`,
`[!] WARN`, `[-] CRIT`, `[*] INFO` — mono badges with tinted fills, plus a
  6px pulsing phosphor beacon for live states. Card headers carry a `❯` prompt
  glyph prefix; the page background carries a faint dot-grid like a SOC scope.

  ## Typography

- **UI/body**: Geist (weights 400/500/600/700). Geometric, rapid legibility
  for narrative labels and headers.
- **Data**: JetBrains Mono (weights 400/500/600). All numerals, IDs,
  timestamps, log lines, table values, nav counts, CLI prompts (`❯`, `$`).
  `font-variant-numeric: tabular-nums` everywhere mono is used for data.
- Scale: modular 1.25, base 14px (dense ops):
  - `--text-xs` 11px — mono captions, table meta
  - `--text-sm` 12px — mono labels, secondary text
  - `--text-base` 14px — body/UI
  - `--text-lg` 16px — card titles (Geist 600)
  - `--text-xl` 20px — page titles (Geist 600)
  - `--text-2xl` 26px — section values (JetBrains Mono 600)
  - `--text-3xl` 34px — KPI numerals (JetBrains Mono 600)
- Headings use Geist 600; numbers use JetBrains Mono 600. Never both in one
  string without separation (e.g. labels in sans, values in mono).

## Layout grammar

- Desktop: fixed left sidebar 224px. Brand mark top (green bolt on obsidian),
  nav list (mono labels with `_01` index, active = green fill block), version/update
  badge pinned bottom. Content: right of sidebar, `max-width 1200px`, 12-col grid,
  generous section gaps.
- Status line lives on the Overview hero (mode badge, uptime, request count) — not a global
  strip; the shell stays minimal (sidebar + footer).
- Mobile (<768px): top bar with brand + hamburger; overlay drawer with flat nav list
  (no grouped nav). Content stacks; KPI row wraps 2-up.
- Tables: left-aligned text, right-aligned numeric columns with tabular-nums, hairline
  row separators, no vertical borders, hover row = surface-2.

## Color (hex, dark only)

Role | Hex
--- | ---
bg (deepest) | `#0B0E14`
surface | `#131822`
surface-2 (raised) | `#161B26`
inset (recessed) | `#08090C`
border | `#1F2633`
border-bright | `#232B3B`
text | `#F4F6FB`
muted | `#94A3B8`
dim | `#64748B`
accent (signal green) | `#28C244`
accent-hover (phosphor) | `#00FF66`
accent-dim (fill) | `rgba(40,194,68,0.12)`
success | `#22C55E`
warning | `#F59E0B`
error | `#EF4444`
info | `#38BDF8`

Distribution: ~60% neutrals / 30% surfaces / 10% accent. Accent appears on: active nav,
primary action, focus rings, live beacons, key KPI accent only when meaningful. Never gradient
text, never glassmorphism, never pill buttons (military posture: 4px geometry, circle dots only).

## Tokens

- Spacing base: 4px. Tight within groups (8–12px), generous between sections (24–32px).
- Radius: exactly two — `--radius-sm` 3px (chips, badges, small controls), `--radius` 4px
  (cards, panels, buttons, inputs). No radius above 4px anywhere (dots stay circular).
- Shadow: **none** — defined edges only. Elevation via border-bright + surface-2, never
  box-shadow on cards. (One exception: focus ring, which is a 2px accent outline.)
- Motion: 150ms default / 250ms page enter, `cubic-bezier(0.23,1,0.32,1)` (ease-out),
  transform+opacity only, no bounce, never animate layout properties.
  `prefers-reduced-motion: reduce` kills all non-essential animation.
- Focus: 2px solid accent outline offset 2px, always visible (`:focus-visible`).
- Borders: 1px hairline everywhere (`--fp-border`); hover raises to `--fp-border-bright`.

## Menu (7 sidebar + gated + deep-link-only)

Sidebar (`frontend/src/lib/nav.js` NAV_ITEMS is the code truth; this file is
the visual truth):

1. **Overview** — status hero (mode/version/uptime); 6 KPIs (pool total, busy,
   cooldown, banned, requests today, models); token risk cards.
   No smoke/diag, no sparklines (moved to CLI `-test-token`/`-doctor`).
2. **Tokens** — add-token form, device-login flow, client API-key management,
   token table (short id, status badge, instance, cooldown countdown, actions:
   clear cooldown, remove), per-model quota expand rows. One responsive
   TokenCard; no separate mobile fork.
3. **Maturity** — per-token streak/standing cards + maturity event timeline
   (ADR-0016 `maturity_events`).
4. **Quota Tracker** — accounting notice + per-model quota table + reset
   countdowns (ADR-0016 `quota_snapshots`).
5. **Models** — served model catalog table (mono id, served badge, aliases) +
   count summary.
6. **Logs** — FilterBar (level, message, hide-admin, follow), console/table
   toggle, mono stream with level dots, pagination (ADR-0016 `log_entries`,
   live tail over existing SSE).
7. **Settings** — grouped SettingsCards bound to `keycatalog.go` deterministically;
   Save/Validate/Reload row. (Replaces the old Config `.env` textarea page.)
8. **Dev Tools** — gated behind `DEVTOOLS_ENABLED`; never linked publicly.
9. **Login** — centered card, brand mark, token input, error alert.

Deep-link-only (no sidebar entry): Setup, Metrics, Traces, Playground
(Playground mounts DevTools).

## Components (exact API — cross-slice contract)

Shared library in `src/lib/components/`. Pages import these; do not restyle inline.

- `Button.svelte` — `{ variant: 'primary'|'secondary'|'ghost'|'danger' = 'secondary',
  size: 'sm'|'md' = 'md', disabled=false, loading=false, type='button', class }`,
  spreads `$$restProps`, children = label. Classes `.fp-btn .fp-btn-{variant}`.
- `Card.svelte` — `{ title?, description?, pad: 'md'|'none' = 'md', class }`,
  slots `{ actions, default, footer }`. Hairline border, radius 4px.
- `Stat.svelte` — `{ label, value, hint?, tone: 'default'|'good'|'warn'|'bad' = 'default',
  big=false }`. Value in mono 600, label in sans muted sm. Tone colors the value + LED.
- `StatusBadge.svelte` — `{ status, tone: 'good'|'warn'|'bad'|'info'|'idle' = 'info',
  pulse=false }`. 3px LED dot + mono label, uppercase.
- `Alert.svelte` — `{ tone: 'info'|'success'|'warning'|'error' = 'info', title? }`,
  children = body. Icon + tinted left hairline (accent/state color at 14% fill).
- `EmptyState.svelte` — `{ title, description? }`, slot `{ action }`.
- `CopyButton.svelte` — `{ text, label='Copy' }`. Copies `text`; shows check 1.5s.
- `PageHeader.svelte` — `{ title, description? }`, slot `{ actions }`.
- `Field.svelte` — `{ label, hint?, error?, id }`, children = control. Label sans sm,
  control mono base, error text red sm.
- `Spinner.svelte` — `{ size: 'sm'|'md' = 'md' }`. 2px arc in accent.
- `PageShell.svelte` — `{ title, description?, loading=false, error='', empty=null,
  onRetry }`, slots `{ actions, default }`. Renders PageHeader, then exactly
  one state: skeleton, Alert + retry, EmptyState, or content. Every page
  mounts this; no per-page loading/error scaffold.
- `KpiGrid.svelte` — `{ items: [{ label, value, hint?, tone? }] }`. Renders Stat
  cards in a responsive row (2-up on mobile). Tones match Stat.
- `DataTable.svelte` — `{ columns: [{ key, label, numeric=false }], rows,
  rowKey, expanded? }`. Hairline rows, right-aligned numerics in mono,
  optional expand slot per row (token quota details, log fields).
- `FilterBar.svelte` — `{ fields, values, onChange }`. SegmentedControl +
  Field controls in one wrapping row; Logs level/message/hide-admin/follow
  is the reference usage.
- `TokenCard.svelte` + `TokenCardMobile.svelte` — intentional fork: a `<tr>`
  cannot responsively become a stacked card, so desktop table rows and mobile
  cards stay separate. Shared logic (status/risk chips, cooldown labels)
  lives in `lib/utils/tokenStatus.js`; the expanded drawer is shared via
  `TokenDetailsDrawer.svelte`. Never re-duplicate helpers into the cards.
- `PremiumQuotaBar.svelte` — presentational only: `{ quota, now }`, no fetch,
  no clock. Parents pass the snapshot and tick.
- `Sparkline.svelte` — `{ values: number[], width = 120, height = 30, label }`.
  Min/max-normalized polyline, accent stroke, no fill/grid/axes. Renders
  nothing for fewer than 2 finite samples. History-backed cards (quota
  usage, maturity streaks) are the reference usage.
- `FieldBox.svelte` — `{ label, unit?, class? }`, children = control.
  Inset field card from the Stitch revamp set: title row (label + unit)
  over the control. Maturity Target/Touch boxes are the reference usage.
- `Stepper.svelte` — `{ value=bindable, min=1, max=28, disabled=false,
  ariaLabel?, decreaseLabel?, increaseLabel? }`. Minus/plus fp-btns around
  a centered number fp-input, clamped. Maturity target is the reference.
- `Pips.svelte` — `{ value=0, total=7, dots=7, label? }`. Fixed dot row
  filled proportionally plus mono value/total readout. Maturity streak and
  quota streak headers are the reference usages.

## Craft rules
- Buttons ranked by importance (primary = accent fill + dark text), never colored by meaning.
- Forms: real labels, correct input types, inline validation that keeps input.
- Status conveyed by LED + label (never color alone); error states visible + readable.
- Empty/loading/error states required on every page (skeleton classes in app.css,
  `EmptyState`, `Alert`).
- Icons: `@lucide/svelte` only, 16–18px, one stroke weight. No icon-only buttons without
  aria-label.
- Copy: terse operator voice. No marketing phrasing.
- a11y gate (WCAG 2.2 AA): visible managed focus, keyboard operability, labels everywhere,
  ≥24px targets (44px preferred for touch), color independence, reduced-motion.

## Slop audit (run before finishing)

Checklist — all must be clean in the final UI:
- [x] No Inter/Geist/system font as primary (IBM Plex Sans/Mono used)
- [x] No purple/indigo/violet gradients; no gradient text on metrics
- [x] No glassmorphism, no blobs, no radius > 8px on cards
- [x] No hairline-border + soft-shadow combo (defined edges only)
- [x] Numeric columns right-aligned, tabular-nums, mono
- [x] Components have hover/active/focus/disabled/loading/error states
- [x] No color-only status signals
- [x] prefers-reduced-motion respected
- [x] Menu shows 7 sidebar pages + gated Dev Tools; Setup/Metrics/Traces/Playground deep-link-only
