# ADR-0018 — Terminal Cyber Ops dashboard theme (Stitch CLI pack)

Status: Accepted

Context: the admin dashboard shipped an amber-on-navy instrument-panel theme (IBM Plex, 8px radii, pill badges). The operator ordered a full retheme to the Stitch "Terminal Cyber Ops" pack (phosphor-green accent, obsidian canvas, Geist + JetBrains Mono, 4px military geometry, bracket telemetry badges) with layout adaption, desktop and mobile.

Decision: swap the theme at the token layer. `frontend/src/app.css` custom properties move to the obsidian/green values; every component already reads `var(--fp-*)`, so the reskin propagates without per-component color edits. Font deps swap net-zero: `@fontsource/geist` + `@fontsource/jetbrains-mono` in, `@fontsource/ibm-plex-*` out. Radii collapse to two values (`--fp-radius` 4px, `--fp-radius-sm` 3px); drop shadows removed; hardcoded `rounded-lg/xl/md` swept to `rounded`. StatusBadge renders bracket glyphs (`[+]`/`[!]`/`[-]`/`[*]`) in tinted hairline boxes. Sidebar active row is a green fill block with `_NN` index (aria-hidden, e2e names unchanged). PageShell gains an optional decorative `crumb` path line; PageHeader titles gain an aria-hidden `❯` prefix. Mockup-only inventions (fake touch tiers, fake IDs, invented stats, Cmd+K palette, bottom CLI bars) are explicitly NOT ported: only real backend-backed data renders.

Reasoning: token-layer swap keeps all 12 pages' logic, stores, endpoints, and e2e pins intact (72/72 green throughout; badge/nav text content unchanged, decorative additions aria-hidden). A per-component rewrite would have multiplied the diff for zero behavioral gain.

Alternatives considered: keep amber and reskin per mockup accents (rejected: user ordered the full pack); self-host via Google Fonts CDN link (rejected: offline builds + CSP posture favor fontsource self-host); Cmd+K command palette (deferred: new functional surface, needs its own slice and backend contract).

Consequences: first font change since the Plex adoption; Geist/JetBrains Mono must render in the operator's browsers (bundled, no network dependency). `rounded-full` remains only for dots, bars, spinner, and toggle geometry. `pill buttons` stay banned per DESIGN.md.

Invariants: no user-facing strings change except additive decorative nodes (aria-hidden); endpoint paths/shapes untouched; no new runtime dependencies (font swap is net-zero).

Affected packages: `frontend/src/app.css`, `frontend/package.json` (font swap), `DESIGN.md` (theme sections rewritten), `frontend/src/lib/components/*` (skin-only), 7 pages (crumb prop + Tokens header stat strip).

Related tests: full hermetic e2e stays green; visual QA via lucky-temporary Playwright sweep spec (deleted after use, screenshots reviewed inline).
