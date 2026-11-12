package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/personal"
)

// Inspection must work alongside an active service without recovering its Run,
// and a typo in the database path must not create an apparently empty service.
func TestInspectionIsReadOnlyAndIncludesMemoryAndDelivery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, path, time.Minute)
	event := openUserEvent(t, s, 1, 10, "research")
	task, run := createTask(t, s, event, "create", "research", "goal", "")
	if _, err := s.WriteMemory(ctx, event.ID, "remember", personal.MemoryChange{Operation: "put", Key: "topic", Value: "research", Evidence: "research"}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{TaskID: task.ID, Text: "queued"}, []string{"queued"})
	if _, err := s.ClaimRun(ctx, personal.RunTask); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.db.Exec(`UPDATE personal_tasks SET status='failed'`); err == nil {
		t.Fatal("inspection connection allowed a write")
	}
	snapshot, err := reader.Diagnostic(ctx, event.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoreID == "" || len(snapshot.Events) != 1 || len(snapshot.Tasks) != 1 || len(snapshot.Deliveries) != 1 {
		t.Fatalf("incomplete associations: %+v", snapshot)
	}
	if len(snapshot.Memories) != 1 || snapshot.Memories[0].Key != "topic" || snapshot.Memories[0].Value != "research" {
		t.Fatalf("synchronous memory change missing from snapshot: %+v", snapshot.Memories)
	}
	foundTask, foundMemory := false, false
	for _, got := range snapshot.Runs {
		if got.ID == run.ID {
			foundTask = got.Status == personal.RunRunning
		}
		if got.Kind == personal.RunMemoryReview {
			foundMemory = true
		}
	}
	if !foundTask || !foundMemory {
		t.Fatalf("missing active or maintenance Run: %+v", snapshot.Runs)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenExisting(ctx, missing); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("inspection created database: %v", err)
	}
}

// Multi-task turns have no single task association on the reply. Request links
// must nevertheless make every task discoverable from the source event.
func TestDiagnosticFollowsMultiTaskRequestLinks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	event := openUserEvent(t, s, 1, 10, "two tasks")
	first, _ := createTask(t, s, event, "one", "one", "one", "")
	createTask(t, s, event, "two", "two", "two", "")
	finishEvent(t, s, event, personal.TurnOutput{Text: "both queued"}, []string{"both queued"})
	all, err := s.Diagnostic(ctx, event.ID, 0)
	if err != nil || len(all.Tasks) != 2 || len(all.Deliveries) != 1 {
		t.Fatalf("event trace: %+v, %v", all, err)
	}
	one, err := s.Diagnostic(ctx, 0, first.ID)
	if err != nil || len(one.Tasks) != 1 || one.Tasks[0].ID != first.ID || len(one.Events) != 1 {
		t.Fatalf("task trace: %+v, %v", one, err)
	}
}
