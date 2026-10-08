package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestDeliveryFormatAndTextSurviveRetryAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.sqlite")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := s.Enqueue(ctx, "analyze", "delivery-format", []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	const text = "**literal markdown**\nsecond line"
	if err := s.CompletePublication(ctx, jobID, []byte(`{}`), []domain.Delivery{{
		Target: domain.DeliveryTarget{Channel: "telegram", Address: "7"},
		Part:   1,
		Text:   text,
		Format: domain.ContentMarkdown,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil {
		t.Fatalf("ClaimDelivery() = (%+v, %v)", delivery, err)
	}
	if err := s.FailDelivery(ctx, delivery.ID, "temporary", 1, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RetryDeliveries(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	delivery, err = s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.Text != text || delivery.Format != domain.ContentMarkdown {
		t.Fatalf("retried delivery = (%+v, %v), want original text and markdown format", delivery, err)
	}
}
