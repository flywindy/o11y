package repeat_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/flywindy/o11y/internal/repeat"
)

// TestSuppressor_RepeatsAfterWindow checks a message passes once per window.
func TestSuppressor_RepeatsAfterWindow(t *testing.T) {
	s := repeat.NewSuppressor(time.Minute, 8)
	now := time.Unix(1_700_000_000, 0)

	assert.False(t, s.SuppressedAt("boom", now), "first occurrence passes")
	assert.True(t, s.SuppressedAt("boom", now.Add(30*time.Second)), "repeat inside the window is suppressed")
	assert.False(t, s.SuppressedAt("other", now), "a different message is tracked separately")
	assert.False(t, s.SuppressedAt("boom", now.Add(time.Minute)), "the message returns once the window elapsed")
}

// TestSuppressor_TracksEachMessageSeparately checks two alternating messages
// are each suppressed on their own clock.
func TestSuppressor_TracksEachMessageSeparately(t *testing.T) {
	s := repeat.NewSuppressor(time.Minute, 8)
	now := time.Unix(1_700_000_000, 0)

	passed := 0
	for i := 0; i < 8; i++ {
		if !s.SuppressedAt("a", now) {
			passed++
		}
		if !s.SuppressedAt("b", now) {
			passed++
		}
		now = now.Add(15 * time.Second)
	}
	// Two minutes of alternating messages: each passes once at t=0 and once at
	// t=60s, so four in total.
	assert.Equal(t, 4, passed)
}

// TestSuppressor_TableIsBounded checks the oldest entry is evicted once the
// table is full, so memory stays bounded.
func TestSuppressor_TableIsBounded(t *testing.T) {
	const maxEntries = 4
	s := repeat.NewSuppressor(time.Hour, maxEntries)
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < maxEntries+2; i++ {
		assert.False(t, s.SuppressedAt(string(rune('a'+i)), now.Add(time.Duration(i)*time.Second)))
	}
	// "a" and "b" were evicted as the oldest entries, so they pass again
	// even though the window has not elapsed; "c" is still tracked.
	later := now.Add(10 * time.Second)
	assert.False(t, s.SuppressedAt("a", later))
	assert.True(t, s.SuppressedAt("d", later), "an entry inside the bound is still suppressed")
}

// TestSuppressor_MinimumOneEntry checks a zero bound still tracks one entry.
func TestSuppressor_MinimumOneEntry(t *testing.T) {
	s := repeat.NewSuppressor(time.Hour, 0)
	now := time.Unix(1_700_000_000, 0)
	assert.False(t, s.SuppressedAt("a", now))
	assert.True(t, s.SuppressedAt("a", now))
	assert.Equal(t, time.Hour, s.Window())
}
