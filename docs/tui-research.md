# FreeBuff TUI — Full Inventory for Dashboard Integration
> Research date: 2026-09-01 · Reference: `reference/freebuff@70f1d8d` (`cli/src`, `common/src`)
> Goal: every data point the TUI shows, its wire source, formatting, and mapping to `freebuff-proxy` dashboard.

## 1. TUI Layout Overview
```
┌─ chat-header (branch, model, git status) ─────────────────┐
├─ chat transcript (assistant/user blocks, tool calls)      │
├─ agent checklist / mode toggle                            │
├─ chat-input-bar (model pill, thinking budget)             │
├─ status-bar (idle: model·remaining·context | waiting/streaming: shimmer + elapsed) │
├─ bottom-banner / help-banner / ad-banner / choice-ad     │
└─ modal overlays: model-selector (landing), referral-banner, subscription-limit-banner
```
Landing (`freebuff-landing-screen.tsx`) when `session.status==='none'`: hero model, `FreebuffModelSelector` grid, `FreebuffReferralBanner`, streak line, premium reset, gravity ad, waiting-room placements.

## 2. Status Bar (`cli/src/components/status-bar.tsx:69`)
**Visible in every turn.**

| Field | Source | Computation | Display |
|---|---|---|---|
| `sessionProgress.fraction` → draining fill | `useFreebuffSessionProgress(session)` | `fraction = remainingMs / totalMs` (`expiresAt-admittedAt`), `remainingMs = max(0, expiresAt - now)` ticks 1Hz via `useNow(1000)` | 100%→0% bar `theme.surfaceHover` |
| `remainingMs` → countdown | same hook | `formatFreebuffSessionRemaining(remainingMs)` (`freebuff-session-display.ts:11`): `<5m: m:s`, `<60m: Xm left`, else `Xh Ym left`; `expiring…` at 0 | idle: `Model · unlimited / 3h 12m left · 42% context` (`138-207`) |
| `isUnlimited` | `session.status==='active' && !session.rateLimit` | `rateLimit` absent = unlimited (no session quota) | shows `unlimited` instead of countdown |
| `contextUsage` | `chat-store.runState.mainAgentState.contextTokenCount` + `FREEBUFF_MODEL_CONTEXT_WINDOWS[model]` | `formatContextUsage(count, window)` → `42%` | suffix `· 42%` |
| `elapsedSeconds` | `timerStartTime` + `statusIndicatorState` (`waiting/streaming/paused`) | `setInterval 1s` | right side `2:34` |
| `statusIndicator` | `status-indicator-state` (`idle, waiting:thinking..., streaming:working..., paused, retrying, capacityWait:high demand…, connecting, reconnected, ctrlC, clipboard`) | `ShimmerText` | center |
| `sessionModel displayName` | `getFreebuffModel(session.model).displayName` | catalog lookup | `GPT-5.6 Luna · 3h left` |
| Buttons | `onStop` (waiting/streaming → `■ Esc`), `onEndSession` (idle+active → `✕ End session`), urgent countdown `<5m` red `formatFreebuffSessionCountdown` `m:ss` | | |

**Proxy mapping:** `session.SessionSnapshot` already carries `InstanceID, Model, ExpiresAt, RemainingMs` (`session.go:233`), `pool.TokenSnapshot` exposes `SessionRemainingSeconds`, `SessionModel`. Dashboard `TokenDetailsDrawer.svelte:96` shows `Active Session: model · countdown` via `fmtCountdown`. Gap: **no `fraction` bar** — add.

## 3. Model Selector (`freebuff-model-selector.tsx:202`) — the richest quota view
**Shown on `status==='none'` (pre-join).**

Sections: `PREMIUM | UNLIMITED | REFERRAL | LIMITED_OFFER` (empty filtered). Hero collapsed vs expanded grid.

| Data | Wire → Computed | TUI Display |
|---|---|---|
| `rateLimitsByModel: Record<model, FreebuffSessionRateLimit>` | `getRateLimitsByModel(session)` (`freebuff-session.ts:371`) → `getFreebuffSectionQuotas(rateLimits, accessTier)` (`freebuff-session-pools.ts`) groups by `pool` | Section header `PREMIUM — 2 of 5 used · resets in 22h` (amber when exhausted) |
| `limit, recentCount, period, resetAt, entitlementBreakdown{base,referral,streak}` | `FreebuffSessionRateLimit` `freebuff-session.ts:223` (`limit number, recentCount number, pool?, poolLabel?`) | Row second line `Premium · 2/5 · resets Sep 2 14:00` (`rowDetails`) |
| `referralInfo{referralCode, qualifiedCount, dailySessions, endsAt}` | `getReferralInfo(session)` | `FreebuffReferralBanner`: `Share link freebuff.com/?ref=XXX · 3 qualified · +1/day` |
| `freebucks{balance, daily{limit,spent,remaining,resetAt}, weekly, monthly, bindingWindow}` | `getFreebucksInfo(session)` | `Freebucks $12.5 · Daily 5/20` |
| `glmPromo{dailySessions, endsAt}` | `getGlmPromo(session)` | `GLM 2/day · ends Sep 3` |
| `standing{cappedBy, cappedReason, blurb, nextSteps[]}` | `getStanding(session)` (`freebuff-standing.ts`) | `Trust: capped by hosting · Blurb · Next: link GitHub` |
| `desktopSessionCounts{active, total}` | `FreebuffDesktopSessionCounts` | `Desktop 2/3 tabs` |
| `limitedModelOffers[]{model, spotsLeft}` | `getLimitedModelOffers(session)` (`limitedModelOffers` only while global pool has capacity) | Extra row `Claude Fable 5 · 12 spots left` |
| `streak{currentStreak, longestStreak, bonusNote}` | `useFreebuffStreakQuery` + `freebuff-streak-line.ts` | Landing header `🔥 7 day streak · +1 session` |
| `subscription{plan, usage}` | `useSubscriptionQuery` / `useUsageQuery` (poll 15s/30s) | `Subscription $20 · 34/100 credits` (non-freebuff) |
| `availability` | `getFreebuffModelUnavailableLabel(model)`, `isFreebuffModelAvailable(tier)` | Greyed row `Unavailable 9am-5pm ET` |
| `contextWindow` | `FREEBUFF_MODEL_CONTEXT_WINDOWS[model]` | Row badge `1M` |

**Proxy mapping:** Dashboard `dashboard_models.go:quotaFor` shows `5 premium quota` (limit only); `dashboard_tokens.go` `quotaRow{limit,recent,remaining,period,resetAt,entitlement}` shows per-model correctly via `formatQuota`. Gap: **no section header `N of M used`**, no `entitlementBreakdown` grouping, no `streak/freebucks/standing/limitedOffers` at all.

## 4. Landing Screen (`freebuff-landing-screen.tsx`)
* Hero model (`getRecommendedFreebuffModelId` — `luna` full tier, `mimo` limited) + `takeOverFreebuffSession` button
* `useFreebuffStreakQuery` → streak line (`getFreebuffStreakLine`)
* `formatFreebuffPremiumResetCountdown(getFreebuffPremiumResetAt(session))` → `Resets in 22h 33m`
* `useGravityAd` / `waiting-room-placements` → ad cards
* `refreshFreebuffLandingMetadata` on `07:00 UTC` tick

Gap: proxy landing is 404 — no streak/premium reset UI.

## 5. Usage Banners (`usage-banner.tsx`, `subscription-limit-banner.tsx`, `bottom-banner.tsx`)
* `useUsageQuery` (30s) + `useSubscriptionQuery` (15s) + `useActivityQuery`
* `rateLimit{limit, used, remaining, resetAt}` for a-la-carte, `subscriptionInfo{tier, renewalDate}` → `formatRenewalDate`, progress `getBannerColorLevel` green→red, `generateLoadingBannerText`
* `help-banner.tsx` / `ad-banner.tsx` / `choice-ad-banner.tsx`

Gap: proxy has no usage/subscription — only spend `Spend24h/SpendWeek` ledger (local, not wire).

## 6. Chat Runtime (`chat.tsx`, `chat-store.ts`, `use-send-message.ts:31kb`)
* `runState.sessionState.mainAgentState.contextTokenCount` → context %
* `think-tag-stream.ts` / `think-tag-parser` → leaked `<think>` → `reasoning_content`
* `isStrictReasoningModel` → `reasoning_content` required on tool calls (mimo/deepseek/kimi)
* `statusIndicatorState` machine

Gap: dashboard playground replays `reasoning_content` correctly, but no `think leak` badge.

## 7. Session Lifecycle (`use-freebuff-session.ts:838`)
* `PollController` — `POLL_INTERVAL_ACTIVE_MS 30s`, `nextDelayMs` caps to `remaining+1s` near expiry, backoff `20→300s` on fail, `getFreebuffInstanceId()` = `holdsLiveFreebuffSlot` (active or grace `30m`), `takeOver/refresh/returnToLanding`, `pendingExplicitPickModel`
* `x-freebuff-compact-session` header omits quotas on poll (proxy preserves `savedQuota` #146 — matches CLI `rateLimitsByModel` optional)

Proxy `pool/pool_lifecycle.go` mirrors `30s±20%` `sessionPollTick`/`bridgeSessionPollTick`, grace `30m` (`session/go graceEndFromState`), compact preservation — parity.

## 8. Wire Source of Truth (all TUI fields trace to these)

| Wire Field | Type File | TUI Consumer |
|---|---|---|
| `session.status: active/ended/none/queued/country_blocked` | `freebuff-session.ts:526` | everything |
| `admittedAt, expiresAt, remainingMs` | `FreebuffActiveSessionInfo` | countdown, progress |
| `model` | same | status bar, context window |
| `rateLimitsByModel` | `Record<model, FreebuffSessionRateLimit>` | model selector sections, dashboard quota table |
| `pool, poolLabel, countsAdmissions` | same | section grouping |
| `entitlementBreakdown` | `FreebuffSessionEntitlementBreakdown` | `base=5, referral=0, streak=0` tooltip |
| `referral, glmPromo, freebucks, standing, subscription, limitedModelOffers, desktopSessionCounts` | `freebuff-session.ts:283` | referral banner, freebucks bar, trust, offers |
| `availableHours, availableAt` | `freebuff-models.ts` | grey row |
| `contextWindow` | `freebuff-models.ts` | status bar % |

## 9. Proxy Dashboard Coverage vs Gaps

| TUI Info | Proxy Today | Gap |
|---|---|---|
| **Session progress** (fraction bar, `formatFreebuffSessionRemaining`) | `SessionRemainingSeconds` numeric, no bar, no `isUnlimited` check | Add `fraction` + `isUnlimited` (`!rateLimit`) + `freebuff-session-display` port |
| **Premium pool `N of M used`** | `PremiumQuota USED/Remaining` now float-fixed (`1.2/5`), but **no section header** | Add header `2 of 5 used · resets in 22h` in Models page |
| **Per-model `recent/limit/period/resetAt/entitlement`** | ✅ `quotaRow` correct (`formatQuota` float) | — |
| **Streak** `🔥 7d +1` | ❌ none (only `quotaTracker` bonus note missing) | Add `standing.streak` + `referral.qualifiedCount` → header note |
| **Freebucks** `balance/daily/weekly` | ❌ `FreebucksInfo` parsed (`session_parse.go`) but not surfaced | Add `FreebucksInfo` card |
| **Standing/Trust** `cappedBy, blurb, nextSteps` | `SessionStanding` stored but only `cardFromSnapshot` shows standing badge | Expose full `standing.cappedReason/blurb/nextSteps` |
| **Referral** `code, qualified, promo` | `GlmPromo` synthesized row only | Add `referral.code/link/qualified` banner like TUI |
| **Limited offers** `spotsLeft` | ❌ not parsed | Add `limitedModelOffers` row |
| **Desktop tabs** `2/3` | ❌ not parsed | Add `desktopSessionCounts` |
| **Context usage** `42%` | `contextTokenCount` only in playground, not token card | Add `FREEBUFF_MODEL_CONTEXT_WINDOWS[model]` lookup + `formatContextUsage` |
| **Countdown visible `<5m` warning** | `fmtCountdown` generic | Add `FREEBUFF_COUNTDOWN_VISIBLE_MS 5m` urgent red |
| **Usage/subscription rings** | Spend ledger only (`SpendDay/Week`) | Wire `subscriptionInfo` not proxied — keep spend, add `usageQuery` if upstream exposes |

## 10. Integration Plan (proposed)
1. **Expose missing wire blocks** in `session.SessionSnapshot`: `FreebucksInfo, Standing, Referral, LimitedOffers, DesktopCounts, Streak` (parse in `session_parse.go` already for freebucks/standing/referral, add limitedOffers/desktop).
2. **Extend `pool.TokenSnapshot` + `healthz` + `/admin/api/tokens`** to include them.
3. **Port `formatFreebuffSessionRemaining`/`Countdown`** + `freebuff-session-pools.getFreebuffSectionQuotas` (group by `pool`) to `dashboard_helpers.go` — render `N of M used` header.
4. **New dashboard cards**: `StreakCard`, `FreebucksCard`, `TrustCard`, `ReferralBanner` (copy of `freebuff-referral-banner.tsx` logic), `LimitedOfferRow`.
5. **Status-bar bar**: Svelte `progress` div `width: fraction*100%` + `unlimited` check + `contextUsage`.

All TUI info is `rateLimitsByModel`-centric — proxy already mirrors it correctly (`savedQuota` #146, `graceEndFromState` #240, fractional `0.1` units #70f1d8d). Remaining work is surfacing the 6 omitted blocks.

