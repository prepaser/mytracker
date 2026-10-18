# API Reference

The web UI uses the REST endpoints below and SSE change notifications. SQLite remains the source of truth; see [Runtime operations](operations.md#sse-and-console-state) for event-stream and recovery behavior.

## Authentication and Errors

Administrator endpoints require a signed-in administrator session. Requests other than `GET` and `HEAD` also require CSRF validation. Public share endpoints require a valid, unexpired share token but no administrator login.

API errors use `{"error":{"code":"...","message":"...","request_id":"..."}}`. API version aliases and deprecation headers are not used.

OwnTracks ingestion uses `POST /pub` with Basic Auth. See [OwnTracks app setup](../README.md#owntracks-app-setup) for the credential URL and identity headers.

## Public Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Health check: `{"ok":true,"stamp_queue":N}`; no authentication |
| `GET` | `/readyz` | Readiness check: DB, storage directories, workers, and durable queues; no authentication |
| `GET` | `/share/{token}/api/track` | GeoJSON for share link viewers |
| `GET` | `/share/{token}/api/events` | Privacy-filtered public SSE track invalidation |

## Administrator Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/track` | GeoJSON track and points (`hours`, `user`, `device`, `from`, `to`, `limit`) |
| `GET` | `/api/timeline` | Persistent trip/stay segments with full-range summary and keyset pagination (`from`, `to`, `user`, `device`, `limit`, `cursor`); UI pages use 100 rows |
| `GET` | `/api/geofences` | List circular geofences |
| `POST` | `/api/geofences` | Create a circular geofence |
| `PATCH`, `DELETE` | `/api/geofences/{id}` | Update or delete a geofence |
| `GET` | `/api/geofence-events` | Recent enter/exit history |
| `GET` | `/api/stats` | Location and stamp statistics |
| `GET` | `/api/locations` | Cursor-paginated `{data,next_cursor}` summaries (`limit`, `cursor`); payload, digest, and TSA nonce are omitted |
| `GET` | `/api/identities` | Current distinct user/device pairs for live map filters |
| `GET` | `/api/status` | Locations, MQTT, workers, queues, proof GC, proof verification, providers, and backup status |
| `GET` | `/api/events` | SSE change feed for the admin UI |
| `GET` | `/api/stamp-jobs` | Stamp jobs (`state`, `limit`), or all TSA/OTS job states for `location_id` |
| `GET` | `/api/verification-jobs` | Proof verification jobs (`state`, `limit`) |
| `GET` | `/api/maintenance-jobs` | Durable maintenance jobs (`kind`, `state`) |
| `GET` | `/api/maintenance-runs` | Cursor-paginated 30-day maintenance run history |
| `POST` | `/api/maintenance-jobs/{id}/retry` | Retry a failed maintenance job |
| `POST` | `/api/stamps/{id}/retry` | Re-queue a location's failed/pending stamps |
| `POST` | `/api/stamps/retry` | Retry all failed stamps |
| `POST` | `/api/backups` | Queue a [backup](backups.md#scheduling-and-storage) and return `202` |
| `GET` | `/api/verify/{id}` | Read the latest stored TSA/OTS verification and current jobs |
| `POST` | `/api/verify/{id}` | Queue both proofs and return `202` with job states |
| `GET` | `/api/verify/{id}/history` | Cursor-paginated 90-day verification history |
| `GET` | `/api/export` | Location archive ZIP (`hours`, `from`, `to`) |
| `POST` | `/api/import` | Import an export ZIP (`multipart/form-data`, field `file`) |
| `GET` | `/api/export/gpx` | GPX 1.1 track (`from`, `to`, `user`, `device`) |
| `POST` | `/api/import/gpx` | Atomic GPX import with explicit `user` and `device` |
| `GET` | `/api/delete-preview` | Preview deletion of a time range |
| `POST` | `/api/delete` | Delete a time range after a verified backup |
| `POST` | `/api/ots/upgrade` | Queue an OTS proof upgrade and return `202` |
| `GET`, `POST` | `/api/shares` | List or create share links |
| `DELETE` | `/api/shares/{id}` | Revoke a share link |
| `POST` | `/api/shares/{id}/rotate` | Rotate a share link and return a replacement URL once |
| `GET` | `/api/sessions` | List server-side administrator sessions |
| `DELETE` | `/api/sessions/{id}` | Revoke an administrator session |
| `POST` | `/api/sessions/revoke-others` | Revoke every session except the current one |
| `POST` | `/api/owntracks-url` | Reveal the credential URL after CSRF validation |
| `POST` | `/api/settings` | Save the Settings form with CSRF and `settings_revision`; return `{data:{redirect}}` on success |
| `GET` | `/metrics` | Prometheus metrics |

See [Data transfer and independent verification](data-transfer.md) for export/import formats and limits, [Backups and recovery](backups.md) for backup and deletion behavior, and [TSA and OTS proofs](proofs.md) for verification policy and result meanings.

`POST /api/delete` returns `{"deleted":N,"purge_pending":false}` with `200` after a completed range deletion when no privacy cleanup is pending. If a prior safety backup or cleanup marker remains, the range deletion still completes, but the response reports `purge_pending:true` with `202`; the existing marker and recovery backups are preserved for the cleanup pass. The same `202` response is used when the cleanup state cannot be inspected reliably.

Geofence writes return `400` for malformed input, invalid fields, or the 100-geofence limit, `404` for a missing geofence, and `500` for storage failures. Server errors use the generic `internal error` message.

Settings saves return `400` for invalid fields and `409` when the submitted revision is stale. A post-commit reload failure returns `500` with `error.committed:true` and `error.settings_revision`; retry using that revision. Authentication failures return `401` without navigating away from unsaved inputs.

Successful statistics responses calculate the total and status counts from the same database view, including when locations change during the request.

## Track Sampling

`GET /api/track` applies deterministic trajectory sampling when the matching range exceeds `limit` (default 5,000, minimum 2, maximum 20,000). The limit is the maximum displayed point count, not an early cutoff, so the first and latest matching locations remain represented. Sampling never calculates a trajectory between different user/device identities. Tracks use `LineString` for a single identity and `MultiLineString` for multiple identities. If the identity count exceeds `limit`, bounded temporal sampling is used instead (`algorithm: temporal-v1`). The GeoJSON `FeatureCollection` includes:

```json
{"metadata":{"total_points":120000,"returned_points":5000,"sampled":true,"algorithm":"trajectory-v1"}}
```

Trajectory sampling unwraps longitude separately for each identity across the dateline; returned coordinates are unchanged. Share maps use the same sampling after applying the link's coordinate precision policy, so selection does not depend on undisclosed coordinate detail. Public track queries are limited to eight concurrent requests overall and two per share, with a 30-second query timeout. Busy requests return `503` with `Retry-After: 10`.
