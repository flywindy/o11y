// Package repeat suppresses repeats of identical diagnostic messages so a
// condition that recurs on every scrape, export or poll is logged once per
// window instead of once per occurrence.
package repeat

import (
	"sync"
	"time"
)

// Suppressor remembers when each distinct message was last let through and
// reports whether a new occurrence falls inside the repeat window. The table
// is bounded: expired entries are dropped on every call and, when the table
// is still full, the oldest entry is evicted, so memory never exceeds
// maxEntries regardless of how many distinct messages a caller produces.
//
// A Suppressor is safe for concurrent use.
type Suppressor struct {
	window     time.Duration
	maxEntries int

	mu       sync.Mutex
	lastSeen map[string]time.Time
}

// NewSuppressor returns a Suppressor that lets each distinct message through
// once per window and tracks at most maxEntries messages. Distinct messages
// are rare in the diagnostic paths this serves (one per broken family, one
// per failing endpoint), so maxEntries is a memory guard, not a working-set
// size; a caller that produces more distinct messages than the bound sees
// the oldest ones repeat sooner than window.
func NewSuppressor(window time.Duration, maxEntries int) *Suppressor {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Suppressor{window: window, maxEntries: maxEntries, lastSeen: make(map[string]time.Time)}
}

// Window returns the repeat window.
func (s *Suppressor) Window() time.Duration { return s.window }

// Suppressed records msg as seen now and reports whether it was already seen
// inside the repeat window.
func (s *Suppressor) Suppressed(msg string) bool {
	return s.SuppressedAt(msg, time.Now())
}

// SuppressedAt is Suppressed with an explicit clock reading; it exists so a
// caller with its own clock (or a test) can drive the window deterministically.
func (s *Suppressor) SuppressedAt(msg string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seen, ok := s.lastSeen[msg]; ok && now.Sub(seen) < s.window {
		return true
	}
	for m, seen := range s.lastSeen {
		if now.Sub(seen) >= s.window {
			delete(s.lastSeen, m)
		}
	}
	if len(s.lastSeen) >= s.maxEntries {
		var oldestMsg string
		var oldest time.Time
		first := true
		for m, seen := range s.lastSeen {
			if first || seen.Before(oldest) {
				oldestMsg, oldest, first = m, seen, false
			}
		}
		delete(s.lastSeen, oldestMsg)
	}
	s.lastSeen[msg] = now
	return false
}
