package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/app"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/store"
)

func TestDigestCutoffAndLocalReplay(t *testing.T) {
	cutoff := time.Date(2026, 10, 2, 12, 30, 0, 123, time.FixedZone("CST", 8*3600))
	request, err := digestRequest([]string{"-user", "7", "-cutoff", cutoff.Format(time.RFC3339Nano)}, time.Now(), "Asia/Shanghai")
	if err != nil || !request.End.Equal(cutoff) || !request.Start.Equal(cutoff.AddDate(0, 0, -7)) {
		t.Fatalf("cutoff request: %+v %v", request, err)
	}
	for _, args := range [][]string{{"-start", "2026-09-01"}, {"-cutoff", "bad"}, {"-cutoff", cutoff.Format(time.RFC3339), "-end", "2026-10-02"}, {"-start", "2026-10-02", "-end", "2026-10-01"}} {
		if _, err := digestRequest(args, cutoff, "Asia/Shanghai"); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.APIKey = ""
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	worker := &app.Worker{Store: s, Config: cfg, Log: zap.NewNop()}
	args := []string{"-user", "7", "-cutoff", cutoff.Format(time.RFC3339Nano)}
	var first, second bytes.Buffer
	if err := runDigest(context.Background(), worker, args, &first); err != nil {
		t.Fatal(err)
	}
	if err := runDigest(context.Background(), worker, args, &second); err != nil || first.String() != second.String() || !strings.HasPrefix(second.String(), "所选时间范围内暂无") {
		t.Fatalf("local replay: %q %q %v", first.String(), second.String(), err)
	}
}

func TestDigestCommandDefersWhenAnalysisIsPending(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	worker := &app.Worker{Store: s, Config: cfg, Log: zap.NewNop()}
	cutoff := time.Now().UTC()
	args := []string{"-user", "7", "-cutoff", cutoff.Format(time.RFC3339Nano)}
	request, err := digestRequest(args, cutoff, cfg.Weekly.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(domain.Submission{UserID: 7, ReceivedAt: cutoff.Add(-time.Hour), CacheKey: "pending"})
	if _, _, err := s.Enqueue(context.Background(), "analyze", "pending", payload, nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runDigest(context.Background(), worker, args, &output)
	if err == nil || !strings.Contains(err.Error(), "kagari run") || !strings.Contains(err.Error(), "later") || output.Len() != 0 {
		t.Fatalf("digest command did not report its dependency: output=%q err=%v", output.String(), err)
	}
	id, created, err := worker.EnqueueDigest(context.Background(), request, nil)
	if err != nil || created {
		t.Fatalf("digest command lost idempotency: id=%d created=%v err=%v", id, created, err)
	}
	job, err := s.Job(context.Background(), id)
	if err != nil || job.Status != "pending" || job.Attempts != 0 || len(job.Result) != 0 {
		t.Fatalf("deferred CLI task was not preserved: %+v %v", job, err)
	}
}
