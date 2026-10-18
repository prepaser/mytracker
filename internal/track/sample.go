package track

import (
	"math"
	"sort"

	"github.com/prepaser/mytracker/internal/store"
)

const Algorithm = "trajectory-v1"

type Metadata struct {
	TotalPoints    int    `json:"total_points"`
	ReturnedPoints int    `json:"returned_points"`
	Sampled        bool   `json:"sampled"`
	Algorithm      string `json:"algorithm,omitempty"`
}

type Result struct {
	Locations []store.Location
	Metadata  Metadata
}

type point struct {
	x float64
	y float64
}

type bucket struct {
	x float64
	y float64
	n int
}

type longitudeUnwrapper struct {
	previous float64
	have     bool
}

func (u *longitudeUnwrapper) project(p store.TrackPoint, fn Project) point {
	x, y := fn(p)
	if u.have {
		x = unwrapLongitude(x, u.previous)
	}
	u.previous, u.have = x, true
	return point{x: x, y: y}
}

type Project func(store.TrackPoint) (float64, float64)

func Sample(snapshot *store.TrackSnapshot, project Project) (Result, error) {
	total, err := snapshot.Count()
	if err != nil {
		return Result{}, err
	}
	limit := snapshot.Limit()
	if total <= limit {
		locs := make([]store.Location, 0, total)
		err := snapshot.Iterate(500, func(batch []store.Location) error {
			locs = append(locs, batch...)
			return nil
		})
		return Result{
			Locations: locs,
			Metadata: Metadata{
				TotalPoints: total, ReturnedPoints: len(locs),
			},
		}, err
	}
	if limit < 2 {
		limit = 2
	}
	groups, order, firstKey, lastKey, overflow, err := collectGroups(snapshot, limit)
	if err != nil {
		return Result{}, err
	}
	if overflow {
		return sampleTemporal(snapshot, total, limit)
	}
	if len(groups) <= 1 {
		return sampleSingle(snapshot, total, limit, project)
	}
	allocateQuotas(groups, order, firstKey, lastKey, limit)
	return sampleMultiple(snapshot, total, limit, project, groups, order, lastKey)
}

type identity struct {
	user, device string
}

type identityGroup struct {
	count, quota  int
	first, last   store.TrackPoint
	lastProjected point
	averages      []bucket
}

func collectGroups(snapshot *store.TrackSnapshot, maxGroups int) (map[identity]*identityGroup, []identity, identity, identity, bool, error) {
	groups := make(map[identity]*identityGroup)
	var order []identity
	var firstKey, lastKey identity
	overflow := false
	err := snapshot.IteratePoints(1000, func(points []store.TrackPoint) error {
		for _, point := range points {
			key := identity{user: point.User, device: point.Device}
			if len(order) == 0 {
				firstKey = key
			}
			lastKey = key
			group := groups[key]
			if group == nil {
				if len(groups) >= maxGroups {
					overflow = true
					continue
				}
				group = &identityGroup{first: point}
				groups[key] = group
				order = append(order, key)
			}
			group.count++
			group.last = point
		}
		return nil
	})
	return groups, order, firstKey, lastKey, overflow, err
}

func allocateQuotas(groups map[identity]*identityGroup, order []identity, firstKey, lastKey identity, limit int) {
	baseline := 0
	for _, key := range order {
		group := groups[key]
		group.quota = min(group.count, 2)
		baseline += group.quota
	}
	if baseline > limit {
		for _, key := range order {
			groups[key].quota = 0
		}
		if firstKey == lastKey {
			groups[firstKey].quota = min(groups[firstKey].count, 2)
		} else {
			groups[firstKey].quota = 1
			groups[lastKey].quota = 1
		}
		allocated := quotaTotal(groups, order)
		for _, key := range order {
			if allocated >= limit {
				break
			}
			if groups[key].quota == 0 {
				groups[key].quota = 1
				allocated++
			}
		}
		baseline = allocated
	}
	for remaining := limit - baseline; remaining > 0; {
		progressed := false
		for _, key := range order {
			group := groups[key]
			if group.quota >= group.count {
				continue
			}
			group.quota++
			remaining--
			progressed = true
			if remaining == 0 {
				break
			}
		}
		if !progressed {
			break
		}
	}
}

func sampleTemporal(snapshot *store.TrackSnapshot, total, limit int) (Result, error) {
	targets := make([]int, limit)
	denominator := int64(limit - 1)
	for i := range targets {
		targets[i] = int((int64(i)*int64(total-1) + denominator/2) / denominator)
	}
	selected := make([]store.TrackPoint, 0, limit)
	index, target := 0, 0
	err := snapshot.IteratePoints(1000, func(batch []store.TrackPoint) error {
		for _, point := range batch {
			if target < len(targets) && index == targets[target] {
				selected = append(selected, point)
				target++
			}
			index++
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	result, err := loadResult(snapshot, total, selected)
	result.Metadata.Algorithm = "temporal-v1"
	return result, err
}

func quotaTotal(groups map[identity]*identityGroup, order []identity) int {
	total := 0
	for _, key := range order {
		total += groups[key].quota
	}
	return total
}

func sampleSingle(snapshot *store.TrackSnapshot, total, limit int, project Project) (Result, error) {
	bucketCount := limit - 2
	if bucketCount == 0 {
		var first, last store.TrackPoint
		i := 0
		err := snapshot.IteratePoints(1000, func(batch []store.TrackPoint) error {
			for _, p := range batch {
				if i == 0 {
					first = p
				}
				last = p
				i++
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
		return loadResult(snapshot, total, []store.TrackPoint{first, last})
	}

	averages := make([]bucket, bucketCount)
	var first, last store.TrackPoint
	var lastProjected point
	var unwrapped longitudeUnwrapper
	i := 0
	err := snapshot.IteratePoints(1000, func(batch []store.TrackPoint) error {
		for _, p := range batch {
			projected := unwrapped.project(p, project)
			switch i {
			case 0:
				first = p
			case total - 1:
				last = p
				lastProjected = projected
			default:
				b := bucketIndex(i, total, bucketCount)
				averages[b].x += projected.x
				averages[b].y += projected.y
				averages[b].n++
			}
			i++
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for i := range averages {
		averages[i].x /= float64(averages[i].n)
		averages[i].y /= float64(averages[i].n)
	}

	selected := make([]store.TrackPoint, 0, limit)
	selected = append(selected, first)
	var pass longitudeUnwrapper
	previous := pass.project(first, project)
	currentBucket := 0
	var candidate store.TrackPoint
	var candidateProjected point
	maxArea := -1.0
	haveCandidate := false
	i = 0
	err = snapshot.IteratePoints(1000, func(batch []store.TrackPoint) error {
		for _, p := range batch {
			current := pass.project(p, project)
			if i == 0 || i == total-1 {
				i++
				continue
			}
			b := bucketIndex(i, total, bucketCount)
			if b != currentBucket {
				if haveCandidate {
					selected = append(selected, candidate)
					previous = candidateProjected
				}
				currentBucket = b
				maxArea = -1
				haveCandidate = false
			}
			next := lastProjected
			if b+1 < len(averages) {
				next = point{x: averages[b+1].x, y: averages[b+1].y}
			}
			area := triangleArea(previous, current, next)
			if area > maxArea {
				candidate, candidateProjected, maxArea, haveCandidate = p, current, area, true
			}
			i++
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if haveCandidate {
		selected = append(selected, candidate)
	}
	selected = append(selected, last)
	return loadResult(snapshot, total, selected)
}

type selectionState struct {
	index         int
	currentBucket int
	previous      point
	candidate     store.TrackPoint
	candidateProj point
	pass          longitudeUnwrapper
	maxArea       float64
	haveCandidate bool
}

func sampleMultiple(snapshot *store.TrackSnapshot, total, limit int, project Project, groups map[identity]*identityGroup, order []identity, lastKey identity) (Result, error) {
	for _, key := range order {
		group := groups[key]
		if group.quota >= 3 && group.quota < group.count {
			group.averages = make([]bucket, group.quota-2)
		}
	}
	indices := make(map[identity]int, len(groups))
	unwrappers := make(map[identity]*longitudeUnwrapper, len(groups))
	err := snapshot.IteratePoints(1000, func(points []store.TrackPoint) error {
		for _, point := range points {
			key := identity{user: point.User, device: point.Device}
			group := groups[key]
			i := indices[key]
			if len(group.averages) > 0 {
				u := unwrappers[key]
				if u == nil {
					u = &longitudeUnwrapper{}
					unwrappers[key] = u
				}
				projected := u.project(point, project)
				if i == group.count-1 {
					group.lastProjected = projected
				}
				if i != 0 && i != group.count-1 {
					b := bucketIndex(i, group.count, len(group.averages))
					group.averages[b].x += projected.x
					group.averages[b].y += projected.y
					group.averages[b].n++
				}
			}
			indices[key] = i + 1
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for _, key := range order {
		group := groups[key]
		for i := range group.averages {
			group.averages[i].x /= float64(group.averages[i].n)
			group.averages[i].y /= float64(group.averages[i].n)
		}
	}

	selected := make([]store.TrackPoint, 0, limit)
	for _, key := range order {
		group := groups[key]
		switch {
		case group.quota == 0:
		case group.quota >= group.count:
		case group.quota == 1:
			if key == lastKey {
				selected = append(selected, group.last)
			} else {
				selected = append(selected, group.first)
			}
		case group.quota == 2:
			selected = append(selected, group.first, group.last)
		case group.quota >= 3:
			selected = append(selected, group.first, group.last)
		default:
		}
	}

	states := make(map[identity]*selectionState, len(groups))
	for _, key := range order {
		group := groups[key]
		if group.quota >= 3 && group.quota < group.count {
			state := &selectionState{maxArea: -1}
			state.previous = state.pass.project(group.first, project)
			states[key] = state
		}
	}
	err = snapshot.IteratePoints(1000, func(points []store.TrackPoint) error {
		for _, p := range points {
			key := identity{user: p.User, device: p.Device}
			group := groups[key]
			if group.quota >= group.count {
				selected = append(selected, p)
				continue
			}
			state := states[key]
			if state == nil {
				continue
			}
			current := state.pass.project(p, project)
			if state.index == 0 || state.index == group.count-1 {
				state.index++
				continue
			}
			b := bucketIndex(state.index, group.count, group.quota-2)
			if b != state.currentBucket {
				if state.haveCandidate {
					selected = append(selected, state.candidate)
					state.previous = state.candidateProj
				}
				state.currentBucket = b
				state.maxArea = -1
				state.haveCandidate = false
			}
			next := group.lastProjected
			if b+1 < len(group.averages) {
				next = point{x: group.averages[b+1].x, y: group.averages[b+1].y}
			}
			area := triangleArea(state.previous, current, next)
			if area > state.maxArea {
				state.candidate, state.candidateProj, state.maxArea, state.haveCandidate = p, current, area, true
			}
			state.index++
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for _, key := range order {
		if state := states[key]; state != nil && state.haveCandidate {
			selected = append(selected, state.candidate)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Tst == selected[j].Tst {
			return selected[i].ID < selected[j].ID
		}
		return selected[i].Tst < selected[j].Tst
	})
	return loadResult(snapshot, total, selected)
}

func WebMercator(p store.TrackPoint) (float64, float64) {
	lat := math.Max(-85.05112878, math.Min(85.05112878, p.Lat))
	x := p.Lon * math.Pi / 180
	y := math.Log(math.Tan(math.Pi/4 + lat*math.Pi/360))
	return x, y
}

func bucketIndex(i, total, buckets int) int {
	b := (i - 1) * buckets / (total - 2)
	if b >= buckets {
		return buckets - 1
	}
	return b
}

func unwrapLongitude(x, previous float64) float64 {
	delta := x - previous
	if delta > math.Pi {
		x -= 2 * math.Pi * math.Ceil((delta-math.Pi)/(2*math.Pi))
	} else if delta < -math.Pi {
		x += 2 * math.Pi * math.Ceil((-delta-math.Pi)/(2*math.Pi))
	}
	return x
}

func triangleArea(a, b, c point) float64 {
	return math.Abs((a.x-c.x)*(b.y-a.y) - (a.x-b.x)*(c.y-a.y))
}

func loadResult(snapshot *store.TrackSnapshot, total int, points []store.TrackPoint) (Result, error) {
	ids := make([]int64, len(points))
	for i, p := range points {
		ids[i] = p.ID
	}
	locs, err := snapshot.LoadLocations(ids)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Locations: locs,
		Metadata: Metadata{
			TotalPoints: total, ReturnedPoints: len(locs), Sampled: true, Algorithm: Algorithm,
		},
	}, nil
}
