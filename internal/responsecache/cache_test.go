package responsecache

import (
	"net/http"
	"testing"
	"time"
)

func TestCacheExpiresAndEvictsOldest(t *testing.T) {
	now := time.Unix(100, 0)
	cache := New(2)
	cache.now = func() time.Time { return now }
	entry := func(value string, ttl time.Duration) Entry {
		return Entry{Status: http.StatusOK, Body: []byte(value), StoredAt: now, ExpiresAt: now.Add(ttl)}
	}
	cache.Set("a", entry("a", time.Minute))
	now = now.Add(time.Second)
	cache.Set("b", entry("b", time.Minute))
	now = now.Add(time.Second)
	cache.Set("c", entry("c", time.Minute))
	if _, ok := cache.Get("a"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if got, ok := cache.Get("c"); !ok || string(got.Body) != "c" {
		t.Fatalf("newest entry = %q, %v", got.Body, ok)
	}
	now = now.Add(time.Minute)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("expired entry was returned")
	}
}

func TestCacheCopiesMutableValues(t *testing.T) {
	cache := New(1)
	body := []byte("original")
	header := http.Header{"X-Test": []string{"original"}}
	cache.Set("key", Entry{Status: 200, Body: body, Header: header, StoredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	body[0] = 'X'
	header.Set("X-Test", "changed")
	got, ok := cache.Get("key")
	if !ok || string(got.Body) != "original" || got.Header.Get("X-Test") != "original" {
		t.Fatalf("cached values were mutated: body=%q header=%q", got.Body, got.Header.Get("X-Test"))
	}
}
