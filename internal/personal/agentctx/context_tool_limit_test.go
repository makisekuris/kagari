package agentctx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/personal"
)

type readFailureRepository struct {
	ReadRepository
	failure error
}

func (r readFailureRepository) ReadRecord(context.Context, personal.ReadScope, personal.RecordRequest) (personal.RecordText, error) {
	return personal.RecordText{}, r.failure
}

func TestBoundToolsNormalizesOnlyDomainNotFound(t *testing.T) {
	invoke := func(failure error) (string, error) {
		t.Helper()
		ctx := context.Background()
		tools, err := ReadTools(ReadToolsConfig{Store: readFailureRepository{failure: failure}, Scope: personal.ReadScope{EventID: 1}})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range BoundTools(tools) {
			info, err := candidate.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.Name != "read_run" {
				continue
			}
			invokable, ok := candidate.(tool.InvokableTool)
			if !ok {
				t.Fatal("read_run is not invokable")
			}
			return invokable.InvokableRun(ctx, `{"run_id":1,"section":"result"}`)
		}
		t.Fatal("read_run tool not found")
		return "", nil
	}

	raw, err := invoke(fmt.Errorf("read record: %w", personal.ErrRecordNotFound))
	if err != nil || raw != `{"error":"record_not_found"}` {
		t.Fatalf("domain miss = (%q, %v)", raw, err)
	}

	diskErr := errors.New("database is closed")
	raw, err = invoke(diskErr)
	if raw != "" || !errors.Is(err, diskErr) {
		t.Fatalf("operational failure = (%q, %v), want original error", raw, err)
	}
}
