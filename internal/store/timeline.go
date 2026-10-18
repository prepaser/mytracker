package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"time"
)

type NullableFloat64 struct {
	Float64 float64
	Valid   bool
}

func (n *NullableFloat64) Scan(value any) error {
	var v sql.NullFloat64
	if err := v.Scan(value); err != nil {
		return err
	}
	n.Float64, n.Valid = v.Float64, v.Valid
	return nil
}
func (n NullableFloat64) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.Float64, nil
}
func (n NullableFloat64) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, n.Float64, 'f', -1, 64), nil
}

type TimelineSegment struct {
	ID              int64           `json:"id"`
	StartLocationID int64           `json:"start_location_id"`
	EndLocationID   int64           `json:"end_location_id"`
	User            string          `json:"user"`
	Device          string          `json:"device"`
	Kind            string          `json:"kind"`
	StartTst        int64           `json:"start_tst"`
	EndTst          int64           `json:"end_tst"`
	CenterLat       NullableFloat64 `json:"center_lat"`
	CenterLon       NullableFloat64 `json:"center_lon"`
	DistanceM       float64         `json:"distance_m"`
	PointCount      int             `json:"point_count"`
	GapBefore       bool            `json:"gap_before"`
}

type TimelineJob struct {
	ID, DirtyFrom, Generation, ClaimedGeneration int64
	User, Device, State, LeaseOwner              string
	Attempts                                     int
}

type TimelineFilter struct {
	User, Device        string
	From, To            int64
	BeforeTst, BeforeID int64
	Limit               int
}

type TimelineSummary struct {
	TotalSegments int     `json:"total_segments"`
	Stays         int     `json:"stays"`
	Trips         int     `json:"trips"`
	DistanceM     float64 `json:"distance_m"`
	StaySeconds   int64   `json:"stay_seconds"`
}

const timelineStageInsert = `INSERT INTO timeline_rebuild_segments(
	user,device,kind,start_location_id,end_location_id,start_tst,end_tst,
	center_lat,center_lon,distance_m,point_count,gap_before
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`

type TimelineRebuild struct {
	store *Store
	conn  *sql.Conn
}

func (s *Store) BeginTimelineRebuild(ctx context.Context) (*TimelineRebuild, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `DROP TABLE IF EXISTS timeline_rebuild_segments;
	CREATE TEMP TABLE timeline_rebuild_segments (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		user TEXT NOT NULL,
		device TEXT NOT NULL,
		kind TEXT NOT NULL,
		start_location_id INTEGER NOT NULL,
		end_location_id INTEGER NOT NULL,
		start_tst INTEGER NOT NULL,
		end_tst INTEGER NOT NULL,
		center_lat REAL,
		center_lon REAL,
		distance_m REAL NOT NULL,
		point_count INTEGER NOT NULL,
		gap_before INTEGER NOT NULL
	)`)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &TimelineRebuild{store: s, conn: conn}, nil
}

func (r *TimelineRebuild) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	conn := r.conn
	r.conn = nil
	_, dropErr := conn.ExecContext(context.Background(), `DROP TABLE IF EXISTS timeline_rebuild_segments`)
	closeErr := conn.Close()
	if dropErr != nil {
		return dropErr
	}
	return closeErr
}

func (r *TimelineRebuild) Append(ctx context.Context, segments []TimelineSegment) error {
	if len(segments) == 0 {
		return nil
	}
	tx, err := r.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, timelineStageInsert)
	if err != nil {
		return err
	}
	for _, v := range segments {
		if _, err = stmt.ExecContext(ctx, v.User, v.Device, v.Kind, v.StartLocationID, v.EndLocationID,
			v.StartTst, v.EndTst, v.CenterLat, v.CenterLon, v.DistanceM, v.PointCount, v.GapBefore); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	if err = stmt.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *TimelineRebuild) IteratePoints(ctx context.Context, user, device string, from int64, batch int, fn func([]TrackPoint) error) error {
	return iterateTimelinePoints(ctx, r.conn, user, device, from, batch, fn)
}

func (r *TimelineRebuild) RebuildFrom(ctx context.Context, user, device string, dirty int64) (int64, error) {
	from, _, err := r.RebuildBoundary(ctx, user, device, dirty)
	return from, err
}

func (r *TimelineRebuild) RebuildBoundary(ctx context.Context, user, device string, dirty int64) (int64, bool, error) {
	return timelineRebuildBoundary(ctx, r.conn, user, device, dirty)
}

func iterateTimelinePoints(ctx context.Context, q contextQueryer, user, device string, from int64, batch int, fn func([]TrackPoint) error) error {
	return iterateTrackPointKeyset(ctx, q, `SELECT id,tst,lat,lon FROM locations WHERE user=? AND device=? AND tst>=?`, []any{user, device, from}, batch, scanTimelinePoint, fn)
}

func enqueueTimelineTx(tx *sql.Tx, user, device string, dirtyFrom, now int64) error {
	_, err := tx.Exec(`
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
		VALUES (?,?,?,'queued',?,?)
		ON CONFLICT(user,device) DO UPDATE SET
			dirty_from=MIN(timeline_rebuild_jobs.dirty_from,excluded.dirty_from),
			generation=timeline_rebuild_jobs.generation+1,
			state=CASE WHEN timeline_rebuild_jobs.state='running' THEN 'running' ELSE 'queued' END,
			attempts=CASE WHEN timeline_rebuild_jobs.state='running' THEN attempts ELSE 0 END,
			next_attempt_at=CASE WHEN timeline_rebuild_jobs.state='running' THEN next_attempt_at ELSE 0 END,
			last_error=CASE WHEN timeline_rebuild_jobs.state='running' THEN last_error ELSE '' END,
			updated_at=excluded.updated_at`, user, device, dirtyFrom, now, now)
	return err
}

func enqueueTimelineWhereTx(tx *sql.Tx, where string, args ...any) error {
	now := time.Now().Unix()
	values := append([]any{now, now}, args...)
	_, err := tx.Exec(`INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
		SELECT user,device,MIN(tst),'queued',?,? FROM locations WHERE `+where+` GROUP BY user,device
		ON CONFLICT(user,device) DO UPDATE SET dirty_from=MIN(timeline_rebuild_jobs.dirty_from,excluded.dirty_from),
		generation=timeline_rebuild_jobs.generation+1,state=CASE WHEN timeline_rebuild_jobs.state='running' THEN 'running' ELSE 'queued' END,
		attempts=CASE WHEN timeline_rebuild_jobs.state='running' THEN timeline_rebuild_jobs.attempts ELSE 0 END,
		next_attempt_at=CASE WHEN timeline_rebuild_jobs.state='running' THEN timeline_rebuild_jobs.next_attempt_at ELSE 0 END,
		last_error=CASE WHEN timeline_rebuild_jobs.state='running' THEN timeline_rebuild_jobs.last_error ELSE '' END,updated_at=excluded.updated_at`, values...)
	return err
}

func invalidateTimelineWhereTx(tx *sql.Tx, where string, args ...any) error {
	if err := enqueueTimelineWhereTx(tx, where, args...); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE timeline_rebuild_jobs SET state='queued',attempts=0,
		lease_owner='',lease_until=0,next_attempt_at=0,last_error=''
		WHERE (user,device) IN (SELECT user,device FROM locations WHERE `+where+`)`, args...); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM timeline_segments WHERE EXISTS (
		SELECT 1 FROM locations l WHERE `+where+`
		AND l.user=timeline_segments.user AND l.device=timeline_segments.device
		AND l.tst BETWEEN timeline_segments.start_tst AND timeline_segments.end_tst
	)`, args...)
	return err
}

func (s *Store) EnqueueTimeline(user, device string, dirtyFrom int64) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
		VALUES (?,?,?,'queued',?,?)
		ON CONFLICT(user,device) DO UPDATE SET dirty_from=MIN(dirty_from,excluded.dirty_from),
		generation=generation+1,state=CASE WHEN state='running' THEN state ELSE 'queued' END,
		attempts=CASE WHEN state='running' THEN attempts ELSE 0 END,next_attempt_at=0,updated_at=excluded.updated_at`,
		user, device, dirtyFrom, now, now)
	if err == nil {
		s.publish(Change{Topic: ChangeTimeline, Bulk: true})
	}
	return err
}

func (s *Store) EnqueueAllTimelines() error {
	rows, err := s.db.Query(`SELECT user,device,MIN(tst) FROM locations GROUP BY user,device`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var user, device string
		var from int64
		if err := rows.Scan(&user, &device, &from); err != nil {
			return err
		}
		if err := s.EnqueueTimeline(user, device, from); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) ResetTimelines() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := enqueueTimelineWhereTx(tx, "1=1"); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE timeline_rebuild_jobs SET state='queued',attempts=0,
		lease_owner='',lease_until=0,next_attempt_at=0,last_error=''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM timeline_segments`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(Change{Topic: ChangeTimeline, Bulk: true})
	return nil
}

func (s *Store) EnqueueLegacyTimelineRepairs() error {
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`
		WITH ordered AS (
			SELECT user,device,start_tst,end_tst,start_location_id,end_location_id,kind,gap_before,
				LAG(start_tst) OVER w AS prev_start_tst,
				LAG(end_tst) OVER w AS prev_end_tst,
				LAG(end_location_id) OVER w AS prev_end_location_id,
				LAG(kind) OVER w AS prev_kind
			FROM timeline_segments
			WINDOW w AS (PARTITION BY user,device ORDER BY start_tst,id)
		), candidates AS (
			SELECT user,device,MIN(prev_start_tst) AS dirty_from
			FROM ordered
			WHERE prev_kind IS NOT NULL
				AND gap_before=0
				AND (prev_end_tst!=start_tst OR prev_end_location_id!=start_location_id)
				AND ((prev_kind='stay' AND kind IN ('trip','stay')) OR (prev_kind='trip' AND kind='stay'))
			GROUP BY user,device
		)
		INSERT OR IGNORE INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
		SELECT c.user,c.device,c.dirty_from,'queued',?,?
		FROM candidates c
		WHERE NOT EXISTS (
			SELECT 1 FROM timeline_rebuild_jobs j WHERE j.user=c.user AND j.device=c.device
		)`, now, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n > 0 {
		s.publish(Change{Topic: ChangeTimeline, Bulk: true})
	}
	return nil
}

func (s *Store) RecoverTimelineJobs(now int64) error {
	_, err := s.db.Exec(`
		UPDATE timeline_rebuild_jobs
		SET state=CASE WHEN generation>claimed_generation THEN 'queued' WHEN attempts>=5 THEN 'failed' ELSE 'queued' END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			lease_owner='',lease_until=0,next_attempt_at=0,
			last_error=CASE WHEN generation>claimed_generation THEN '' WHEN attempts>=5 THEN 'retry limit exceeded' ELSE last_error END,
			updated_at=?
		WHERE state='running';
		UPDATE timeline_rebuild_jobs
		SET state='failed',next_attempt_at=0,updated_at=?,last_error='retry limit exceeded'
		WHERE state='retry' AND attempts>=5 AND generation<=claimed_generation`, now, now)
	return err
}

func (s *Store) ClaimTimelineJob(owner string, now, leaseUntil int64) (*TimelineJob, error) {
	var j TimelineJob
	if _, err := s.db.Exec(`UPDATE timeline_rebuild_jobs SET state='failed',lease_owner='',lease_until=0,next_attempt_at=0,updated_at=?,last_error='retry limit exceeded'
		WHERE attempts>=5 AND generation<=claimed_generation AND
		(state IN ('queued','retry') OR (state='running' AND lease_until<=?))`, now, now); err != nil {
		return nil, err
	}
	cursor := s.timelineCursor.Load()
	err := s.db.QueryRow(`UPDATE timeline_rebuild_jobs SET state='running',attempts=CASE WHEN generation>claimed_generation THEN 1 ELSE attempts+1 END,
		claimed_generation=generation,lease_owner=?,lease_until=?,updated_at=?
		WHERE id=(SELECT id FROM timeline_rebuild_jobs WHERE (attempts<5 OR generation>claimed_generation) AND ((state IN ('queued','retry') AND next_attempt_at<=?)
		OR (state='running' AND lease_until<=?)) ORDER BY CASE WHEN id>? THEN 0 ELSE 1 END,id LIMIT 1)
		RETURNING id,user,device,dirty_from,state,generation,claimed_generation,attempts,lease_owner`,
		owner, leaseUntil, now, now, now, cursor).Scan(&j.ID, &j.User, &j.Device, &j.DirtyFrom, &j.State, &j.Generation, &j.ClaimedGeneration, &j.Attempts, &j.LeaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		s.timelineCursor.Store(j.ID)
	}
	return &j, err
}

func (s *Store) RenewTimelineJob(id int64, owner string, until int64) (bool, error) {
	return s.RenewTimelineJobContext(context.Background(), id, owner, until)
}

func (s *Store) RenewTimelineJobContext(ctx context.Context, id int64, owner string, until int64) (bool, error) {
	now := time.Now().Unix()
	r, err := s.db.ExecContext(ctx, `UPDATE timeline_rebuild_jobs SET lease_until=? WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, until, id, owner, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) TimelinePoints(user, device string, from int64) ([]TrackPoint, error) {
	var out []TrackPoint
	err := s.IterateTimelinePoints(context.Background(), user, device, from, 1000, func(batch []TrackPoint) error { out = append(out, batch...); return nil })
	return out, err
}

func (s *Store) IterateTimelinePoints(ctx context.Context, user, device string, from int64, batch int, fn func([]TrackPoint) error) error {
	return iterateTimelinePoints(ctx, s.db, user, device, from, batch, fn)
}

func (s *Store) TimelineRebuildFrom(user, device string, dirty int64) (int64, error) {
	from, _, err := timelineRebuildBoundary(context.Background(), s.db, user, device, dirty)
	return from, err
}

func timelineRebuildBoundary(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, user, device string, dirty int64) (int64, bool, error) {
	var start sql.NullInt64
	var gap bool
	// A completed stay retains its original anchor when replay starts here.
	err := q.QueryRowContext(ctx, `SELECT start_tst,gap_before FROM timeline_segments
		WHERE user=? AND device=? AND kind='stay' AND end_tst<?
		ORDER BY start_tst DESC,id DESC LIMIT 1`, user, device, dirty).Scan(&start, &gap)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if start.Valid {
		return start.Int64, gap, nil
	}
	err = q.QueryRowContext(ctx, `SELECT MIN(tst) FROM locations WHERE user=? AND device=?`, user, device).Scan(&start)
	if err != nil {
		return 0, false, err
	}
	if !start.Valid {
		return dirty, false, nil
	}
	return start.Int64, false, nil
}

func finishTimelineJobTx(tx *sql.Tx, j *TimelineJob, from int64, segments []TimelineSegment, staged bool, cause error) error {
	now := time.Now().Unix()
	var id int64
	err := tx.QueryRow(`UPDATE timeline_rebuild_jobs SET id=id
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>? AND claimed_generation=?
		RETURNING id`, j.ID, j.LeaseOwner, now, j.ClaimedGeneration).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if cause == nil {
		if _, err := tx.Exec(`DELETE FROM timeline_segments WHERE user=? AND device=? AND (start_tst>=? OR end_tst>?)`, j.User, j.Device, from, from); err != nil {
			return err
		}
		if staged {
			if _, err := tx.Exec(`INSERT INTO timeline_segments(user,device,kind,start_location_id,end_location_id,start_tst,end_tst,center_lat,center_lon,distance_m,point_count,gap_before,created_at)
				SELECT user,device,kind,start_location_id,end_location_id,start_tst,end_tst,center_lat,center_lon,distance_m,point_count,gap_before,? FROM timeline_rebuild_segments ORDER BY seq`, now); err != nil {
				return err
			}
		} else {
			for _, v := range segments {
				if _, err := tx.Exec(`INSERT INTO timeline_segments(user,device,kind,start_location_id,end_location_id,start_tst,end_tst,center_lat,center_lon,distance_m,point_count,gap_before,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					v.User, v.Device, v.Kind, v.StartLocationID, v.EndLocationID, v.StartTst, v.EndTst, v.CenterLat, v.CenterLon, v.DistanceM, v.PointCount, v.GapBefore, now); err != nil {
					return err
				}
			}
		}
	}
	state := "queued"
	next := int64(0)
	last := ""
	if cause != nil {
		state = "retry"
		next = time.Unix(now, 0).Add(time.Duration(1<<min(j.Attempts, 8)) * time.Second).Unix()
		last = SafeJobError(cause)
		if j.Attempts >= 5 {
			state = "failed"
		}
	}
	r, err := tx.Exec(`UPDATE timeline_rebuild_jobs SET state=CASE WHEN generation>claimed_generation THEN 'queued' ELSE ? END,
		dirty_from=CASE WHEN generation>claimed_generation THEN dirty_from ELSE ? END,
		attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
		next_attempt_at=CASE WHEN generation>claimed_generation THEN 0 ELSE ? END,
		last_error=CASE WHEN generation>claimed_generation THEN '' ELSE ? END,
		lease_owner='',lease_until=0,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, state, from, next, last, now, j.ID, j.LeaseOwner, now)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}
	if cause == nil {
		if _, err = tx.Exec(`DELETE FROM timeline_rebuild_jobs WHERE id=? AND generation=claimed_generation`, j.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) FinishTimelineJob(j *TimelineJob, from int64, segments []TimelineSegment, cause error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = finishTimelineJobTx(tx, j, from, segments, false, cause); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		s.publish(Change{Topic: ChangeTimeline, Bulk: true})
	}
	return err
}

func (r *TimelineRebuild) Finish(ctx context.Context, j *TimelineJob, from int64, cause error) error {
	tx, err := r.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = finishTimelineJobTx(tx, j, from, nil, true, cause); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		r.store.publish(Change{Topic: ChangeTimeline, Bulk: true})
	}
	return err
}

func timelineFilter(f TimelineFilter, cursor bool) (string, []any) {
	q := `1=1`
	var args []any
	if f.User != "" {
		q += ` AND user=?`
		args = append(args, f.User)
	}
	if f.Device != "" {
		q += ` AND device=?`
		args = append(args, f.Device)
	}
	if f.From > 0 {
		q += ` AND end_tst>=?`
		args = append(args, f.From)
	}
	if f.To > 0 {
		q += ` AND start_tst<=?`
		args = append(args, f.To)
	}
	if cursor && f.BeforeTst > 0 && f.BeforeID > 0 {
		q += ` AND (start_tst,id)<(?,?)`
		args = append(args, f.BeforeTst, f.BeforeID)
	}
	return q, args
}

func (s *Store) TimelineSummary(f TimelineFilter) (TimelineSummary, error) {
	return s.TimelineSummaryContext(context.Background(), f)
}

func (s *Store) TimelineSummaryContext(ctx context.Context, f TimelineFilter) (TimelineSummary, error) {
	return timelineSummaryContext(ctx, s.db, f)
}

func timelineSummaryContext(ctx context.Context, q contextQueryer, f TimelineFilter) (TimelineSummary, error) {
	where, args := timelineFilter(f, false)
	var out TimelineSummary
	rows, err := q.QueryContext(ctx, `SELECT kind,start_tst,end_tst,distance_m FROM timeline_segments WHERE `+where, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var kind string
		var start, end int64
		var distance float64
		if err := rows.Scan(&kind, &start, &end, &distance); err != nil {
			return out, err
		}
		clipStart, clipEnd := start, end
		if f.From > clipStart {
			clipStart = f.From
		}
		if f.To > 0 && f.To < clipEnd {
			clipEnd = f.To
		}
		if clipEnd < clipStart {
			continue
		}
		out.TotalSegments++
		clippedDistance := distance
		if end > start {
			clippedDistance *= float64(clipEnd-clipStart) / float64(end-start)
		}
		out.DistanceM += clippedDistance
		if kind == "stay" {
			out.Stays++
			out.StaySeconds += clipEnd - clipStart
		} else {
			out.Trips++
		}
	}
	return out, rows.Err()
}

func (s *Store) ListTimelinePage(f TimelineFilter) ([]TimelineSegment, error) {
	return s.ListTimelinePageContext(context.Background(), f)
}

func (s *Store) ListTimelinePageContext(ctx context.Context, f TimelineFilter) ([]TimelineSegment, error) {
	return listTimelinePageContext(ctx, s.db, f)
}

func listTimelinePageContext(ctx context.Context, queryer contextQueryer, f TimelineFilter) ([]TimelineSegment, error) {
	if f.Limit < 1 {
		f.Limit = 100
	}
	if f.Limit > 501 {
		f.Limit = 501
	}
	where, args := timelineFilter(f, true)
	query := `SELECT id,user,device,kind,start_location_id,end_location_id,start_tst,end_tst,center_lat,center_lon,distance_m,point_count,gap_before FROM timeline_segments WHERE ` + where
	query += ` ORDER BY start_tst DESC,id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TimelineSegment
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var v TimelineSegment
		if err := rows.Scan(&v.ID, &v.User, &v.Device, &v.Kind, &v.StartLocationID, &v.EndLocationID, &v.StartTst, &v.EndTst, &v.CenterLat, &v.CenterLon, &v.DistanceM, &v.PointCount, &v.GapBefore); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) TimelinePageSummaryContext(ctx context.Context, f TimelineFilter) ([]TimelineSegment, TimelineSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, TimelineSummary{}, err
	}
	defer func() { _ = tx.Rollback() }()
	page, err := listTimelinePageContext(ctx, tx, f)
	if err != nil {
		return nil, TimelineSummary{}, err
	}
	summary, err := timelineSummaryContext(ctx, tx, f)
	if err != nil {
		return nil, TimelineSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, TimelineSummary{}, err
	}
	return page, summary, nil
}

func (s *Store) ListTimeline(user, device string, from, to int64, limit int) ([]TimelineSegment, error) {
	return s.ListTimelinePage(TimelineFilter{User: user, Device: device, From: from, To: to, Limit: limit})
}

func (s *Store) TimelineJobCount() (int, error) {
	return s.TimelineJobCountContext(context.Background())
}

func (s *Store) TimelineJobCountContext(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM timeline_rebuild_jobs WHERE state!='failed'`).Scan(&n)
	return n, err
}

func (s *Store) TimelineJobDiagnostics(now int64) (DurableJobDiagnostics, error) {
	var out DurableJobDiagnostics
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(state='queued'),0),COALESCE(SUM(state='running'),0),
		COALESCE(SUM(state='retry'),0),COALESCE(SUM(state='failed'),0),
		COALESCE(SUM(state='running' AND lease_until<=?),0)
		FROM timeline_rebuild_jobs`, now).Scan(&out.Queued, &out.Running, &out.Retry, &out.Failed, &out.ExpiredLeases)
	return out, err
}
