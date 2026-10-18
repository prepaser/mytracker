package store

type locationQueryFilter struct {
	user   string
	device string
	from   int64
	to     int64
}

func buildLocationFilter(f locationQueryFilter) (string, []any) {
	q := `1=1`
	var args []any
	if f.user != "" {
		q += ` AND user=?`
		args = append(args, f.user)
	}
	if f.device != "" {
		q += ` AND device=?`
		args = append(args, f.device)
	}
	if f.from > 0 {
		q += ` AND tst>=?`
		args = append(args, f.from)
	}
	if f.to > 0 {
		q += ` AND tst<=?`
		args = append(args, f.to)
	}
	return q, args
}
