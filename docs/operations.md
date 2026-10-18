# Runtime operations

SQLite and the existing REST APIs are authoritative. Background workers and SSE keep derived state and the console current without replacing durable data with in-memory queues.

## Location payloads

New location payloads retain only `_type`, `tst`, `lat`, `lon`, `acc`, `alt`, `vel`, `cog`, and `batt`. Existing payloads remain unchanged. Canonical digest equality is checked before storing a minimized payload.

## Durable workers

Location insertion and TSA/OTS job creation, including the configured attempt limit, share one SQLite transaction. Stamp and verification workers renew 90-second leases every 15 seconds. Expired work is reclaimed and stale owners cannot install or complete proof files. Proof installation is fenced by the same SQLite completion transaction; verification run storage and job completion also share one transaction. Transient failures use persisted exponential backoff.

A new coalesced generation has its own retry budget, including after live lease expiry. Verification captures location metadata and proof bytes under a shared proof lock, then releases the lock before querying external providers.

Shutdown uses one 20-second deadline for HTTP, MQTT, and workers. Incomplete cleanup returns a nonzero exit code; the DB is not closed while workers may still use it.

Proof import commit markers distinguish committed imports from rolled-back duplicate repairs, and append journals tolerate an interrupted final record. Committed recovery skips locations deleted since the import rather than reinstalling their proofs. Proof deletion is recorded transactionally in `proof_gc` and retried after restart. Doctor streams proof-directory checks in bounded batches; its returned missing/orphan lists grow only with reported anomalies. Verification reconciliation processes at most 500 candidates per pass and resumes from its cursor. See [Proof results and rechecks](proofs.md#results-and-rechecks) for trust policy and verification scheduling.

## Maintenance

Backups, retention, OTS upgrades, proof GC, and session cleanup are singleton jobs in SQLite. One worker executes them serially with the same renewable lease model, so scheduled DB and proof mutations cannot overlap. Duplicate manual requests coalesce into one follow-up run. Missed schedules run once after restart rather than replaying every elapsed interval; run summaries are retained for 30 days.

Proof GC keeps every pending deletion in SQLite. Its `remaining` run value counts entries ready at the current time, while `pending` includes entries deferred by retry backoff. Replacement artifacts are scanned once per GC batch in bounded directory chunks. A transient cleanup error records the error and exponential `next_attempt_at`; it does not spin in a tight loop.

Manual backup and OTS upgrade requests return `202` after enqueueing, not after completion. The dashboard shows job state and recent runs; `/api/maintenance-jobs` and `/api/maintenance-runs` expose the same state. Backup and deletion recovery details are in [Backups and recovery](backups.md).

OTS upgrades process a bounded batch with a one-minute wall-time budget. When the budget expires, the successful pass reports `yielded: true` in its summary, retains the cursor and unprocessed failed IDs, and schedules the next pass after one second so other due maintenance can run. Counts that could not be refreshed are omitted from the yielded summary; parent cancellation remains a cancellable worker failure. Unanchored submitted proofs remain pending for a later scheduled pass instead of causing an immediate repeat loop, and transient worker failures use the maintenance job backoff.

Only a successful OTS upgrade or proof GC batch with remaining ready work schedules an immediate follow-up. Failed batches retain their retry backoff and, after exhausting attempts, their normal scheduled cooldown.

## MQTT

MQTT ingest uses the configured `pub_rate_limit` and `pub_rate_window` per user/device; HTTP `/pub` counts requests per client IP separately. Incoming frames are size-checked before packet allocation, allowing the 1 MiB payload limit plus topic and protocol overhead. Connect and subscribe waits are bounded and cancellation-aware; proxy dialing and TLS handshakes share the connection timeout. QoS 1 acknowledgments are sent only after stored, duplicate/ignored, or invalid frames reach terminal handling; transient ingest failures and rate-limit backpressure cancel the current connection without acknowledging the frame so the broker can redeliver it. Persistent sessions (`CleanSession=false`) require a stable client ID to preserve that redelivery across reconnects. Pending frames are filtered against the current topic even before `SUBSCRIBE`; stale frames from a changed topic are acknowledged and discarded.

After connection loss or a rejected subscription, the client retries after five seconds and subscribes again. Connected status requires a successful subscription acknowledgment. Custom CA files also apply to secure WebSocket (`wss://`) brokers.

## Timeline and geofences

Timeline segments are derived asynchronously after committed location changes. Out-of-order imports, retention, deletion, and threshold changes enqueue a persistent rebuild, so restarts do not lose work. Rebuilds use context-aware keyset batches and retain only the active stay-detection window. Range bounds rule out impossible stay candidates before exact distance checks, including long stationary windows followed by movement. Claims rotate across identity jobs so continuously changing devices cannot starve quieter ones. Summaries clip boundary segments to the requested time range and prorate their distance.

Adjacent stays and trips share boundary points so movement into and out of a stay is included once. Gaps never bridge unobserved travel. Incremental rebuilds replay from an unaffected stay's original anchor and preserve its preceding gap; without such a boundary they start at the first location. Startup queues repairs for older cached segments with missing movement boundaries without resetting active or failed jobs. Stay centers use a circular longitude mean across the dateline. Save the Tracking settings once to rebuild previously cached segments after upgrading.

Each timeline response reads its page and full-range summary from one SQLite snapshot and honors request cancellation. Geofence JSON bodies are limited to 64 KiB; larger bodies are rejected rather than truncated.

Geofences are circular and emit only enter/exit events. Location deletion rebuilds only affected identity states. Initial state and historical GPX imports never emit notifications. Webhooks omit coordinates and use the configured HMAC secret; failed deliveries retry from SQLite up to five times. Successful delivery rows are removed immediately and terminal failures are retained for 30 days.

Schema v15 adds indexes on timeline and geofence location references so cascading deletion does not scan each child table for every deleted location. Existing data and archive formats are unchanged.

Webhook URLs must point directly to the receiving endpoint. Redirects are delivery failures; payloads and signatures are never forwarded to another host or downgraded from HTTPS to HTTP.

## SSE and console state

Dashboard and maps subscribe to an in-memory change feed. Events only invalidate affected snapshots and are not replayed. Every UI performs a 60-second safety refresh. Admin events contain bounded topics and location IDs. Public share events are an empty `track` signal and obey the link's user/device, time-window, expiry, and publication-delay policy.

Each share connection retains at most 2,048 delayed notification buckets. Excess buckets are left to the browser's safety refresh and never produce an early signal before the publication delay.

A reverse proxy must stream `/api/events` and `/share/{token}/api/events` without buffering, caching, or compression and flush updates immediately. See the [reverse proxy deployment guide](../deploy/README.md#b-reverse-proxy).

A failed console refresh keeps the last successful snapshot visible, marks it stale, and offers Retry. Queued actions are not reported as completed until their terminal state is observed. If an administrator session expires, automatic queries and the event stream pause. Sign in opens a separate tab; Resume validates the new session, refreshes the CSRF token, and reconnects without replacing the form or replaying actions.

Temporary session or share lookup failures return service-unavailable responses instead of reporting expiration. Expired share links cannot be rotated. Bulk stamp retry immediately refreshes failures and tracks up to 50 visible jobs using exact location queries with at most two concurrent reads; a short follow-up window falls back to SSE and safety refreshes for jobs still pending.

Missing backups and stale proof evidence appear in the health warnings. Status queries propagate request cancellation through DB aggregates and privacy scan waits; canceled checks do not replace successful metric values.

Concurrent status requests share backup verification without holding a mutex during archive reads; a canceled waiter can leave immediately. Provider checks that were not performed, including network failures, appear as `Not checked` rather than invalid PoW. Verification and maintenance worker health metrics expire when their heartbeat is 30 seconds old, matching the readiness threshold. Lease renewal refreshes the heartbeat during long-running work. Pending-job age is evaluated at metric scrape time and excludes terminal failures.

Unsaved settings remain only in the page, not browser storage. Failed saves retain non-secret input and the active tab; secret fields must be entered again. Theme selection follows the operating system until changed manually, then stores only `light` or `dark` in browser local storage. One-time credential and share URLs use accessible dialogs and cannot be revealed again after those dialogs close.

Native ZIP and GPX downloads use a temporary cookie containing only a request identifier to release the helper frame after the browser receives the attachment headers. The page deletes it immediately; any leftover marker expires after one hour.

Timeline pages share a fixed time range and show a full-range summary. Later pages display a change notice instead of moving while new data arrives; Refresh starts again from the first page with a new range. View on map opens the segment's identity and exact Custom period. Maps fit the first non-empty track once, then preserve the viewport during automatic updates; Fit track explicitly adjusts it again. Public shares have their own Fit track control and become unavailable when revoked or expired. Geofences can be edited or enabled/disabled without replacing their IDs or event history.

Automatic dashboard, timeline, and map updates arriving during a query are combined into one follow-up query. Continuous changes do not cancel a slow query before its result can be displayed. Changing filters, navigating timeline pages, or explicitly reloading still cancels an obsolete request. The failed stamp list is filtered on the server; submitted stamp actions are tracked by location, including completed jobs, so a large unrelated backlog cannot hide a failure or imply completion.
