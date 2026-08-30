package main

import (
	"sync/atomic"
	"testing"
)

func TestKeepGoingComesOnlyFromIXEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"no", false},
		{"YES", false},
		{"yes", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("IX_KEEP_GOING", tc.value)
			graph := &Graph{
				Pools:    map[string]int{"threads": 1},
				TrashDir: t.TempDir(),
			}

			if got := newExecutor(graph).keepGoing; got != tc.want {
				t.Fatalf("IX_KEEP_GOING=%q: keepGoing=%v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestVisitAllCollectsIndependentFailures(t *testing.T) {
	var successfulBranch atomic.Bool
	ex := &executor{out: map[string]*future{
		"failed": {f: func() bool { return true }},
		"good": {f: func() bool {
			successfulBranch.Store(true)

			return false
		}},
	}}

	results := ex.visitAll([]string{"failed", "good"})

	if len(results) != 2 || !results[0] || results[1] {
		t.Fatalf("visitAll results=%v, want [true false]", results)
	}

	if !successfulBranch.Load() {
		t.Fatal("independent branch did not run")
	}
}

func TestFutureRemembersFailure(t *testing.T) {
	var calls atomic.Int32
	f := &future{f: func() bool {
		calls.Add(1)

		return true
	}}

	if !f.callOnce() || !f.callOnce() {
		t.Fatal("future did not preserve its failed result")
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("future called %d times, want 1", got)
	}
}
