package verification

var overallRank = map[string]int{
	"verified":     0,
	"degraded":     1,
	"pending":      2,
	"inconclusive": 3,
	"missing":      4,
	"error":        5,
	"invalid":      6,
}

func MergeOverall(current, next string) string {
	currentRank, currentKnown := overallRank[current]
	nextRank, nextKnown := overallRank[next]
	if !currentKnown {
		return next
	}
	if !nextKnown {
		return current
	}
	if nextRank > currentRank {
		return next
	}
	return current
}
