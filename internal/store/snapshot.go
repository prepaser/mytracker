package store

import (
	"context"
	"database/sql"
	"errors"
)

func pinSnapshot(ctx context.Context, tx *sql.Tx) error {
	var value int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM locations LIMIT 1`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
