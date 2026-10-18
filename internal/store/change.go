package store

import "sync"

const (
	ChangeLocations    = "locations"
	ChangeStampJobs    = "stamp_jobs"
	ChangeVerification = "verification"
	ChangeMaintenance  = "maintenance"
	ChangeShares       = "shares"
	ChangeMQTT         = "mqtt"
	ChangeConfig       = "config"
	ChangeTimeline     = "timeline"
	ChangeGeofences    = "geofences"
)

type Change struct {
	Topic      string
	LocationID int64
	User       string
	Device     string
	Tst        int64
	Bulk       bool
}

type ChangeSink interface {
	Publish(Change)
}

func validChangeTopic(topic string) bool {
	switch topic {
	case ChangeLocations, ChangeStampJobs, ChangeVerification, ChangeMaintenance,
		ChangeShares, ChangeMQTT, ChangeConfig, ChangeTimeline, ChangeGeofences:
		return true
	default:
		return false
	}
}

type changePublisher struct {
	mu   sync.RWMutex
	sink ChangeSink
}

func (p *changePublisher) set(sink ChangeSink) {
	p.mu.Lock()
	p.sink = sink
	p.mu.Unlock()
}

func (p *changePublisher) publish(change Change) {
	if !validChangeTopic(change.Topic) {
		return
	}
	p.mu.RLock()
	sink := p.sink
	p.mu.RUnlock()
	if sink != nil {
		sink.Publish(change)
	}
}
