package mongo

import (
	"math"
	"testing"
)

func TestPoolSize(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want int64
	}{
		{0, 0},
		{100, 100},
		{math.MaxInt64, math.MaxInt64},
		{math.MaxInt64 + 1, math.MaxInt64},
		{math.MaxUint64, math.MaxInt64},
	} {
		if got := poolSize(tc.in); got != tc.want {
			t.Errorf("poolSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
