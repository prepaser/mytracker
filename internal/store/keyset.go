package store

import (
	"context"
	"database/sql"
)

type trackPointScan func(*sql.Rows, *TrackPoint) error

func iterateLocationKeyset(ctx context.Context, q contextQueryer, base string, baseArgs []any, batch int, fn func([]Location) error) error {
	if batch < 1 {
		batch = 500
	}
	var lastTst, lastID int64
	started := false
	for {
		query := base
		args := append([]any(nil), baseArgs...)
		if started {
			query += ` AND (tst,id)>(?,?)`
			args = append(args, lastTst, lastID)
		}
		query += ` ORDER BY tst,id LIMIT ?`
		args = append(args, batch)
		locs, err := queryLocationsFromContext(ctx, q, query, args...)
		if err != nil {
			return err
		}
		if len(locs) == 0 {
			return nil
		}
		if err := fn(locs); err != nil {
			return err
		}
		if len(locs) < batch {
			return nil
		}
		last := locs[len(locs)-1]
		lastTst, lastID, started = last.Tst, last.ID, true
	}
}

func iterateTrackPointKeyset(ctx context.Context, q contextQueryer, base string, baseArgs []any, batch int, scan trackPointScan, fn func([]TrackPoint) error) error {
	if batch < 1 {
		batch = 1000
	}
	var lastTst, lastID int64
	started := false
	for {
		query := base
		args := append([]any(nil), baseArgs...)
		if started {
			query += ` AND (tst,id)>(?,?)`
			args = append(args, lastTst, lastID)
		}
		query += ` ORDER BY tst,id LIMIT ?`
		args = append(args, batch)
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		points := make([]TrackPoint, 0, batch)
		var p TrackPoint
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			p = TrackPoint{}
			if err := scan(rows, &p); err != nil {
				_ = rows.Close()
				return err
			}
			points = append(points, p)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(points) == 0 {
			return nil
		}
		if err := fn(points); err != nil {
			return err
		}
		if len(points) < batch {
			return nil
		}
		last := points[len(points)-1]
		lastTst, lastID, started = last.Tst, last.ID, true
	}
}

func scanTrackPoint(rows *sql.Rows, p *TrackPoint) error {
	return rows.Scan(&p.ID, &p.Tst, &p.Lat, &p.Lon, &p.User, &p.Device)
}

func scanTimelinePoint(rows *sql.Rows, p *TrackPoint) error {
	return rows.Scan(&p.ID, &p.Tst, &p.Lat, &p.Lon)
}
