package engine

import (
	"testing"
	"time"
)

func TestWaitForNextScan(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		elapsed  time.Duration
		want     time.Duration
	}{
		{name: "fast scan waits the remainder", interval: 10 * time.Second, elapsed: 3 * time.Second, want: 7 * time.Second},
		{name: "scan exactly on time still yields a beat", interval: 10 * time.Second, elapsed: 10 * time.Second, want: time.Second},
		{name: "overrun uses the floor", interval: 10 * time.Second, elapsed: 12 * time.Second, want: time.Second},
		{name: "sub-second remainder uses the floor", interval: 10 * time.Second, elapsed: 9500 * time.Millisecond, want: time.Second},
		{name: "invalid interval uses the floor", interval: 0, elapsed: 0, want: time.Second},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := waitForNextScan(test.interval, test.elapsed); got != test.want {
				t.Fatalf("waitForNextScan(%s, %s) = %s, want %s", test.interval, test.elapsed, got, test.want)
			}
		})
	}
}

func TestScanBudgetLeavesRoomForTheCadence(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "ten seconds leaves one second", interval: 10 * time.Second, want: 9 * time.Second},
		{name: "minute follows the interval", interval: time.Minute, want: 59 * time.Second},
		{name: "long interval keeps the ceiling", interval: 2 * time.Minute, want: 90 * time.Second},
		{name: "zero falls back to the ceiling", interval: 0, want: 90 * time.Second},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := scanBudget(test.interval); got != test.want {
				t.Fatalf("scanBudget(%s) = %s, want %s", test.interval, got, test.want)
			}
		})
	}
}
