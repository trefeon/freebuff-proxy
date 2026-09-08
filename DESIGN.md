# DESIGN.md — dashboard visual grammar

Minimal token/layout grammar for dashboard work. Source of truth is
`frontend/src/app.css`; this file summarizes, it does not redefine.

## Theme

Dark-only (`color-scheme: dark`), "terminal cyber ops" instrument feel:
obsidian surfaces, hairline enclosures, no shadows except the focus ring.

## Tokens (`:root` in `app.css`)

| Role    | Token(s)                                            | Value(s)                          |
|---------|-----------------------------------------------------|-----------------------------------|
| bg      | `--fp-bg`                                           | `#0b0e14`                         |
| surface | `--fp-surface` / `--fp-surface-2` / `--fp-inset`    | `#131822` / `#161b26` / `#08090c` |
| input   | `--fp-input-bg`                                     | `#08090c`                         |
| border  | `--fp-border` / `--fp-border-bright`                | `#1f2633` / `#232b3b`             |
| text    | `--fp-text` / `--fp-muted` / `--fp-dim`             | `#f4f6fb` / `#94a3b8` / `#64748b` |
| accent  | `--fp-accent` / `--fp-accent-hover` / `--fp-accent-dim` | `#28c244` / `#00ff66` / `rgba(40,194,68,.12)` |
| state   | `--fp-success` / `--fp-warning` / `--fp-error` / `--fp-info` | `#22c55e` / `#f59e0b` / `#ef4444` / `#38bdf8` |
| radius  | `--fp-radius-sm` / `--fp-radius`                    | `3px` / `4px`                     |
| motion  | `--fp-ease` / `--fp-duration` / `--fp-duration-lg`  | `cubic-bezier(.23,1,.32,1)` / `150ms` / `250ms` |
| focus   | `--fp-ring`                                         | `0 0 0 2px bg, 0 0 0 4px accent`  |

## Type

- Sans: Geist (`400/500/600/700`). Mono: JetBrains Mono (`400/500/600`).
- Numeric data is always mono + tabular (`.fp-num`, `.tabular-nums`).

## Primitives (`app.css` classes)

- Surfaces: `.fp-card` (defined edge, hover lifts border to `--fp-border-bright`),
  `.fp-inset` (recessed), `.instrument-grid` (faint dot-grid backdrop).
- Buttons ranked by importance, not color: `.fp-btn` + `.fp-btn-primary` /
  `.fp-btn-secondary` / `.fp-btn-ghost` / `.fp-btn-danger`, `.fp-btn-sm`,
  `.fp-copy-icon` (square icon-only tap target).
- Inputs: `.fp-input`, `.fp-select` (number spinners hidden; custom steppers).
- Tables: `.fp-table` — hairline rows, left text, right numbers (`.num`).
- Status: `.led` + `.led-good` / `-warn` / `-bad` / `-info` / `-idle` /
  `-critical` / `-accent`; `.led-pulse` (2s) for live dots.
- Feedback: `.skeleton*` shimmer loaders; `.page-enter` page transition;
  `.sr-only` for screen-reader text; `prefers-reduced-motion` disables animation.

## Rules

1. Add new UI with existing `fp-*` classes; new tokens need a companion `app.css` change.
2. Buttons differ by rank (primary/secondary/ghost/danger), not hue.
3. Numbers right-aligned, mono, tabular. Status is an LED dot + label, never color alone.
4. Visible focus ring on everything (`:focus-visible`); never remove it.
5. Respect `prefers-reduced-motion`.
