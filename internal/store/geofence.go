package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type GeofenceInputError string

func (e GeofenceInputError) Error() string { return string(e) }

type Geofence struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	RadiusM   float64 `json:"radius_m"`
	User      string  `json:"user"`
	Device    string  `json:"device"`
	Enabled   bool    `json:"enabled"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}
type GeofenceEvent struct {
	ID         int64  `json:"id"`
	GeofenceID int64  `json:"geofence_id"`
	Name       string `json:"name"`
	LocationID int64  `json:"location_id"`
	User       string `json:"user"`
	Device     string `json:"device"`
	Event      string `json:"event"`
	Tst        int64  `json:"tst"`
	CreatedAt  int64  `json:"created_at"`
}
type GeofenceDelivery struct {
	ID, EventID int64
	Attempts    int
	LeaseOwner  string
	Event       GeofenceEvent
}

func validateGeofence(v *Geofence) error {
	v.Name = strings.TrimSpace(v.Name)
	v.User = strings.TrimSpace(v.User)
	v.Device = strings.TrimSpace(v.Device)
	if !validText(v.Name, 80, false) {
		return GeofenceInputError("name must be 1 to 80 bytes")
	}
	if !validText(v.User, 128, true) || !validText(v.Device, 128, true) {
		return GeofenceInputError("user and device must be at most 128 bytes without control characters")
	}
	if v.Lat < -90 || v.Lat > 90 || v.Lon < -180 || v.Lon > 180 {
		return GeofenceInputError("invalid coordinates")
	}
	if v.RadiusM < 25 || v.RadiusM > 100000 {
		return GeofenceInputError("radius_m must be between 25 and 100000")
	}
	return nil
}

func validText(value string, maximum int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Store) ListGeofences() ([]Geofence, error) {
	rows, err := s.db.Query(`SELECT id,name,lat,lon,radius_m,user,device,enabled,created_at,updated_at FROM geofences ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]Geofence, 0)
	for rows.Next() {
		var v Geofence
		if err := rows.Scan(&v.ID, &v.Name, &v.Lat, &v.Lon, &v.RadiusM, &v.User, &v.Device, &v.Enabled, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CreateGeofence(v *Geofence) (*Geofence, error) {
	if err := validateGeofence(v); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE geofences SET id=id WHERE 0`); err != nil {
		return nil, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM geofences`).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 100 {
		return nil, GeofenceInputError("geofence limit reached")
	}
	now := time.Now().Unix()
	r, err := tx.Exec(`INSERT INTO geofences(name,lat,lon,radius_m,user,device,enabled,created_at,updated_at)VALUES(?,?,?,?,?,?,?,?,?)`, v.Name, v.Lat, v.Lon, v.RadiusM, v.User, v.Device, v.Enabled, now, now)
	if err != nil {
		return nil, err
	}
	v.ID, err = r.LastInsertId()
	if err != nil {
		return nil, err
	}
	v.CreatedAt = now
	v.UpdatedAt = now
	if err = rebuildGeofenceStateTx(tx, v.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.publish(Change{Topic: ChangeGeofences})
	return v, nil
}

func (s *Store) UpdateGeofence(id int64, v *Geofence) (*Geofence, error) {
	if err := validateGeofence(v); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`UPDATE geofences SET name=?,lat=?,lon=?,radius_m=?,user=?,device=?,enabled=?,updated_at=? WHERE id=?`, v.Name, v.Lat, v.Lon, v.RadiusM, v.User, v.Device, v.Enabled, now, id)
	if err != nil {
		return nil, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, sql.ErrNoRows
	}
	if err = rebuildGeofenceStateTx(tx, id); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(`SELECT id,name,lat,lon,radius_m,user,device,enabled,created_at,updated_at FROM geofences WHERE id=?`, id).Scan(
		&v.ID, &v.Name, &v.Lat, &v.Lon, &v.RadiusM, &v.User, &v.Device, &v.Enabled, &v.CreatedAt, &v.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.publish(Change{Topic: ChangeGeofences})
	return v, nil
}

func (s *Store) UpdateGeofenceEnabled(id int64, enabled bool) (*Geofence, error) {
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`UPDATE geofences SET enabled=?,updated_at=? WHERE id=?`, enabled, now, id)
	if err != nil {
		return nil, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, sql.ErrNoRows
	}
	if err = rebuildGeofenceStateTx(tx, id); err != nil {
		return nil, err
	}
	v := &Geofence{}
	err = tx.QueryRow(`SELECT id,name,lat,lon,radius_m,user,device,enabled,created_at,updated_at FROM geofences WHERE id=?`, id).Scan(
		&v.ID, &v.Name, &v.Lat, &v.Lon, &v.RadiusM, &v.User, &v.Device, &v.Enabled, &v.CreatedAt, &v.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.publish(Change{Topic: ChangeGeofences})
	return v, nil
}

func (s *Store) DeleteGeofence(id int64) error {
	r, err := s.db.Exec(`DELETE FROM geofences WHERE id=?`, id)
	if err == nil {
		if n, _ := r.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		s.publish(Change{Topic: ChangeGeofences})
	}
	return err
}

func processGeofencesTx(tx *sql.Tx, loc *Location, id int64, suppress bool) (bool, error) {
	rows, err := tx.Query(`SELECT id,lat,lon,radius_m FROM geofences WHERE enabled=1 AND (user='' OR user=?) AND (device='' OR device=?)`, loc.User, loc.Device)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	changed := false
	now := time.Now().Unix()
	for rows.Next() {
		var gid int64
		var lat, lon, radius float64
		if err := rows.Scan(&gid, &lat, &lon, &radius); err != nil {
			return false, err
		}
		inside := geoDistance(lat, lon, loc.Lat, loc.Lon) <= radius
		var previous bool
		var last int64
		err := tx.QueryRow(`SELECT inside,last_tst FROM geofence_states WHERE geofence_id=? AND user=? AND device=?`, gid, loc.User, loc.Device).Scan(&previous, &last)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.Exec(`INSERT INTO geofence_states(geofence_id,user,device,inside,last_tst,last_location_id)VALUES(?,?,?,?,?,?)`, gid, loc.User, loc.Device, inside, loc.Tst, id)
			if err != nil {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		if loc.Tst <= last {
			continue
		}
		_, err = tx.Exec(`UPDATE geofence_states SET inside=?,last_tst=?,last_location_id=? WHERE geofence_id=? AND user=? AND device=?`, inside, loc.Tst, id, gid, loc.User, loc.Device)
		if err != nil {
			return false, err
		}
		if suppress || inside == previous {
			continue
		}
		event := "exit"
		if inside {
			event = "enter"
		}
		r, err := tx.Exec(`INSERT INTO geofence_events(geofence_id,location_id,user,device,event,tst,created_at)VALUES(?,?,?,?,?,?,?)`, gid, id, loc.User, loc.Device, event, loc.Tst, now)
		if err != nil {
			return false, err
		}
		eid, _ := r.LastInsertId()
		if _, err = tx.Exec(`INSERT INTO geofence_deliveries(event_id,state,created_at,updated_at)VALUES(?,'queued',?,?)`, eid, now, now); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, rows.Err()
}

func (s *Store) RebuildGeofenceState(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = rebuildGeofenceStateTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func rebuildGeofenceStateTx(tx *sql.Tx, id int64) error {
	const batchSize = 128

	if _, err := tx.Exec(`DELETE FROM geofence_states WHERE geofence_id=?`, id); err != nil {
		return err
	}
	var fence Geofence
	if err := tx.QueryRow(`SELECT id,lat,lon,radius_m,user,device,enabled FROM geofences WHERE id=?`, id).Scan(
		&fence.ID, &fence.Lat, &fence.Lon, &fence.RadiusM, &fence.User, &fence.Device, &fence.Enabled,
	); err != nil {
		return err
	}
	if !fence.Enabled {
		return nil
	}
	var afterUser, afterDevice string
	hasCursor := false
	for {
		query := `SELECT l.id,l.user,l.device,l.tst,l.lat,l.lon FROM locations l
			WHERE (?='' OR l.user=?) AND (?='' OR l.device=?)
			AND l.id=(SELECT id FROM locations x WHERE x.user=l.user AND x.device=l.device ORDER BY x.tst DESC,x.id DESC LIMIT 1)`
		args := []any{fence.User, fence.User, fence.Device, fence.Device}
		if hasCursor {
			query += ` AND (l.user>? OR (l.user=? AND l.device>?))`
			args = append(args, afterUser, afterUser, afterDevice)
		}
		query += ` ORDER BY l.user,l.device LIMIT ?`
		args = append(args, batchSize)

		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		locs := make([]Location, 0, batchSize)
		for rows.Next() {
			var v Location
			if err := rows.Scan(&v.ID, &v.User, &v.Device, &v.Tst, &v.Lat, &v.Lon); err != nil {
				_ = rows.Close()
				return err
			}
			locs = append(locs, v)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(locs) == 0 {
			return nil
		}

		for _, loc := range locs {
			inside := geoDistance(fence.Lat, fence.Lon, loc.Lat, loc.Lon) <= fence.RadiusM
			if _, err = tx.Exec(`INSERT INTO geofence_states(geofence_id,user,device,inside,last_tst,last_location_id) VALUES(?,?,?,?,?,?)`,
				fence.ID, loc.User, loc.Device, inside, loc.Tst, loc.ID); err != nil {
				return err
			}
		}
		last := locs[len(locs)-1]
		afterUser, afterDevice = last.User, last.Device
		hasCursor = true
	}
}

type geofenceStateKey struct {
	GeofenceID   int64
	User, Device string
}

const geofenceGCKeysTable = "mytracker_geofence_gc_keys"

func prepareGeofenceStateKeysTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS ` + geofenceGCKeysTable + `(
		geofence_id INTEGER NOT NULL,
		user TEXT NOT NULL,
		device TEXT NOT NULL,
		PRIMARY KEY(geofence_id,user,device)
	)`); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM ` + geofenceGCKeysTable)
	return err
}

func stageGeofenceStateKeysWhereTx(tx *sql.Tx, where string, args ...any) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO `+geofenceGCKeysTable+`(geofence_id,user,device)
		SELECT s.geofence_id,s.user,s.device
		FROM geofence_states s JOIN locations l ON l.id=s.last_location_id WHERE `+where, args...)
	return err
}

func rebuildGeofenceStateKeysTx(tx *sql.Tx) error {
	const batchSize = 128
	var last geofenceStateKey
	started := false
	for {
		query := `SELECT geofence_id,user,device FROM ` + geofenceGCKeysTable
		args := []any{}
		if started {
			query += ` WHERE geofence_id>? OR (geofence_id=? AND user>?) OR (geofence_id=? AND user=? AND device>?)`
			args = append(args, last.GeofenceID, last.GeofenceID, last.User, last.GeofenceID, last.User, last.Device)
		}
		query += ` ORDER BY geofence_id,user,device LIMIT ?`
		args = append(args, batchSize)
		rows, err := tx.Query(query, args...)
		if err != nil {
			return err
		}
		keys := make([]geofenceStateKey, 0, batchSize)
		for rows.Next() {
			var k geofenceStateKey
			if err := rows.Scan(&k.GeofenceID, &k.User, &k.Device); err != nil {
				_ = rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		for _, k := range keys {
			if _, err := tx.Exec(`DELETE FROM geofence_states WHERE geofence_id=? AND user=? AND device=?`, k.GeofenceID, k.User, k.Device); err != nil {
				return err
			}
			var locationID, tst int64
			var lat, lon, fenceLat, fenceLon, radius float64
			err := tx.QueryRow(`SELECT l.id,l.tst,l.lat,l.lon,g.lat,g.lon,g.radius_m FROM geofences g JOIN locations l ON l.user=? AND l.device=? WHERE g.id=? AND g.enabled=1 AND (g.user='' OR g.user=l.user) AND (g.device='' OR g.device=l.device) ORDER BY l.tst DESC,l.id DESC LIMIT 1`, k.User, k.Device, k.GeofenceID).Scan(&locationID, &tst, &lat, &lon, &fenceLat, &fenceLon, &radius)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			inside := geoDistance(fenceLat, fenceLon, lat, lon) <= radius
			if _, err = tx.Exec(`INSERT INTO geofence_states(geofence_id,user,device,inside,last_tst,last_location_id) VALUES(?,?,?,?,?,?)`, k.GeofenceID, k.User, k.Device, inside, tst, locationID); err != nil {
				return err
			}
		}
		last = keys[len(keys)-1]
		started = true
	}
}

func (s *Store) ListGeofenceEvents(limit int) ([]GeofenceEvent, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT e.id,e.geofence_id,g.name,e.location_id,e.user,e.device,e.event,e.tst,e.created_at FROM geofence_events e JOIN geofences g ON g.id=e.geofence_id ORDER BY e.tst DESC,e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]GeofenceEvent, 0)
	for rows.Next() {
		var v GeofenceEvent
		if err := rows.Scan(&v.ID, &v.GeofenceID, &v.Name, &v.LocationID, &v.User, &v.Device, &v.Event, &v.Tst, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) RecoverGeofenceDeliveries(now int64) error {
	_, err := s.db.Exec(`
		DELETE FROM geofence_deliveries
		WHERE state='done' OR (state='failed' AND updated_at<?);
		UPDATE geofence_deliveries
		SET state=CASE WHEN attempts>=5 THEN 'failed' ELSE 'queued' END,
			lease_owner='',lease_until=0,next_attempt_at=0,
			last_error=CASE WHEN attempts>=5 THEN 'retry limit exceeded' ELSE last_error END,
			updated_at=?
		WHERE state='running';
		UPDATE geofence_deliveries
		SET state='failed',next_attempt_at=0,updated_at=?,last_error='retry limit exceeded'
		WHERE state='retry' AND attempts>=5`, now-int64((30*24*time.Hour)/time.Second), now, now)
	return err
}

func (s *Store) CleanupGeofenceDeliveries(before int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM geofence_deliveries WHERE state='done' OR (state='failed' AND updated_at<?)`, before)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
func (s *Store) ClaimGeofenceDelivery(owner string, now, until int64) (*GeofenceDelivery, error) {
	var d GeofenceDelivery
	if _, err := s.db.Exec(`UPDATE geofence_deliveries SET state='failed',lease_owner='',lease_until=0,next_attempt_at=0,updated_at=?,last_error='retry limit exceeded'
		WHERE attempts>=5 AND (state IN ('queued','retry') OR (state='running' AND lease_until<=?))`, now, now); err != nil {
		return nil, err
	}
	err := s.db.QueryRow(`UPDATE geofence_deliveries SET state='running',attempts=attempts+1,lease_owner=?,lease_until=?,updated_at=? WHERE id=(SELECT id FROM geofence_deliveries WHERE attempts<5 AND ((state IN ('queued','retry') AND next_attempt_at<=?) OR (state='running' AND lease_until<=?)) ORDER BY id LIMIT 1) RETURNING id,event_id,attempts,lease_owner`, owner, until, now, now, now).Scan(&d.ID, &d.EventID, &d.Attempts, &d.LeaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = s.db.QueryRow(`SELECT e.id,e.geofence_id,g.name,e.location_id,e.user,e.device,e.event,e.tst,e.created_at FROM geofence_events e JOIN geofences g ON g.id=e.geofence_id WHERE e.id=?`, d.EventID).Scan(&d.Event.ID, &d.Event.GeofenceID, &d.Event.Name, &d.Event.LocationID, &d.Event.User, &d.Event.Device, &d.Event.Event, &d.Event.Tst, &d.Event.CreatedAt)
	return &d, err
}
func (s *Store) RenewGeofenceDelivery(id int64, owner string, until int64) (bool, error) {
	return s.RenewGeofenceDeliveryContext(context.Background(), id, owner, until)
}

func (s *Store) RenewGeofenceDeliveryContext(ctx context.Context, id int64, owner string, until int64) (bool, error) {
	now := time.Now().Unix()
	r, err := s.db.ExecContext(ctx, `UPDATE geofence_deliveries SET lease_until=?,updated_at=? WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, until, now, id, owner, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) FinishGeofenceDelivery(d *GeofenceDelivery, cause error) error {
	now := time.Now()
	if cause == nil {
		res, err := s.db.Exec(`DELETE FROM geofence_deliveries WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, d.ID, d.LeaseOwner, now.Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrLeaseLost
		}
		s.publish(Change{Topic: ChangeGeofences, LocationID: d.Event.LocationID})
		return nil
	}
	state := "retry"
	next := int64(0)
	msg := ""
	next = now.Add(time.Duration(1<<min(d.Attempts, 8)) * time.Second).Unix()
	msg = SafeJobError(cause)
	if d.Attempts >= 5 {
		state = "failed"
	}
	r, err := s.db.Exec(`UPDATE geofence_deliveries SET state=?,next_attempt_at=?,last_error=?,lease_owner='',lease_until=0,updated_at=? WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, state, next, msg, now.Unix(), d.ID, d.LeaseOwner, now.Unix())
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrLeaseLost
	}
	s.publish(Change{Topic: ChangeGeofences, LocationID: d.Event.LocationID})
	return nil
}

type DurableJobDiagnostics struct {
	Queued        int `json:"queued"`
	Running       int `json:"running"`
	Retry         int `json:"retry"`
	Failed        int `json:"failed"`
	ExpiredLeases int `json:"expired_leases"`
}

func (s *Store) GeofenceDeliveryDiagnostics(now int64) (DurableJobDiagnostics, error) {
	var out DurableJobDiagnostics
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(state='queued'),0),COALESCE(SUM(state='running'),0),
		COALESCE(SUM(state='retry'),0),COALESCE(SUM(state='failed'),0),
		COALESCE(SUM(state='running' AND lease_until<=?),0)
		FROM geofence_deliveries`, now).Scan(&out.Queued, &out.Running, &out.Retry, &out.Failed, &out.ExpiredLeases)
	return out, err
}
func (s *Store) GeofenceDeliveryFailures() (int, error) {
	return s.GeofenceDeliveryFailuresContext(context.Background())
}

func (s *Store) GeofenceDeliveryFailuresContext(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM geofence_deliveries WHERE state='failed'`).Scan(&n)
	return n, err
}

func geoDistance(a, b, c, d float64) float64 {
	const r = 6371008.8
	p1 := a * math.Pi / 180
	p2 := c * math.Pi / 180
	dp := (c - a) * math.Pi / 180
	dl := (d - b) * math.Pi / 180
	x := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	x = math.Max(0, math.Min(1, x))
	return 2 * r * math.Atan2(math.Sqrt(x), math.Sqrt(1-x))
}
