package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestExecuteFailFastPlanStopsAfterWarmupFailure(t *testing.T) {
	calls := 0
	plan := executeFailFastPlan(3, 4, func() result {
		calls++
		return result{errMsg: "warmup failed"}
	})

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if plan.failurePhase != "warmup" {
		t.Fatalf("failure phase = %q, want warmup", plan.failurePhase)
	}
	if len(plan.warmups) != 1 {
		t.Fatalf("attempted warmups = %d, want 1", len(plan.warmups))
	}
	if len(plan.measurements) != 0 {
		t.Fatalf("attempted measurements = %d, want 0", len(plan.measurements))
	}
}

func TestExecuteFailFastPlanStopsAfterMeasuredFailure(t *testing.T) {
	calls := 0
	plan := executeFailFastPlan(2, 4, func() result {
		calls++
		if calls == 4 {
			return result{errMsg: "measurement failed"}
		}
		return result{ok: true}
	})

	if calls != 4 {
		t.Fatalf("calls = %d, want 4 (two warmups and two measurements)", calls)
	}
	if plan.failurePhase != "measurement" {
		t.Fatalf("failure phase = %q, want measurement", plan.failurePhase)
	}
	if len(plan.warmups) != 2 {
		t.Fatalf("attempted warmups = %d, want 2", len(plan.warmups))
	}
	if len(plan.measurements) != 2 {
		t.Fatalf("attempted measurements = %d, want 2", len(plan.measurements))
	}
}

func TestExecuteBatchFailClosedPlanRetainsWarmupBatchAndSkipsMeasurements(t *testing.T) {
	calls := []int{}
	plan := executeBatchFailClosedPlan(3, 4, func(count int) []result {
		calls = append(calls, count)
		return []result{
			{ok: true},
			{errMsg: "status 404"},
			{ok: true},
		}
	})

	if len(calls) != 1 || calls[0] != 3 {
		t.Fatalf("batch calls = %v, want only warmup count 3", calls)
	}
	if plan.failurePhase != "warmup" {
		t.Fatalf("failure phase = %q, want warmup", plan.failurePhase)
	}
	if len(plan.warmups) != 3 || len(plan.measurements) != 0 {
		t.Fatalf(
			"warmups/measurements = %d/%d, want 3/0",
			len(plan.warmups),
			len(plan.measurements),
		)
	}
}

func TestExecuteBatchFailClosedPlanRetainsMeasuredFailureBatch(t *testing.T) {
	calls := 0
	plan := executeBatchFailClosedPlan(2, 3, func(count int) []result {
		calls++
		if calls == 1 {
			return []result{{ok: true}, {ok: true}}
		}
		return []result{{ok: true}, {errMsg: "unexpected EOF"}, {ok: true}}
	})

	if calls != 2 {
		t.Fatalf("batch calls = %d, want 2", calls)
	}
	if plan.failurePhase != "measurement" {
		t.Fatalf("failure phase = %q, want measurement", plan.failurePhase)
	}
	if len(plan.warmups) != 2 || len(plan.measurements) != 3 {
		t.Fatalf(
			"warmups/measurements = %d/%d, want 2/3",
			len(plan.warmups),
			len(plan.measurements),
		)
	}
}

func TestFirstFailureFindsConcurrentBatchFailure(t *testing.T) {
	results := []result{
		{ok: true},
		{errMsg: "status 404"},
		{ok: true},
	}
	index, ok := firstFailure(results)
	if !ok || index != 1 {
		t.Fatalf("firstFailure = %d/%t, want 1/true", index, ok)
	}
}

func TestFirstFailureAcceptsSuccessfulWarmupBatch(t *testing.T) {
	if index, ok := firstFailure([]result{{ok: true}, {ok: true}}); ok {
		t.Fatalf("firstFailure = %d/true, want no failure", index)
	}
}

func TestMeasureOneRetainsTruncatedDownloadDiagnostics(t *testing.T) {
	const (
		size    = int64(1 << 20)
		partial = int64(384 << 10)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, &patternReader{n: partial}); err != nil {
			t.Errorf("write partial response: %v", err)
		}
	}))
	defer server.Close()

	got := measureOne(server.Client(), server.URL, size, "download", "")
	if got.errMsg == "" {
		t.Fatal("truncated response unexpectedly succeeded")
	}
	if got.bytes != partial {
		t.Fatalf("partial response bytes = %d, want %d", got.bytes, partial)
	}
	if got.elapsedMs <= 0 {
		t.Fatalf("failed response elapsed_ms = %v, want > 0", got.elapsedMs)
	}
	if got.ok {
		t.Fatal("truncated response reported ok")
	}
}

func TestMeasureOneUploadEarlyResponseHasRaceSafeProgressSnapshot(t *testing.T) {
	const size = int64(8 << 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stop", http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()

	got := measureOne(server.Client(), server.URL, size, "upload", strings.Repeat("0", 64))
	if got.errMsg != "status 413" {
		t.Fatalf("upload error = %q, want status 413", got.errMsg)
	}
	if got.bytes < 0 || got.bytes > size {
		t.Fatalf("upload progress snapshot = %d, want 0..%d", got.bytes, size)
	}
	if got.elapsedMs <= 0 {
		t.Fatalf("failed upload elapsed_ms = %v, want > 0", got.elapsedMs)
	}
	if got.ok {
		t.Fatal("rejected upload reported ok")
	}
}
