package personal

import "go.uber.org/zap"

func (r *Runtime) eventLog(event Event) *zap.Logger {
	log := r.Log
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{zap.Int64("event_id", event.ID), zap.String("event_kind", string(event.Kind))}
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
	if run.TaskID != 0 {
		fields = append(fields, zap.Int64("task_id", run.TaskID), zap.Int64("revision", run.Revision))
	}
	return log.With(fields...)
}
