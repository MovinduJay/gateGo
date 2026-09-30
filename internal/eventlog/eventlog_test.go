package eventlog

import (
	"testing"
	"time"
)

func TestLogIsBoundedOrderedAndFilterable(t *testing.T) {
	log := New(2)
	log.now = func() time.Time { return time.Unix(100, 0) }
	log.Add(Event{Type: "reload", Route: "one"})
	log.Add(Event{Type: "circuit", Route: "two"})
	log.Add(Event{Type: "reload", Route: "two", Details: map[string]any{"result": "success"}})

	events := log.List(Filter{})
	if len(events) != 2 || events[0].Sequence != 3 || events[1].Sequence != 2 {
		t.Fatalf("unexpected bounded order: %+v", events)
	}
	filtered := log.List(Filter{Type: "reload", Route: "two", Limit: 1})
	if len(filtered) != 1 || filtered[0].Sequence != 3 {
		t.Fatalf("unexpected filtered events: %+v", filtered)
	}
	filtered[0].Details["result"] = "mutated"
	if got := log.List(Filter{Limit: 1})[0].Details["result"]; got != "success" {
		t.Fatalf("caller mutated stored details: %v", got)
	}
}
