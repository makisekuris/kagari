package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestScheduledExecutorReceivesFrozenWindowAndConflictingMemory(t *testing.T) {
	for _, eligible := range []bool{true, false} {
		t.Run(fmt.Sprint(eligible), func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText("Saved result; fixed background was retained.")}}
			repo := newToolRepo()
			repo.memories["project.language"] = Memory{Key: "project.language", Value: "The project now uses Rust.", Version: 2}
			started := dispatchTestNow.Add(3 * time.Hour)
			trigger := &ScheduleTrigger{ScheduleID: 5, ScheduleRevision: 1, ScopeRevision: 2, OccurrenceID: 7, StartedAt: &started,
				ScheduledAt: dispatchTestNow, EnqueuedAt: dispatchTestNow,
				Window: &CheckWindow{Start: dispatchTestNow, End: started}, Timezone: "UTC", CoverageEligible: eligible,
				RequiredSources: []SourceRequirement{{ID: "feed", Label: "Release feed"}}}
			input := TaskInput{Goal: "Check the release feed", Context: "This plan uses Go.", Trigger: trigger, MemoryRefs: []string{"project.language"}}
			if _, err := runTaskExecutor(t, context.Background(), readingEngine(fixture, nil), repo, executorRun(input), nil); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(trigger)
			if err != nil {
				t.Fatal(err)
			}
			wireTrigger, err := json.Marshal(string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			request := requestInput(fixture.requests[0])
			for _, want := range []string{string(wireTrigger[1 : len(wireTrigger)-1]), input.Context, repo.memories["project.language"].Value,
				"Use started_at as this task's relative-time reference", "Saved task context takes precedence", "historical attribution only"} {
				if !strings.Contains(request, want) {
					t.Fatalf("scheduled executor input lost %q", want)
				}
			}
		})
	}
}
