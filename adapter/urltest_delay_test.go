package adapter

import (
	"testing"
	"time"
)

func TestSuccessfulProbeDelay(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    uint16
	}{
		{0, 1}, {time.Nanosecond, 1}, {time.Millisecond - time.Nanosecond, 1}, {time.Millisecond, 1},
		{65533 * time.Millisecond, 65533}, {65534 * time.Millisecond, 65534},
		{65535 * time.Millisecond, 65534}, {65536 * time.Millisecond, 65534}, {70 * time.Second, 65534},
	} {
		if got := successfulDelay(tc.elapsed); got != tc.want {
			t.Errorf("successfulDelay(%s) = %d, want %d", tc.elapsed, got, tc.want)
		}
	}
}
