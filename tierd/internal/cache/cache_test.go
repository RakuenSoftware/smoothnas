package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetOrFetch_CachesResult(t *testing.T) {
	c := New[int](time.Minute)
	calls := 0

	fetch := func() (int, error) {
		calls++
		return 42, nil
	}

	v1, err := c.GetOrFetch(fetch)
	if err != nil || v1 != 42 {
		t.Fatalf("expected 42, got %d (err=%v)", v1, err)
	}

	v2, err := c.GetOrFetch(fetch)
	if err != nil || v2 != 42 {
		t.Fatalf("expected 42, got %d (err=%v)", v2, err)
	}

	if calls != 1 {
		t.Fatalf("expected 1 fetch call, got %d", calls)
	}
}

func TestGetOrFetch_RefetchesAfterTTL(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	calls := 0

	fetch := func() (int, error) {
		calls++
		return calls, nil
	}

	v1, _ := c.GetOrFetch(fetch)
	if v1 != 1 {
		t.Fatalf("expected 1, got %d", v1)
	}

	time.Sleep(15 * time.Millisecond)

	v2, _ := c.GetOrFetch(fetch)
	if v2 != 2 {
		t.Fatalf("expected 2 after TTL, got %d", v2)
	}
}

func TestInvalidate_ForcesRefetch(t *testing.T) {
	c := New[string](time.Minute)
	calls := 0

	fetch := func() (string, error) {
		calls++
		if calls == 1 {
			return "first", nil
		}
		return "second", nil
	}

	v1, _ := c.GetOrFetch(fetch)
	if v1 != "first" {
		t.Fatalf("expected first, got %s", v1)
	}

	c.Invalidate()

	v2, _ := c.GetOrFetch(fetch)
	if v2 != "second" {
		t.Fatalf("expected second after invalidate, got %s", v2)
	}
}

func TestGetOrFetch_DoesNotCacheErrors(t *testing.T) {
	c := New[int](time.Minute)
	calls := 0

	fetch := func() (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("fail")
		}
		return 99, nil
	}

	_, err := c.GetOrFetch(fetch)
	if err == nil {
		t.Fatal("expected error on first call")
	}

	v, err := c.GetOrFetch(fetch)
	if err != nil || v != 99 {
		t.Fatalf("expected 99 after error recovery, got %d (err=%v)", v, err)
	}

	if calls != 2 {
		t.Fatalf("expected 2 fetch calls, got %d", calls)
	}
}

func TestGetOrFetch_SingleFlight(t *testing.T) {
	c := New[int](time.Minute)
	var concurrent atomic.Int32

	fetch := func() (int, error) {
		n := concurrent.Add(1)
		if n > 1 {
			t.Errorf("concurrent fetches detected: %d", n)
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
		return 1, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Invalidate() // force refetch each time
			c.GetOrFetch(fetch)
		}()
	}
	wg.Wait()
}
