package store

func (s *Store) LastLocation() (*Location, error) {
	return s.LastLocationFor("", "")
}

func (s *Store) LastLocationFor(user, device string) (*Location, error) {
	q := `SELECT ` + locationColumns + ` FROM locations WHERE 1=1`
	var args []any
	if user != "" {
		q += ` AND user=?`
		args = append(args, user)
	}
	if device != "" {
		q += ` AND device=?`
		args = append(args, device)
	}
	q += ` ORDER BY id DESC LIMIT 1`
	locs, err := s.queryLocations(q, args...)
	if err != nil || len(locs) == 0 {
		return nil, err
	}
	return &locs[0], nil
}
