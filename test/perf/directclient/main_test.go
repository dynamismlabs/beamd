package main

import "testing"

func TestRunSampleBatchFailFastStopsAfterFailure(t *testing.T) {
	calls := 0
	samples := runSampleBatch(5, true, func(index int) sample {
		calls++
		if index == 1 {
			return sample{Index: index, Error: "failed"}
		}
		return sample{Index: index, OK: true}
	})

	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].Index != 0 || !samples[0].OK {
		t.Fatalf("first sample = %+v, want successful index 0", samples[0])
	}
	if samples[1].Index != 1 || samples[1].OK || samples[1].Error != "failed" {
		t.Fatalf("second sample = %+v, want failed index 1", samples[1])
	}
}

func TestRunSampleBatchDefaultKeepsGoingAfterFailure(t *testing.T) {
	calls := 0
	samples := runSampleBatch(4, false, func(index int) sample {
		calls++
		return sample{Index: index, OK: index != 1}
	})

	if calls != 4 || len(samples) != 4 {
		t.Fatalf("calls/samples = %d/%d, want 4/4", calls, len(samples))
	}
}
