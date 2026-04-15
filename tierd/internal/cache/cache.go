package cache

import (
	"sync"
	"time"
)

// Entry is a thread-safe, TTL-based single-value cache.
// It caches the result of an expensive fetch function and serves it
// for subsequent calls within the TTL window.
type Entry[T any] struct {
	mu       sync.Mutex
	data     T
	valid    bool
	cachedAt time.Time
	ttl      time.Duration
}

// New creates a cache entry with the given TTL.
func New[T any](ttl time.Duration) *Entry[T] {
	return &Entry[T]{ttl: ttl}
}

// GetOrFetch returns the cached value if still valid, otherwise calls
// fetch to populate the cache. Concurrent callers block on the mutex
// so only one fetch runs at a time (single-flight).
func (e *Entry[T]) GetOrFetch(fetch func() (T, error)) (T, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.valid && time.Since(e.cachedAt) < e.ttl {
		return e.data, nil
	}

	data, err := fetch()
	if err != nil {
		return data, err
	}

	e.data = data
	e.cachedAt = time.Now()
	e.valid = true
	return data, nil
}

// Invalidate clears the cached value so the next GetOrFetch triggers
// a fresh fetch. Call this after mutations that change the underlying data.
func (e *Entry[T]) Invalidate() {
	e.mu.Lock()
	e.valid = false
	e.mu.Unlock()
}
