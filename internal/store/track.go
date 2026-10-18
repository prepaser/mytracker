package store

import (
	"context"
	"database/sql"
	"strings"
)

type TrackFilter struct {
	User   string
	Device string
	From   int64
	To     int64
	Limit  int
}

type TrackSnapshot struct {
	tx     *sql.Tx
	filter TrackFilter
	ctx    context.Context
}

type TrackPoint struct {
	ID           int64
	Tst          int64
	Lat          float64
	Lon          float64
	User, Device string
}

func normalizeTrackLimit(limit int) int {
	if limit < 1 {
		return 5000
	}
	if limit == 1 {
		return 2
	}
	if limit > 20000 {
		return 20000
	}
	return limit
}

func trackFilter(f TrackFilter) (string, []any) {
	return buildLocationFilter(locationQueryFilter{
		user: f.User, device: f.Device, from: f.From, to: f.To,
	})
}

func (s *Store) BeginTrackSnapshot(f TrackFilter) (*TrackSnapshot, error) {
	return s.BeginTrackSnapshotContext(context.Background(), f)
}

func (s *Store) BeginTrackSnapshotContext(ctx context.Context, f TrackFilter) (*TrackSnapshot, error) {
	f.Limit = normalizeTrackLimit(f.Limit)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	snapshot := &TrackSnapshot{tx: tx, filter: f, ctx: ctx}
	if err := pinSnapshot(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return snapshot, nil
}

func (s *TrackSnapshot) Close() error {
	return s.tx.Rollback()
}

func (s *TrackSnapshot) Limit() int {
	return s.filter.Limit
}

func (s *TrackSnapshot) Count() (int, error) {
	where, args := trackFilter(s.filter)
	var n int
	err := s.tx.QueryRowContext(s.ctx, `SELECT COUNT(*) FROM locations WHERE `+where, args...).Scan(&n)
	return n, err
}

func (s *TrackSnapshot) Iterate(batch int, fn func([]Location) error) error {
	where, args := trackFilter(s.filter)
	return iterateLocationKeyset(s.ctx, s.tx, `SELECT `+locationColumns+` FROM locations WHERE `+where, args, batch, fn)
}

func (s *TrackSnapshot) IteratePoints(batch int, fn func([]TrackPoint) error) error {
	where, args := trackFilter(s.filter)
	return iterateTrackPointKeyset(s.ctx, s.tx, `SELECT id,tst,lat,lon,user,device FROM locations WHERE `+where, args, batch, scanTrackPoint, fn)
}

func (s *TrackSnapshot) LoadLocations(ids []int64) ([]Location, error) {
	byID := make(map[int64]Location, len(ids))
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		locs, err := queryLocationsFromContext(s.ctx, s.tx,
			`SELECT `+locationColumns+` FROM locations WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for _, loc := range locs {
			byID[loc.ID] = loc
		}
	}
	out := make([]Location, 0, len(ids))
	for _, id := range ids {
		loc, ok := byID[id]
		if !ok {
			return nil, sql.ErrNoRows
		}
		out = append(out, loc)
	}
	return out, nil
}

func (s *Store) ListTrack(f TrackFilter) ([]Location, error) {
	f.Limit = normalizeTrackLimit(f.Limit)
	where, args := trackFilter(f)
	args = append(args, f.Limit)
	return s.queryLocations(`SELECT `+locationColumns+` FROM locations WHERE `+where+
		` ORDER BY tst ASC,id ASC LIMIT ?`, args...)
}

func (s *Store) ListIdentities() ([]struct{ User, Device string }, error) {
	rows, err := s.db.Query(`SELECT DISTINCT user, device FROM locations ORDER BY user, device`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []struct{ User, Device string }
	for rows.Next() {
		var id struct{ User, Device string }
		if err := rows.Scan(&id.User, &id.Device); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
