package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"kagari/internal/agent"
	"kagari/internal/app"
	"kagari/internal/browser"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/persona"
	"kagari/internal/personal"
	"kagari/internal/telegram"

	"go.uber.org/zap"
)

func runPersonal(ctx context.Context, cfg config.Config, read func(context.Context, string) (domain.Source, error), log *zap.Logger) (runErr error) {
	if err := cfg.ValidatePersonal(); err != nil {
		return err
	}
	unlock, err := app.Lock(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := personal.NewStore(cfg.Storage.Path, cfg.Personal.TaskBudget)
	if err != nil {
		return err
	}
	defer s.Close()
	ownerID := cfg.Telegram.AllowedUserIDs[0]
	if err := s.BindOwner(ctx, ownerID); err != nil {
		return err
	}
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		return err
	}
	directory := cfg.Logging.Directory
	if directory == "" {
		directory = logging.DefaultDirectory(cfg.Storage.Path)
	}
	var level zap.AtomicLevel
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	session, err := logging.Open(logging.Options{
		Directory: directory, StoreID: storeID, Console: os.Stderr, Level: level.Level(),
		RotateBytes: cfg.Logging.RotateMiB << 20, MaxBytes: cfg.Logging.MaxTotalMiB << 20,
		MaxAge:  time.Duration(cfg.Logging.RetentionDays) * 24 * time.Hour,
		Secrets: []string{cfg.Model.APIKey, cfg.Telegram.Token},
	})
	if err != nil {
		return fmt.Errorf("initialize service logs: %w", err)
	}
	log = session.Logger
	ctx = logging.WithLogger(ctx, log)
	defer func() {
		if runErr != nil {
			log.Error("service.stopped", logging.ErrorFields(runErr)...)
		} else {
			log.Info("service.stopped")
		}
		runErr = errors.Join(runErr, session.Close())
	}()
	// No polling or worker starts before the persistent log has opened successfully.
	log.Info("service.starting", zap.String("log_directory", directory), zap.Int("task_concurrency", cfg.Personal.TaskConcurrency))
	role := persona.Default()
	engine, err := agent.New(ctx, cfg, read, nil, role)
	if err != nil {
		return err
	}
	engine.Log = log
	if cfg.Browser.Enabled {
		engine.OpenBrowser = func(ctx context.Context) (func(context.Context, string) (domain.Source, error), func(), error) {
			return browser.Open(ctx, cfg)
		}
	}
	main := &personal.MainAgent{Store: s, Log: log, Model: engine.Model, Persona: role.Prompt(), Profile: engine.Profile, MaxIterations: cfg.Agent.MaxIterations, Timeout: cfg.Agent.Timeout}
	client, err := telegram.NewPersonal(cfg.Telegram, s, log)
	if err != nil {
		return err
	}
	adapter := telegram.Adapter(client.Send)
	runtime := &personal.Runtime{
		Store: s, Think: main.Think, Execute: personal.ReadingExecutor(engine, s), ReviewMemory: main.ReviewMemory,
		TaskConcurrency: cfg.Personal.TaskConcurrency, Poll: client.Poll, Log: log,
		Prepare: func(text string) ([]string, error) { return adapter.Prepare(domain.Content{Text: text}) },
		Send: func(ctx context.Context, text string) (int64, error) {
			return client.Send(ctx, ownerID, domain.Content{Text: text})
		},
	}
	return runtime.Run(ctx)
}

// Inspection never recovers queued work or changes the running service's state.
func inspectPersonal(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: kagari tasks [TASK_ID] | runs TASK_ID | memories [QUERY] | memory-history [KEY]")
	}
	var taskID int64
	if args[0] == "runs" || (args[0] == "tasks" && len(args) == 2) {
		if len(args) != 2 {
			return errors.New("usage: kagari runs TASK_ID")
		}
		var err error
		taskID, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil || taskID <= 0 {
			return errors.New("TASK_ID must be a positive integer")
		}
	}
	s, err := personal.OpenExisting(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	var result any
	switch args[0] {
	case "tasks":
		if taskID == 0 {
			result, err = s.Tasks(ctx, 100)
		} else {
			var task *personal.Task
			task, err = s.Task(ctx, taskID)
			if err == nil && task == nil {
				err = fmt.Errorf("task #%d does not exist", taskID)
			}
			result = task
		}
	case "runs":
		result, err = s.Runs(ctx, taskID, 100)
	case "memories", "memory-history":
		query := ""
		if len(args) == 2 {
			query = args[1]
		}
		if args[0] == "memories" {
			result, err = s.Memories(ctx, query, 100)
		} else {
			result, err = s.MemoryHistory(ctx, query, 100)
		}
	default:
		return errors.New("unknown personal inspection command")
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
