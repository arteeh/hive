# Webhooks: cadence, patching, and reconciliation

Hive continues polling when GitHub webhooks are unavailable. Signed deliveries
accelerate updates; the governor's polling cadence remains the fallback. This
page describes the current `v5` behavior and the separate accuracy contract for
[the ForgeState migration (#13373)](https://github.com/hivecommons/hive/issues/13373).

## Three different clocks

| Mechanism | What it does | Cadence |
| --- | --- | --- |
| Governor evaluation | Enumerates actionable work and decides which agents may run. Invalidating webhooks can request an early evaluation through the same gates. | `governor.eval_interval_s` (default `300` seconds), stretched while webhooks are healthy or the API budget requires backoff. |
| Webhook patch | Confirms the named issue or PR with a targeted GitHub read and patches an existing activity-monitor row. PR events also invalidate detail/check/review caches. | `dashboard.stage_tracker.webhook_refresh_debounce_ms` (default `2000` milliseconds), independent of the governor tick. |
| ForgeState reconcile (migration target) | Authoritatively compares the complete issue/PR list with the per-repository cache, repairs missed events, and records drift. | The epic specifies `reconcile_interval_s`, default `900` seconds, with a shorter interval when drift is high. This is separate from the governor interval. |

An agent's own cadence controls when that agent is eligible to run. It does not
set any of these data-refresh intervals. Likewise, the browser's tracker refresh
reads the spoke's snapshot; it does not poll GitHub.

Today, the governor still enumerates work on evaluation cycles. A webhook wake
can reuse clean PR detail and GraphQL batch caches, but it is not a guarantee of
zero repository-list or search calls. ForgeState's single shared cache and reader
migrations are tracked in #13374–#13379; the independent reconcile setting above
is the epic's design, not a setting supplied by this documentation change.

## Automatic outage fallback and recovery

The health window is **twice `governor.eval_interval_s`**. A signature-verified
delivery for a configured repository refreshes its liveness timestamp. Unknown
repositories and rejected signatures do not refresh it. Without any accepted
delivery, the loop starts in `poll` mode.

While deliveries are healthy, the governor uses the larger of the API-budget
interval and `governor.eval_interval_webhook_s` (default `900` seconds), capped
by `governor.eval_interval_max_s` (default `1800`). Invalidating deliveries also
request early cycles: `governor.webhook_wake_debounce_s` defaults to `10` seconds
and `governor.webhook_wake_min_gap_s` to `30` seconds. These controls coalesce
bursts and bound evaluation frequency; they do not delay the separate tracker
patch by that amount.

When the health window expires, a one-shot health timer rechecks liveness just
after the boundary (a one-second margin). It restores the polling interval
without waiting for the stretched webhook tick. The recheck itself does not run
an evaluation: the reset ticker schedules the next poll. Ongoing evaluation
work can delay handling a timer. The fallback keeps the API-budget multiplier
and maximum interval; it never bypasses rate limiting or action gates.

For example, with `eval_interval_s: 60`, `eval_interval_webhook_s: 900` and a
normal API budget, the health window closes 120 seconds after the last delivery.
The timer rechecks at about 121 seconds, resets the tick to 60 seconds, and the
next polling evaluation is due about 181 seconds after that delivery. Backoff
or a busy loop can extend those times.

Once deliveries resume, the next evaluation or health recheck restores webhook
pacing and arms the health timer again. An invalidating delivery requests that
evaluation through the debounced wake; a delivery that only records liveness
(such as `ping`) can wait for the next scheduled poll. No restart or manual mode
switch is required.

The process-wide health indicator uses the **most recent delivery from any
managed repository**. PR-cache health is checked per repository: a quiet repo
resumes TTL-bound detail reads and GraphQL batch refreshes even if another repo
keeps the process in webhook mode. Thus `webhooks.healthy: true` is neither proof
of delivery coverage for every repository nor proof that no events were lost.
A quiet repository and a broken delivery path both age out conservatively.

## Accuracy and failure handling

A webhook is a hint to re-read state, not an authoritative snapshot. The current
tracker patch fetches only the named item through the paced REST transport.
Successful reads update existing rows; closed or merged items leave their lists.
New or reopened items absent from the snapshot wait for a subsequent enumeration.
The patch does not update the governor's actionable set by itself.

Targeted reads are skipped when the API budget is critical. If a read fails,
that patch leaves the existing row intact and the next successful evaluation
can repair it. Delivery delay, debounce, transport pacing, API errors, and
critical-budget holds mean the current tracker path has no unconditional
10-second freshness guarantee. Loss of webhook health also restores the normal
PR-detail TTL and disables clean-repository GraphQL batch skipping.

The ForgeState epic defines the stronger end-to-end contract below. These are
migration acceptance criteria, not guarantees obtained merely by enabling
webhooks or setting a faster governor cadence:

- A closed issue leaves both the tracker and governor set within 10 seconds;
  out-of-order open/close deliveries converge to the correct state through
  confirming reads and monotonic updates. Reopened items are added again.
- A failed reconcile, including a 403 partway through, preserves the last-good
  snapshot and marks it stale. The overview shows staleness instead of replacing
  counts with zeros.
- Hive's own mutations write through before the next cache read. Irreversible
  actions (merge, close, claim, kick) use a read-through freshness bound of at
  most 60 seconds and refuse stale snapshots.
- Startup completes a full reconcile before the first governor decision pass.
  Later reconciles repair missed events, measure drift, and shorten their
  interval when drift exceeds the threshold, with a dashboard warning.
- Tracker rows expose `fetched_at` and their source (`webhook` or `reconcile`),
  so operators can distinguish an updated display from recently confirmed data.

Polling fallback restores opportunities to discover changes; it cannot promise
fresh data while GitHub itself is unreachable or denying reads. A successful
webhook receipt also does not prove that its confirming read succeeded.

## Operator checks

For endpoint, signing-secret, subscriptions, and hub-relay setup, see
[Webhook-driven PR cache invalidation](operator-reference.md#webhook-driven-pr-cache-invalidation).

Inspect these fields on the spoke:

| Surface | Fields and interpretation |
| --- | --- |
| `GET /api/status` | `evalMode` (`poll` or `webhook`) and `evalIntervalS` show the loop's effective pacing. `lastWakeReason`, `lastWakeAt`, and `webhookWakes` describe early evaluations. |
| `GET /api/status` and `GET /api/gh-rate-limits` | `webhooks.healthy`, `last_event_at`, and `events_1h` describe accepted delivery liveness. `invalidations_1h`, `targeted_refreshes_1h`, and `tracker_invalidations_1h` describe cache activity; targeted refreshes count attempts, not successful confirmations. |
| `GET /api/gh-rate-limits` | `graphql_pr_batch.webhook_skips` counts batches reused while a repository is healthy and clean. |

The loop logs `github webhooks went stale; eval mode poll` once on an outage
transition, and `github webhooks healthy; eval mode webhook` once on recovery.
Repeated polls during the same outage do not repeat the transition warning.

If deliveries stop, compare `last_event_at` with twice the configured polling
interval and check the effective mode after that window. Check GitHub's delivery
history, the hub relay, and the spoke receiver if events should be arriving.
A receiver response of 503 indicates a missing `GITHUB_WEBHOOK_SECRET`; 401
indicates a bad signature. After valid deliveries resume, verify that the
liveness timestamp advances and the loop returns to webhook pacing. If rows
remain old despite accepted deliveries, inspect the API budget and targeted
read failures rather than treating receipt as confirmation of freshness.
