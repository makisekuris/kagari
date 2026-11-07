package personal

import (
	"encoding/json"
	"go.uber.org/zap"
)

func (r *Runtime) eventLog(event Event) *zap.Logger {
	log := r.Log
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{zap.Int64("event_id", event.ID), zap.String("event_kind", string(event.Kind))}
	if event.ScheduleID != 0 {
		fields = append(fields, zap.Int64("schedule_id", event.ScheduleID))
	}
	if event.TaskID != 0 {
		fields = append(fields, zap.Int64("task_id", event.TaskID))
	}
	if event.RunID != 0 {
		fields = append(fields, zap.Int64("run_id", event.RunID))
	}
	return log.With(fields...)
}

func (r *Runtime) runLog(run Run) *zap.Logger {
	log := r.Log
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{zap.Int64("event_id", run.EventID), zap.Int64("run_id", run.ID), zap.String("run_kind", string(run.Kind))}
	var input TaskInput
	if json.Unmarshal([]byte(run.Input), &input) == nil && input.Trigger != nil {
		fields = append(fields, zap.Int64("schedule_id", input.Trigger.ScheduleID), zap.Int64("scope_revision", input.Trigger.ScopeRevision), zap.Int64("occurrence_id", input.Trigger.OccurrenceID))
	}
	if run.TaskID != 0 {
		fields = append(fields, zap.Int64("task_id", run.TaskID), zap.Int64("revision", run.Revision))
	}
	return log.With(fields...)
}
