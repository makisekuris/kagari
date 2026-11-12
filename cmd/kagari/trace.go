package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"kagari/internal/config"
	"kagari/internal/logging"
	"kagari/internal/personal/sqlite"
)

func inspectEvents(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	limit := flags.Int("limit", 20, "number of recent events (1..1000)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: kagari events [-limit N]")
	}
	s, err := sqlite.OpenExisting(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	events, err := s.RecentEvents(ctx, *limit)
	if err != nil {
		return err
	}
	for i := range events {
		events[i].Text = ""
	}
	return json.NewEncoder(out).Encode(events)
}

func inspectTrace(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("trace", flag.ContinueOnError)
	eventID := flags.Int64("event", 0, "event ID")
	taskID := flags.Int64("task", 0, "task ID")
	full := flags.Bool("full", false, "include redacted call content")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*eventID > 0) == (*taskID > 0) || *eventID < 0 || *taskID < 0 {
		return errors.New("usage: kagari trace (-event ID | -task ID) [-full]")
	}
	s, err := sqlite.OpenExisting(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	state, err := s.Diagnostic(ctx, *eventID, *taskID)
	_ = s.Close()
	if err != nil {
		return err
	}
	ids := traceIDs(state)
	if !*full {
		for i := range state.Events {
			state.Events[i].Text = ""
		}
		for i := range state.Tasks {
			state.Tasks[i].Title = ""
			state.Tasks[i].Goal = ""
			state.Tasks[i].Context = ""
			state.Tasks[i].Summary = ""
		}
		for i := range state.Runs {
			state.Runs[i].Input = ""
			state.Runs[i].Result = ""
		}
		for i := range state.Deliveries {
			state.Deliveries[i].Text = ""
		}
		for i := range state.Memories {
			state.Memories[i].Value = ""
		}
	}
	stateValue, err := logging.Sanitize(state, []string{cfg.Model.APIKey, cfg.Telegram.Token})
	if err != nil {
		return err
	}
	header, err := json.Marshal(map[string]any{"state": stateValue, "state_read_at": time.Now().UTC()})
	if err != nil {
		return err
	}
	if _, err := out.Write(append(header[:len(header)-1], []byte(",\"records\":[")...)); err != nil {
		return err
	}
	directory := cfg.Logging.Directory
	if directory == "" {
		directory = logging.DefaultDirectory(cfg.Storage.Path)
	}
	files, listErr := logging.ListFiles(directory)
	warnings := []string{}
	if listErr != nil {
		warnings = append(warnings, "Log directory is unavailable; database state is shown without historical logs.")
	}
	if state.StoreID == "" {
		warnings = append(warnings, "Database has no logging identity; start the V2 service to initialize it.")
		files = nil
	}
	count, relatedCount := 0, 0
	openCalls := map[string]bool{}
	encoder := json.NewEncoder(out)
	for _, path := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			warnings = append(warnings, "A log file was removed during inspection; history is incomplete.")
			continue
		}
		if err != nil {
			return fmt.Errorf("open trace log: %w", err)
		}
		// ReadBytes has no Scanner's 64 KiB token ceiling. Keep only one complete
		// record in memory while scanning a retention window of up to 1 GiB.
		reader := bufio.NewReader(file)
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				var record map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(line)))
				decoder.UseNumber()
				if err := decoder.Decode(&record); err != nil || decoder.Decode(new(any)) != io.EOF || line[len(line)-1] != '\n' {
					warnings = append(warnings, "An incomplete or invalid log record was skipped.")
				} else if record["store_id"] == state.StoreID && matchesTrace(record, ids) {
					if matchesTraceIDs(record, ids) {
						relatedCount++
					}
					trackTraceCall(record, openCalls)
					if !*full {
						delete(record, "payload")
						delete(record, "text")
						delete(record, "arguments")
						delete(record, "result")
					}
					if count > 0 {
						if _, err := io.WriteString(out, ","); err != nil {
							file.Close()
							return err
						}
					}
					if err := encoder.Encode(record); err != nil {
						file.Close()
						return err
					}
					count++
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					file.Close()
					return readErr
				}
				break
			}
		}
		file.Close()
	}
	if relatedCount == 0 {
		warnings = append(warnings, "No matching historical records remain; they may be expired, unavailable, or never recorded.")
	}
	if len(openCalls) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d calls have a start without a recorded finish; they may still be running or have been interrupted.", len(openCalls)))
	}
	// A missing record cannot prove a missing side effect: DB commits and logging
	// are not one transaction, and runtime logging failures deliberately fail open.
	warnings = append(warnings, "Database state is a current snapshot; retained logs are historical evidence and may be incomplete.")
	if _, err := io.WriteString(out, "],\"warnings\":"); err != nil {
		return err
	}
	if err := encoder.Encode(warnings); err != nil {
		return err
	}
	_, err = io.WriteString(out, "}\n")
	return err
}

func traceIDs(state sqlite.DiagnosticSnapshot) map[string]map[int64]bool {
	ids := map[string]map[int64]bool{"event_id": {}, "task_id": {}, "run_id": {}, "delivery_id": {}}
	for _, event := range state.Events {
		ids["event_id"][event.ID] = true
	}
	for _, task := range state.Tasks {
		ids["task_id"][task.ID] = true
	}
	for _, run := range state.Runs {
		ids["run_id"][run.ID] = true
	}
	for _, delivery := range state.Deliveries {
		ids["delivery_id"][delivery.ID] = true
	}
	return ids
}

func matchesTrace(record map[string]any, ids map[string]map[int64]bool) bool {
	msg, _ := record["msg"].(string)
	if strings.HasPrefix(msg, "service.") || strings.HasPrefix(msg, "logging.") || strings.HasPrefix(msg, "telegram.polling.") ||
		(msg == "store.outcome" && record["operation"] == "store.recover") {
		return true
	}
	return matchesTraceIDs(record, ids)
}

func matchesTraceIDs(record map[string]any, ids map[string]map[int64]bool) bool {
	// A source event can create several tasks; an explicit narrower scope must
	// not fall back to that event and pull a sibling task into this trace.
	for _, key := range []string{"task_id", "run_id", "delivery_id", "event_id"} {
		if number, ok := record[key].(json.Number); ok {
			id, err := number.Int64()
			if err == nil && id > 0 {
				return ids[key][id]
			}
		}
	}
	return false
}

func trackTraceCall(record map[string]any, open map[string]bool) {
	msg, _ := record["msg"].(string)
	keyName := "model_call_id"
	if strings.HasPrefix(msg, "tool.") {
		keyName = "tool_call_id"
	}
	id, ok := record[keyName].(string)
	if !ok || id == "" {
		return
	}
	instance, _ := record["instance_id"].(string)
	// Provider tool IDs need only be unique within their own model conversation.
	key := fmt.Sprint(instance, ":", record["event_id"], ":", record["run_id"], ":", keyName, ":", id)
	if strings.HasSuffix(msg, ".started") {
		open[key] = true
	}
	if strings.HasSuffix(msg, ".finished") || strings.HasSuffix(msg, ".failed") {
		delete(open, key)
	}
}
