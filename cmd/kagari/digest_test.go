package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/app"
	"kagari/internal/config"
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
	if err := runDigest(context.Background(), worker, args, &second); err != nil || first.String() != second.String() || !strings.HasPrefix(second.String(), "本周回顾（0条）") {
		t.Fatalf("local replay: %q %q %v", first.String(), second.String(), err)
	}
}
