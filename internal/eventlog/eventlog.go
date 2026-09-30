package eventlog

import (
	"sync"
	"time"
)

type Event struct {
	Sequence uint64         `json:"sequence"`
	Time     time.Time      `json:"time"`
	Type     string         `json:"type"`
	Route    string         `json:"route,omitempty"`
	Backend  string         `json:"backend,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

type Filter struct {
	Type  string
	Route string
	Limit int
}

type Log struct {
	mu       sync.RWMutex
	events   []Event
	next     int
	count    int
	sequence uint64
	now      func() time.Time
}

func New(capacity int) *Log {
	if capacity < 1 {
		capacity = 256
	}
	return &Log{events: make([]Event, capacity), now: time.Now}
}

func (l *Log) Add(event Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sequence++
	event.Sequence = l.sequence
	event.Time = l.now().UTC()
	event.Details = cloneDetails(event.Details)
	l.events[l.next] = event
	l.next = (l.next + 1) % len(l.events)
	if l.count < len(l.events) {
		l.count++
	}
	return event
}

func (l *Log) List(filter Filter) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	limit := filter.Limit
	if limit <= 0 || limit > l.count {
		limit = l.count
	}
	result := make([]Event, 0, limit)
	for offset := 0; offset < l.count && len(result) < limit; offset++ {
		index := (l.next - 1 - offset + len(l.events)) % len(l.events)
		event := l.events[index]
		if filter.Type != "" && event.Type != filter.Type {
			continue
		}
		if filter.Route != "" && event.Route != filter.Route {
			continue
		}
		event.Details = cloneDetails(event.Details)
		result = append(result, event)
	}
	return result
}

func cloneDetails(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	clone := make(map[string]any, len(details))
	for key, value := range details {
		clone[key] = value
	}
	return clone
}
