package responsecache

import (
	"net/http"
	"sync"
	"time"
)

type Entry struct {
	Status    int
	Header    http.Header
	Body      []byte
	StoredAt  time.Time
	ExpiresAt time.Time
}

type Cache struct {
	mu         sync.Mutex
	entries    map[string]Entry
	maxEntries int
	now        func() time.Time
}

func New(maxEntries int) *Cache {
	return &Cache{entries: make(map[string]Entry), maxEntries: maxEntries, now: time.Now}
}

func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Entry{}, false
	}
	if !c.now().Before(entry.ExpiresAt) {
		delete(c.entries, key)
		return Entry{}, false
	}
	entry.Header = entry.Header.Clone()
	entry.Body = append([]byte(nil), entry.Body...)
	return entry, true
}

func (c *Cache) Set(key string, entry Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries <= 0 {
		return
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, current := range c.entries {
			if oldestKey == "" || current.StoredAt.Before(oldest) {
				oldestKey, oldest = candidate, current.StoredAt
			}
		}
		delete(c.entries, oldestKey)
	}
	entry.Header = entry.Header.Clone()
	entry.Body = append([]byte(nil), entry.Body...)
	c.entries[key] = entry
}

func (c *Cache) Purge() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := len(c.entries)
	c.entries = make(map[string]Entry)
	return count
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) Capacity() int { return c.maxEntries }
