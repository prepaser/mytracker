package web

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const maxTrackHours = 24 * 3650

func parseLocationRange(q url.Values, now time.Time, hoursAllowed bool) (int64, int64, error) {
	var from, to int64
	if raw := q.Get("hours"); raw != "" {
		if !hoursAllowed {
			return 0, 0, fmt.Errorf("hours is not supported")
		}
		hours, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || hours < 1 || hours > maxTrackHours {
			return 0, 0, fmt.Errorf("hours must be between 1 and %d", maxTrackHours)
		}
		from = now.Unix() - hours*int64(time.Hour/time.Second)
	}
	var err error
	if raw := q.Get("from"); raw != "" {
		from, err = parseUnix(raw, "from")
		if err != nil {
			return 0, 0, err
		}
	}
	if raw := q.Get("to"); raw != "" {
		to, err = parseUnix(raw, "to")
		if err != nil {
			return 0, 0, err
		}
	}
	if from > 0 && to > 0 && from > to {
		return 0, 0, fmt.Errorf("from must not be after to")
	}
	return from, to, nil
}

func parseUnix(raw, name string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s is invalid", name)
	}
	return value, nil
}

func parseTrackLimit(raw string) (int, error) {
	if raw == "" || raw == "0" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 20_000 {
		return 0, fmt.Errorf("limit must be between 1 and 20000")
	}
	return value, nil
}
