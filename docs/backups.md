# Backups and recovery

mytracker stores verified ZIP bundles with the `.mtbackup` extension in `data_dir/backups/`. These are full server-state backups, not [location exports](../README.md#location-export).

## Bundle contents

```text
manifest.json           # format/schema versions and entry SHA-256 digests
mytracker.db            # consistent SQLite snapshot
proofs/
  1.tsr
  1.ots
```

The bundle contains the complete SQLite DB and the TSA/OTS proof files referenced by its snapshot. The manifest records the schema version and the size and SHA-256 digest of each DB/proof entry.

Verification rejects duplicate archive or manifest entries, unknown manifest fields, invalid proof names, and proofs not referenced by the snapshot. Bundles produced by earlier mytracker releases use the same format.

| Included | Not included |
| --- | --- |
| Locations, jobs, verification history, and web settings | Deployment `config.toml` |
| Administrator password hash and integration credentials | External TLS certificates, private keys, and TSA/MQTT CA files |
| Snapshot-referenced TSA/OTS proofs | Native ACME state in `data_dir/acme/` |
| Stored session records, revoked on restore | Bootstrap `.initial-password` |

Bundles contain credentials and location data. Protect access to them and preserve deployment TOML, external TLS/CA files, and ACME state separately. Do not copy a live SQLite file as a substitute for the backup command.

Creation streams files through a temporary bundle while proof mutation is locked, verifies every digest and SQLite integrity, calls `fsync`, then atomically installs it. A shared OS file lock protects snapshot-referenced proofs across processes, including a CLI backup running alongside the server; proof replacement, import recovery, and GC acquire the exclusive lock.

Proof references, manifest entries, and ZIP directory records are processed incrementally with temporary on-disk indexes instead of loading the complete file list into memory. There is no fixed proof-count limit. Existing size limits remain: 8 GiB per bundle and its total uncompressed contents, 4 GiB for SQLite, and 64 MiB per proof. Creation, verification, and restore need writable temporary storage; restore also checks staging disk space before extraction. `verify-backup` streams its JSON report, while callers requesting a complete in-memory manifest still allocate that result.

## Scheduling and storage

Configure the backup interval and number to keep in **Settings → Operations**, not in deployment TOML. Defaults are 24 hours and 14 bundles. Old backups are pruned only after a newly created bundle passes verification.

Backup jobs use the persistent maintenance scheduler. Duplicate manual requests coalesce into one follow-up run, and a missed schedule runs once after restart rather than replaying every elapsed interval. The dashboard shows job state and recent runs. `POST /api/backups` queues a job and returns `202`; the HTTP response does not mean the backup has finished.

The default local directory is `data/backups/`. With the [supplied systemd layout](../deploy/README.md), it is `/var/lib/mytracker/backups/`. Backup files use mode `0600` and the directory uses `0700`.

## Create and verify

For a local checkout with the default relative data directory:

```sh
# Create a verified DB-and-proof bundle.
./mytracker backup -config config.toml
# Verify a completed bundle, replacing the example filename.
./mytracker verify-backup data/backups/mytracker-....mtbackup
```

For the installed systemd layout, run maintenance commands as the service user from its working directory:

```sh
# Create a verified DB-and-proof bundle.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker backup -config /etc/mytracker/config.toml'
# Verify a completed bundle, replacing the example filename.
sudo -u mytracker /usr/local/bin/mytracker verify-backup /var/lib/mytracker/backups/mytracker-....mtbackup
```

The CLI can create a backup while the server is running. Verification checks archive structure, entry digests, and SQLite integrity; it does not authenticate the source of the bundle.

## Restore

Stop the server first. Restore refuses to run while the server owns the data lock. Run each step only after the previous one succeeds.

For a local checkout, stop its running server process before running:

```sh
# Restore the selected bundle.
./mytracker restore data/backups/mytracker-....mtbackup -config config.toml
# Inspect the restored instance.
./mytracker doctor -config config.toml
# Start the server.
./mytracker -config config.toml
```

For the installed systemd layout:

```sh
# Stop the service.
sudo systemctl stop mytracker
# Restore the selected bundle.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker restore /var/lib/mytracker/backups/mytracker-....mtbackup -config /etc/mytracker/config.toml'
# Inspect the restored instance.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker doctor -config /etc/mytracker/config.toml'
# Start the service.
sudo systemctl start mytracker
```

Restore verifies and stages the full bundle, creates a safety backup of readable current data, then swaps DB and proofs under an `fsync`ed journal. If the current DB is corrupt, uses a newer schema, or is missing while sidecars or proofs remain, restore preserves the available DB, WAL/SHM sidecars, and proof directory in a private `data_dir/.restore-recovery-*` directory instead of requiring the DB to open. Copying this raw recovery state requires additional disk space. The directory is not a verified bundle and is not removed by backup pruning or `privacy-scrub --purge-backups`; review and remove it separately after confirming recovery. Restore prints its retained recovery path.

Creating the restore safety backup never prunes existing backups, even if current settings are unreadable. Subsequent ordinary backups use the restored retention policy.

The staged DB keeps its original schema version; migration occurs on server startup, not during restore. On the next startup, a prepared journal is rolled back and a committed journal is finished automatically. If rollback or cleanup cannot complete, the journal is retained and startup fails with the underlying filesystem error instead of reporting a successful recovery.

Restored administrator sessions are always revoked. Web settings and administrator, OwnTracks, MQTT, and webhook credentials return to the backup state; deployment TOML remains unchanged. A successful restore removes the target's bootstrap password file. External TLS/CA files referenced by settings are not embedded in bundles and must remain accessible at their configured paths.

After startup, check `/readyz` and the dashboard. Schema migrations run on startup; an older binary may not be able to open a database migrated by a newer release. Rolling back an update may require restoring its matching pre-update bundle as well as the old executable.

## Deletion and privacy maintenance

Retention, import, and manual range deletion create a verified safety bundle before destructive work. Manual deletion reports `purge_pending` and preserves recovery backups until proof GC is empty and the WAL truncate checkpoint succeeds. Previously created bundles can retain locations removed from the live DB. If a purge stops after the live deletion, status, doctor, and delete preview report that a purge is pending. Resume with `privacy-scrub --purge-backups`; it removes the root safety bundle only after a new verified backup and successful backup cleanup. Before unlinking the safety bundle, cleanup durably writes `data_dir/.privacy-purge.pending`. If the directory sync confirming backup removal fails, this marker keeps the purge pending even when the safety file is no longer visible. After removal is durable, failure to sync the final marker removal only logs a warning; a marker that reappears after a crash can safely repeat cleanup. The verified replacement backup is retained throughout.

An ordinary web range deletion also reports any already-pending privacy cleanup. The deletion can return `202` with `purge_pending:true` after the selected locations are deleted; it does not remove the existing safety backup, cleanup marker, or recovery backup. A state inspection error is reported conservatively the same way so a caller never mistakes an unknown cleanup state for a clean one.

Backup pruning is deferred while a privacy safety bundle or cleanup marker remains, proof GC is pending, or the WAL truncate checkpoint cannot complete, even when `backup_keep` is reached. Safety backups for explicit privacy purge never prune existing bundles. The newly created bundle is always retained, including after a clock correction leaves older bundles with future-dated filenames. Location deletion also invalidates overlapping timeline segments and revokes affected rebuild leases in the deletion transaction, so an older in-flight rebuild cannot publish deleted data again.

Latest-backup checks use file modification times rather than filename order and prefer non-future times after a clock correction. When initializing the schedule, future-dated backups make backup due immediately. Persisted future scheduling baselines likewise trigger an immediate backup instead of waiting for that timestamp.

Range deletion and retention freeze their maximum location ID before creating the safety bundle. Locations arriving afterward are left for a later deletion pass, even if their reported timestamps fall inside the selected range. Backup creation, pruning, and manual deletion share a filesystem lock so a concurrent CLI or scheduled backup cannot remove the bundle a purge is preserving. Status and doctor take a shared backup lock during inspection so pruning cannot invalidate the selected file; doctor does not create lock files.

For a stronger local deletion pass, stop the server and run `privacy-scrub`. It takes an exclusive data lock, clears derived timeline segments and queues their rebuild, checkpoints the WAL, vacuums SQLite, and removes orphan proofs. Timeline segments are rebuilt after the server restarts. Add `--purge-backups` to leave only one newly verified backup.

If proof GC is still pending, or any scrub, backup, or backup-purge step fails, `privacy-scrub` exits with an error and preserves recovery backups and pending purge state. A pending GC row may be waiting for its next retry time; it is not proof that deletion has completed.

For a local checkout, after stopping its server process:

```sh
# Scrub local data without removing existing backups.
./mytracker privacy-scrub -config config.toml
```

To also purge old backups:

```sh
# Scrub local data and leave one newly verified bundle.
./mytracker privacy-scrub --purge-backups -config config.toml
```

For the installed systemd layout:

```sh
# Stop the service.
sudo systemctl stop mytracker
# Scrub local data and leave one newly verified bundle.
sudo -u mytracker sh -c 'cd /var/lib/mytracker-bootstrap && exec /usr/local/bin/mytracker privacy-scrub --purge-backups -config /etc/mytracker/config.toml'
# Start the service.
sudo systemctl start mytracker
```

Omit `--purge-backups` in the installed command to retain existing bundles.

## Off-host copies

Sync completed bundles rather than the live DB. Run synchronization under an account that can read the private backup directory and access your configured restic/rclone repository:

```sh
restic backup /var/lib/mytracker/backups
# or
rclone sync /var/lib/mytracker/backups remote:mytracker
```

## Recovery runbook

1. Stop ingest with `sudo systemctl stop mytracker`, or stop the local server process.
2. Run `mytracker doctor -config ...` as the service user from its working directory. It only reports and never initializes, migrates, or repairs data. Missing data is reported as `uninitialized`; an older schema or pending restore journal is reported as `migration_required` or `recovery_required`. These transitions are performed by server startup, not by `doctor`.
3. Preserve the complete `data_dir`, including `.restore-journal.json` and staging paths.
4. Verify candidate bundles with `mytracker verify-backup`.
5. Follow [Restore](#restore); do not manually remove restore journal files.
6. Run `doctor` again, start the service, then check `/readyz` and the dashboard.
