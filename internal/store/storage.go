package store

import (
	"context"
	"database/sql"
	"time"
)

type StorageStats struct {
	LogicalBytes  int64   `json:"logical_bytes"`
	PageCount     int64   `json:"page_count"`
	FreePages     int64   `json:"free_pages"`
	FreePageRatio float64 `json:"free_page_ratio"`
	Locations     int64   `json:"locations"`
	OldestAt      string  `json:"oldest_at,omitempty"`
	NewestAt      string  `json:"newest_at,omitempty"`
}

func (s *Store) StorageStats() (StorageStats, error) {
	return s.StorageStatsContext(context.Background())
}

func (s *Store) StorageStatsContext(ctx context.Context) (StorageStats, error) {
	var out StorageStats
	var pageSize int64
	for q, dest := range map[string]*int64{
		`PRAGMA page_size`:      &pageSize,
		`PRAGMA page_count`:     &out.PageCount,
		`PRAGMA freelist_count`: &out.FreePages,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dest); err != nil {
			return out, err
		}
	}
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),MIN(tst),MAX(tst) FROM locations`).
		Scan(&out.Locations, &oldest, &newest); err != nil {
		return out, err
	}
	out.LogicalBytes = pageSize * out.PageCount
	if out.PageCount > 0 {
		out.FreePageRatio = float64(out.FreePages) / float64(out.PageCount)
	}
	if oldest.Valid {
		out.OldestAt = time.Unix(oldest.Int64, 0).UTC().Format(time.RFC3339)
	}
	if newest.Valid {
		out.NewestAt = time.Unix(newest.Int64, 0).UTC().Format(time.RFC3339)
	}
	return out, nil
}

func (s *Store) MissingRangeIndexes() ([]string, error) {
	required := []string{
		"idx_locations_tst_id",
		"idx_locations_user_tst_id",
		"idx_locations_device_tst_id",
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_schema WHERE type='index' AND tbl_name='locations'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	found := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range required {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}
