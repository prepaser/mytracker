package store

import (
	"database/sql"
	"fmt"
)

const CurrentSchemaVersion = 15

type migration struct {
	version int
	name    string
	up      func(*sql.Tx) error
}

var migrations = []migration{
	{1, "initial schema", migrateV1},
	{2, "share privacy", migrateV2},
	{3, "stamp jobs", migrateV3},
	{4, "stamp leases and proof gc", migrateV4},
	{5, "hashed shares and sessions", migrateV5},
	{6, "proof verification history", migrateV6},
	{7, "strict proof policy", migrateV7},
	{8, "proof verification jobs", migrateV8},
	{9, "maintenance jobs", migrateV9},
	{10, "location range indexes", migrateV10},
	{11, "timeline and geofences", migrateV11},
	{12, "non-reusable location ids", migrateV12},
	{13, "proof import commit markers", migrateV13},
	{14, "application settings", migrateV14},
	{15, "foreign key child indexes", migrateV15},
}

func migrateV13(tx *sql.Tx) error {
	return execMigration(tx, 13, `CREATE TABLE proof_import_commits (token TEXT PRIMARY KEY)`)
}

func migrateV14(tx *sql.Tx) error {
	return execMigration(tx, 14, `
		CREATE TABLE app_settings (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			json TEXT NOT NULL CHECK(typeof(json) = 'text' AND json_valid(json)),
			updated_at INTEGER NOT NULL
		)
	`)
}

func migrateV15(tx *sql.Tx) error {
	return execMigration(tx, 15, `
		CREATE INDEX IF NOT EXISTS idx_timeline_segments_start_location ON timeline_segments(start_location_id);
		CREATE INDEX IF NOT EXISTS idx_timeline_segments_end_location ON timeline_segments(end_location_id);
		CREATE INDEX IF NOT EXISTS idx_geofence_states_last_location ON geofence_states(last_location_id);
		CREATE INDEX IF NOT EXISTS idx_geofence_events_location ON geofence_events(location_id);
	`)
}

func (s *Store) migrate() error {
	if err := validateMigrations(migrations); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > CurrentSchemaVersion {
		return fmt.Errorf("database schema v%d is newer than supported v%d", version, CurrentSchemaVersion)
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if err := m.up(tx); err != nil {
			return fmt.Errorf("migrate to v%d (%s): %w", m.version, m.name, err)
		}
	}
	return tx.Commit()
}

func validateMigrations(ms []migration) error {
	if len(ms) != CurrentSchemaVersion {
		return fmt.Errorf("migration registry has %d entries, want %d", len(ms), CurrentSchemaVersion)
	}
	for i, m := range ms {
		want := i + 1
		if m.version != want {
			return fmt.Errorf("migration %d has version %d", want, m.version)
		}
		if m.name == "" || m.up == nil {
			return fmt.Errorf("migration v%d is incomplete", m.version)
		}
	}
	return nil
}

func execMigration(tx *sql.Tx, version int, schema string) error {
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", version))
	return err
}

func migrateV1(tx *sql.Tx) error {
	return execMigration(tx, 1, `
		CREATE TABLE IF NOT EXISTS locations (
			id INTEGER PRIMARY KEY,
			user TEXT NOT NULL,
			device TEXT NOT NULL,
			tst INTEGER NOT NULL,
			lat REAL NOT NULL,
			lon REAL NOT NULL,
			payload BLOB NOT NULL,
			digest BLOB NOT NULL,
			tsa_time INTEGER,
			tsa_status TEXT NOT NULL DEFAULT 'pending',
			ots_status TEXT NOT NULL DEFAULT 'pending',
			created_at INTEGER NOT NULL,
			UNIQUE(user, device, tst)
		);
		CREATE INDEX IF NOT EXISTS idx_locations_created ON locations(created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_locations_tst ON locations(tst);
		CREATE INDEX IF NOT EXISTS idx_locations_user_device ON locations(user, device);
		CREATE TABLE IF NOT EXISTS share_links (
			id INTEGER PRIMARY KEY,
			token TEXT NOT NULL UNIQUE,
			label TEXT NOT NULL DEFAULT '',
			user TEXT NOT NULL DEFAULT '',
			device TEXT NOT NULL DEFAULT '',
			hours INTEGER NOT NULL DEFAULT 24,
			expires_at INTEGER,
			created_at INTEGER NOT NULL,
			last_access_at INTEGER
		);
		CREATE INDEX IF NOT EXISTS idx_share_token ON share_links(token);
	`)
}

func migrateV2(tx *sql.Tx) error {
	for _, q := range []string{
		`ALTER TABLE share_links ADD COLUMN delay_minutes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE share_links ADD COLUMN precision_m INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`PRAGMA user_version=2`)
	return err
}

func migrateV10(tx *sql.Tx) error {
	return execMigration(tx, 10, `
		DROP INDEX IF EXISTS idx_locations_tst;
		DROP INDEX IF EXISTS idx_locations_user_device;
		CREATE INDEX IF NOT EXISTS idx_locations_tst_id ON locations(tst,id);
		CREATE INDEX IF NOT EXISTS idx_locations_user_tst_id ON locations(user,tst,id);
		CREATE INDEX IF NOT EXISTS idx_locations_device_tst_id ON locations(device,tst,id);
	`)
}

func migrateV11(tx *sql.Tx) error {
	return execMigration(tx, 11, `
		CREATE TABLE IF NOT EXISTS timeline_segments (
			id INTEGER PRIMARY KEY,
			user TEXT NOT NULL,
			device TEXT NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('stay','trip')),
			start_location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			end_location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			start_tst INTEGER NOT NULL,
			end_tst INTEGER NOT NULL,
			center_lat REAL,
			center_lon REAL,
			distance_m REAL NOT NULL DEFAULT 0,
			point_count INTEGER NOT NULL,
			gap_before INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_timeline_segments_range ON timeline_segments(start_tst,end_tst,id);
		CREATE INDEX IF NOT EXISTS idx_timeline_segments_identity ON timeline_segments(user,device,start_tst,id);
		CREATE TABLE IF NOT EXISTS timeline_rebuild_jobs (
			id INTEGER PRIMARY KEY,
			user TEXT NOT NULL,
			device TEXT NOT NULL,
			dirty_from INTEGER NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('queued','running','retry','failed')),
			generation INTEGER NOT NULL DEFAULT 1,
			claimed_generation INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(user,device)
		);
		CREATE INDEX IF NOT EXISTS idx_timeline_rebuild_ready ON timeline_rebuild_jobs(state,next_attempt_at,lease_until,id);
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
			SELECT user,device,MIN(tst),'queued',strftime('%s','now'),strftime('%s','now')
			FROM locations GROUP BY user,device;
		CREATE TABLE IF NOT EXISTS geofences (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			lat REAL NOT NULL,
			lon REAL NOT NULL,
			radius_m REAL NOT NULL CHECK(radius_m BETWEEN 25 AND 100000),
			user TEXT NOT NULL DEFAULT '',
			device TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS geofence_states (
			geofence_id INTEGER NOT NULL REFERENCES geofences(id) ON DELETE CASCADE,
			user TEXT NOT NULL,
			device TEXT NOT NULL,
			inside INTEGER NOT NULL,
			last_tst INTEGER NOT NULL,
			last_location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			PRIMARY KEY(geofence_id,user,device)
		);
		CREATE TABLE IF NOT EXISTS geofence_events (
			id INTEGER PRIMARY KEY,
			geofence_id INTEGER NOT NULL REFERENCES geofences(id) ON DELETE CASCADE,
			location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			user TEXT NOT NULL,
			device TEXT NOT NULL,
			event TEXT NOT NULL CHECK(event IN ('enter','exit')),
			tst INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_geofence_events_recent ON geofence_events(tst DESC,id DESC);
		CREATE TABLE IF NOT EXISTS geofence_deliveries (
			id INTEGER PRIMARY KEY,
			event_id INTEGER NOT NULL UNIQUE REFERENCES geofence_events(id) ON DELETE CASCADE,
			state TEXT NOT NULL CHECK(state IN ('queued','running','retry','done','failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_geofence_delivery_ready ON geofence_deliveries(state,next_attempt_at,lease_until,id);
	`)
}

func migrateV12(tx *sql.Tx) error {
	return execMigration(tx, 12, `
		CREATE TABLE IF NOT EXISTS location_id_allocator (
			id INTEGER PRIMARY KEY AUTOINCREMENT
		);
		INSERT INTO location_id_allocator(id)
			SELECT MAX(id) FROM (
				SELECT id FROM locations
				UNION ALL
				SELECT CAST(substr(path, 1, length(path) - 4) AS INTEGER)
				FROM proof_gc
				WHERE substr(path, -4) IN ('.tsr', '.ots')
					AND length(path) > 4
					AND substr(path, 1, length(path) - 4) NOT GLOB '*[^0-9]*'
			) HAVING MAX(id) > 0;
	`)
}

func migrateV3(tx *sql.Tx) error {
	return execMigration(tx, 3, `
		CREATE TABLE stamp_jobs (
			id INTEGER PRIMARY KEY,
			location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('tsa','ots')),
			state TEXT NOT NULL CHECK(state IN ('queued','running','retry','done','failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 3,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			UNIQUE(location_id, kind)
		);
		CREATE INDEX idx_stamp_jobs_ready ON stamp_jobs(state,next_attempt_at,id);
		INSERT OR IGNORE INTO stamp_jobs(location_id,kind,state,updated_at)
			SELECT id,'tsa',CASE WHEN tsa_status='ok' THEN 'done' WHEN tsa_status='fail' THEN 'failed' ELSE 'queued' END,strftime('%s','now')
			FROM locations;
		INSERT OR IGNORE INTO stamp_jobs(location_id,kind,state,updated_at)
			SELECT id,'ots',CASE WHEN ots_status IN ('submitted','confirmed') THEN 'done' WHEN ots_status='fail' THEN 'failed' ELSE 'queued' END,strftime('%s','now')
			FROM locations;
	`)
}

func migrateV4(tx *sql.Tx) error {
	return execMigration(tx, 4, `
		ALTER TABLE stamp_jobs ADD COLUMN lease_owner TEXT NOT NULL DEFAULT '';
		ALTER TABLE stamp_jobs ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0;
		CREATE INDEX idx_stamp_jobs_lease ON stamp_jobs(state,lease_until,id);
		CREATE TABLE proof_gc (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX idx_proof_gc_ready ON proof_gc(next_attempt_at,id);
	`)
}

func migrateV5(tx *sql.Tx) error {
	return execMigration(tx, 5, `
		DROP TABLE share_links;
		CREATE TABLE share_links (
			id INTEGER PRIMARY KEY,
			token_hash BLOB NOT NULL UNIQUE,
			label TEXT NOT NULL DEFAULT '',
			user TEXT NOT NULL DEFAULT '',
			device TEXT NOT NULL DEFAULT '',
			hours INTEGER NOT NULL DEFAULT 24,
			expires_at INTEGER,
			created_at INTEGER NOT NULL,
			last_access_at INTEGER,
			delay_minutes INTEGER NOT NULL DEFAULT 15,
			precision_m INTEGER NOT NULL DEFAULT 100
		);
		CREATE INDEX idx_share_token_hash ON share_links(token_hash);
		CREATE TABLE admin_sessions (
			id INTEGER PRIMARY KEY,
			token_hash BLOB NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX idx_admin_sessions_expiry ON admin_sessions(expires_at,last_seen_at);
	`)
}

func migrateV6(tx *sql.Tx) error {
	return execMigration(tx, 6, `
		CREATE TABLE proof_verification_runs (
			id INTEGER PRIMARY KEY,
			location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('tsa','ots')),
			trigger TEXT NOT NULL CHECK(trigger IN ('stamp','upgrade','scheduled','manual','import')),
			result TEXT NOT NULL CHECK(result IN ('verified','degraded','pending','inconclusive','invalid','error','missing')),
			proof_sha256 BLOB NOT NULL DEFAULT X'',
			checked_at INTEGER NOT NULL,
			tsa_gen_time INTEGER,
			tsa_policy_oid TEXT NOT NULL DEFAULT '',
			tsa_serial TEXT NOT NULL DEFAULT '',
			tsa_signer_subject TEXT NOT NULL DEFAULT '',
			tsa_signer_fingerprint TEXT NOT NULL DEFAULT '',
			bitcoin_height INTEGER,
			bitcoin_block_hash TEXT NOT NULL DEFAULT '',
			agreeing_sources INTEGER NOT NULL DEFAULT 0,
			source_count INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_proof_verification_latest
			ON proof_verification_runs(location_id,kind,id DESC);
		CREATE INDEX idx_proof_verification_checked
			ON proof_verification_runs(checked_at,id);
		CREATE TABLE proof_verification_attempts (
			id INTEGER PRIMARY KEY,
			run_id INTEGER NOT NULL REFERENCES proof_verification_runs(id) ON DELETE CASCADE,
			provider_id TEXT NOT NULL,
			result TEXT NOT NULL CHECK(result IN ('verified','pending','invalid','error')),
			block_height INTEGER,
			block_hash TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error_code TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_proof_verification_attempts_run
			ON proof_verification_attempts(run_id,id);
	`)
}

func migrateV7(tx *sql.Tx) error {
	return execMigration(tx, 7, `
		ALTER TABLE locations ADD COLUMN tsa_nonce BLOB NOT NULL DEFAULT X'';
		ALTER TABLE proof_verification_runs ADD COLUMN engine_version INTEGER NOT NULL DEFAULT 1;
		ALTER TABLE proof_verification_runs ADD COLUMN policy_sha256 BLOB NOT NULL DEFAULT X'';
		ALTER TABLE proof_verification_runs ADD COLUMN tsa_nonce_status TEXT NOT NULL DEFAULT '';
		ALTER TABLE proof_verification_runs ADD COLUMN tsa_policy_status TEXT NOT NULL DEFAULT '';
		ALTER TABLE proof_verification_runs ADD COLUMN tsa_trust_mode TEXT NOT NULL DEFAULT '';
		ALTER TABLE proof_verification_runs ADD COLUMN tsa_accuracy_ms INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_runs ADD COLUMN tsa_trust_anchor_fingerprint TEXT NOT NULL DEFAULT '';
		ALTER TABLE proof_verification_runs ADD COLUMN bitcoin_pow_valid INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_runs ADD COLUMN bitcoin_confirmations INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_attempts ADD COLUMN tip_height INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_attempts ADD COLUMN confirmations INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_attempts ADD COLUMN header_valid INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_attempts ADD COLUMN pow_valid INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE proof_verification_attempts ADD COLUMN merkle_valid INTEGER NOT NULL DEFAULT 0;
	`)
}

func migrateV8(tx *sql.Tx) error {
	return execMigration(tx, 8, `
		CREATE TABLE proof_verification_jobs (
			id INTEGER PRIMARY KEY,
			location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('tsa','ots')),
			trigger TEXT NOT NULL CHECK(trigger IN ('stamp','upgrade','scheduled','manual','import')),
			state TEXT NOT NULL CHECK(state IN ('queued','running','retry','done','failed')),
			generation INTEGER NOT NULL DEFAULT 1,
			claimed_generation INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			next_attempt_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(location_id,kind)
		);
		CREATE INDEX idx_proof_verification_jobs_ready
			ON proof_verification_jobs(state,next_attempt_at,lease_until,id);
		CREATE INDEX idx_proof_verification_jobs_updated
			ON proof_verification_jobs(state,updated_at,id);
	`)
}

func migrateV9(tx *sql.Tx) error {
	return execMigration(tx, 9, `
		CREATE TABLE maintenance_jobs (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL UNIQUE CHECK(kind IN ('backup','retention','ots_upgrade','proof_gc','session_cleanup')),
			trigger TEXT NOT NULL CHECK(trigger IN ('scheduled','manual')),
			state TEXT NOT NULL CHECK(state IN ('scheduled','queued','running','retry','failed','disabled')),
			generation INTEGER NOT NULL DEFAULT 0,
			claimed_generation INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			next_run_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			last_started_at INTEGER NOT NULL DEFAULT 0,
			last_finished_at INTEGER NOT NULL DEFAULT 0,
			last_success_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX idx_maintenance_jobs_ready
			ON maintenance_jobs(state,next_run_at,lease_until,id);
		CREATE TABLE maintenance_runs (
			id INTEGER PRIMARY KEY,
			job_id INTEGER NOT NULL REFERENCES maintenance_jobs(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('backup','retention','ots_upgrade','proof_gc','session_cleanup')),
			trigger TEXT NOT NULL CHECK(trigger IN ('scheduled','manual')),
			result TEXT NOT NULL CHECK(result IN ('success','error','canceled')),
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_maintenance_runs_finished
			ON maintenance_runs(finished_at,id);
	`)
}
