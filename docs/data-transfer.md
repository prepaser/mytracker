# Data transfer and independent verification

Use native ZIP exports for location records and proofs, GPX for track interchange, and [backup bundles](backups.md) for complete server-state recovery.

## Native ZIP export

Choose **Settings → Download export**, or use authenticated `GET /api/export`. Optional query parameters are `hours`, `from` (Unix time), and `to` (Unix time).

The archive is for submission or review, not a server backup:

```text
manifest.json
README.txt
locations.jsonl    # one record per line, including the stored OwnTracks payload
locations.csv      # spreadsheet-oriented location data
verification_runs.jsonl
verification_attempts.jsonl
proofs/
  1.tsr            # RFC 3161 timestamp
  1.ots            # OpenTimestamps proof
```

Verification history is streamed with selected locations and remapped during import. Exports preserve the expected TSA nonce and verification policy metadata and record a SHA-256 digest for every archive entry except the manifest itself. Historical verification digests describe the proof used by each run and may differ from the archived proof after an OTS upgrade. The manifest checks each current proof file. Deployment `config.toml`, the SQLite DB, and application settings are not included.

CSV user and device cells with spreadsheet formula prefixes are escaped for display; identities remain unchanged in JSONL and proof digests. Location rows and history use keyset iteration from one SQLite read snapshot. Concurrent ingest does not appear partway through an export. ZIP directory records and manifest digests are spooled to temporary files and SQLite, so memory use does not grow with the number of proofs. Temporary storage must accommodate the generated archive and its indexes. Proof replacement and GC are locked for the snapshot, keeping DB rows and archived proof files consistent.

Web ZIP and GPX downloads prepare a temporary file before sending it. The DB snapshot is released before file delivery. File generation honors request cancellation and has a 30-minute context deadline; delivery renews a 120-second write deadline for each chunk, so a progressing download can exceed two minutes while a stalled write still times out. A reverse proxy needs compatible download timeouts.

## Native ZIP import

`POST /api/import` accepts `multipart/form-data` with the archive in field `file`. Import validates archive structure, manifest digests, and `digest_sha256` against canonical payload hashing before committing data. Existing locations are skipped and proof files are restored.

Imported proofs are queued for a fresh check. Imported runs receive `engine_version: 0` and remain historical evidence, not trusted current results; only native verification records the local engine version. See [TSA and OTS proofs](proofs.md#results-and-rechecks).

Validation walks the ZIP directory and manifest incrementally, indexing entry offsets, digests, and location mappings in temporary SQLite instead of retaining archive-scale indexes in memory. Active temporary indexes hold advisory file locks, so cleanup preserves them while the operation is running; crash leftovers older than 24 hours are removed by the temporary-artifact scavenger.

Proof installation and recovery stream their journals instead of retaining a proof-sized file list. Cancellation stops forward installation and file hashing; rollback finishes under the proof mutation lock before the request returns. After a successful DB commit, already-installed proofs are not rehashed during journal cleanup.

Safety backups and deletion recovery are described in [Deletion and privacy maintenance](backups.md#deletion-and-privacy-maintenance).

### Archive limits

Export and import use the same limits. An export that exceeds one fails instead of producing an archive mytracker cannot import.

| Item | Limit |
| --- | --- |
| Locations | 250,000 |
| Proof entries | 500,000 |
| Individual proof | 8 MiB |
| Total proof bytes | 1 GiB |
| Verification runs | 500,000 |
| Provider attempts | 1,000,000 |
| Streamed data entry | 256 MiB |
| Individual JSON record | 2 MiB |
| ZIP directory metadata | 64 MiB |

Native export archives use these limits, and import and proof-audit readers check actual central-directory counts and enforce the 64 MiB directory limit before walking ZIP entries. Backup bundles use separate scalable archive limits. Web import's compressed size is additionally limited by `import_max_bytes` in deployment TOML, defaulting to 64 MiB.

## Independent proof audit

Use the CLI to verify export proofs with the [same strict policy](proofs.md), independently of archived verification results:

```sh
# Audit using the configured trust policy.
./mytracker verify-export mytracker-export-....zip --proofs -config config.toml
# Produce a text report.
./mytracker verify-export mytracker-export-....zip --proofs --format text
# Check archive integrity only, without external requests.
./mytracker verify-export mytracker-export-....zip
```

Proof auditing reads deployment TOML and its DB settings without migration or credential initialization. Missing TOML uses default deployment paths; a missing DB or settings uses the default verification policy. An unreadable DB or invalid stored settings is an error.

The CLI spools location results to a private temporary file instead of retaining the complete report in memory. JSON and text output begin only after archive validation and proof auditing finish. Temporary storage must accommodate the report; the spool is removed on success or failure.

| Exit code | Meaning |
| --- | --- |
| `0` | Every proof is strictly verified |
| `2` | Only degraded, pending, or inconclusive proof results exist, or the archive contains no locations |
| `1` | Archive error, or at least one invalid, missing, or errored proof |

Without `--proofs`, `verify-export` remains an offline archive integrity check rather than a cryptographic proof audit.

Both verification commands clean up unlocked temporary artifacts left by crashes once they are older than 24 hours. Normal completion removes their own temporary files immediately.

## Standalone browser verifier

Generate a self-contained verifier from the source tree:

```sh
# Generate with the default verification configuration.
go run ./cmd/verifygen -out index.html
# Alternatively, embed public TSA roots and allowed policy OIDs.
go run ./cmd/verifygen -out index.html -tsa-roots tsa-roots.pem -tsa-policy 1.2.3.4
```

Open the included `index.html` directly in a browser, or serve it as a static file. The generator embeds the Go WebAssembly verifier, its matching runtime, styles, and UI scripts; no mytracker server or companion files are required. `make verifier` regenerates this file from source. `make verifier-test` runs desktop/mobile checks using the existing `e2e` dependencies.

The verifier follows a single-column file-to-result flow: choose an export archive, review the visible trust and online-check policy, run verification, and inspect the report. Verification methods, trust assumptions, and offline/online limits remain available under **Method and limitations** instead of preceding the file input.

After verification, **Print summary** produces a compact report for the whole run. It includes archive and location counts, status counts across all records, the policy actually used, and the verification time. It does not print individual location rows; use the on-screen report or JSON download when record-level details are required.

Select a current export ZIP and public TSA trust roots. Entry hashes and canonical location digests are checked locally before proof verification. Checksums establish archive consistency, not authenticity, and archived verification results are not accepted as fresh evidence. Browser verification accepts at most 64 MiB compressed and 256 MiB decompressed; use the CLI for larger archives.

TSA verification uses the Go engine with supplied trust roots, policy OIDs, and archived nonce. It cannot use the browser's private OS trust store. OTS verification reuses local Bitcoin header, PoW, Merkle, and confirmation checks. Online OTS queries are off by default; enabling them sends block queries to selected Esplora providers, which can see the browser IP. Providers must allow browser CORS requests. Network or CORS failures remain inconclusive or errors, never successful verification.

Archives and credentials are not uploaded or saved in browser storage. A JSON report can be downloaded without coordinates, user/device identities, or provider URLs. Cancel stops the verifier worker and outstanding requests. OCSP/CRL and a full Bitcoin header chain are not checked.

## GPX interchange

GPX import streams every track point through one SQLite transaction. Each point must have valid coordinates and an RFC 3339 time. A malformed point or duplicate timestamp inside the file rejects the whole import; points already present for the selected user/device are skipped and reported. Imported locations use the normal proof queues without retaining an archive-sized in-memory point list.

The XML parser limits tokens outside track-point contents to 64 KiB, including the `<trkpt>` opening tag. A track point's remaining XML is limited to 256 KiB. Exceeding either limit rejects the whole import.

User and device values must each contain 1-128 valid UTF-8 bytes with no surrounding whitespace or control characters. The web UI and CLI use the same validation, preserving native ZIP compatibility.

GPX export bounds must be nonnegative Unix timestamps. Zero leaves that bound open; when both bounds are set, `from` must not exceed `to`. The CLI rejects invalid ranges before opening application data or writing the output file.

```sh
# Import a track for an explicit identity.
./mytracker import-gpx track.gpx --user alice --device phone
# Export a filtered track.
./mytracker export-gpx track.gpx --user alice --device phone --from 1750000000
```

The corresponding authenticated API paths are `GET /api/export/gpx` and `POST /api/import/gpx`. GPX exports contain latitude, longitude, time, and optional elevation. Proofs and private OwnTracks payload fields remain in the native export format.
