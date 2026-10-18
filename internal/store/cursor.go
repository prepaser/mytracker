package store

import "database/sql"

type LocationSummary struct {
	ID        int64
	User      string
	Device    string
	Tst       int64
	Lat       float64
	Lon       float64
	TsaTime   sql.NullInt64
	TsaStatus string
	OtsStatus string
	CreatedAt int64
}

func (s *Store) ListCursor(before int64, limit int) ([]Location, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 201 {
		limit = 201
	}
	if before > 0 {
		return s.queryLocations(`SELECT `+locationColumns+` FROM locations WHERE id<? ORDER BY id DESC LIMIT ?`, before, limit)
	}
	return s.queryLocations(`SELECT `+locationColumns+` FROM locations ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) ListCursorSummary(before int64, limit int) ([]LocationSummary, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 201 {
		limit = 201
	}
	query := `SELECT id,user,device,tst,lat,lon,tsa_time,tsa_status,ots_status,created_at FROM locations`
	args := []any{}
	if before > 0 {
		query += ` WHERE id<?`
		args = append(args, before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]LocationSummary, 0, limit)
	for rows.Next() {
		var loc LocationSummary
		if err := rows.Scan(&loc.ID, &loc.User, &loc.Device, &loc.Tst, &loc.Lat, &loc.Lon,
			&loc.TsaTime, &loc.TsaStatus, &loc.OtsStatus, &loc.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, loc)
	}
	return out, rows.Err()
}
