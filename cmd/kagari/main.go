package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/app"
	"kagari/internal/config"
	"kagari/internal/digest"
	"kagari/internal/domain"
	"kagari/internal/reader"
	"kagari/internal/render"
	"kagari/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kagari:", err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.NewFlagSet("kagari", flag.ContinueOnError)
	configPath := root.String("config", "", "configuration YAML path")
	if err := root.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := root.Args()
	if len(args) == 0 {
		return errors.New("commands: read, analyze, run, digest, status, export, retry, retry-delivery")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logCfg := zap.NewProductionConfig()
	if err := logCfg.Level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	log, err := logCfg.Build()
	if err != nil {
		return err
	}
	defer log.Sync()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := reader.New(reader.Options{Timeout: cfg.Reader.Timeout, MaxBytes: cfg.Reader.MaxBytes, MaxContentChars: cfg.Reader.MaxContentChars, MaxLinks: cfg.Reader.MaxLinks})
	if args[0] == "read" {
		if len(args) != 2 {
			return errors.New("usage: kagari read URL")
		}
		source, readErr := r.Read(ctx, args[1])
		if err := writeJSON(source); err != nil {
			return err
		}
		return readErr
	}
	processor := args[0] == "analyze" || args[0] == "run" || args[0] == "digest"
	// 先取得处理器锁再恢复任务，避免 CLI 将运行中服务的 processing/sending 状态改写。
	if processor {
		unlock, err := app.Lock(cfg.Storage.Path)
		if err != nil {
			return err
		}
		defer unlock()
	}
	s, err := store.New(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	if processor {
		if err := s.Recover(ctx); err != nil {
			return err
		}
	}
	w := &app.Worker{Store: s, Config: cfg, Log: log}
	if args[0] == "analyze" || args[0] == "run" {
		e, err := agent.New(ctx, cfg, r.Read, s.CachedSource)
		if err != nil {
			return err
		}
		w.Engine = e
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			return errors.New("usage: kagari run")
		}
		return app.Run(ctx, w)
	case "analyze":
		flags := flag.NewFlagSet("analyze", flag.ContinueOnError)
		note := flags.String("note", "", "reading question or preference")
		user := flags.Int64("user", 0, "archive owner user ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if len(flags.Args()) == 0 {
			return errors.New("usage: kagari analyze [-note TEXT] [-user ID] URL...")
		}
		sub, err := w.Engine.Prepare(domain.Submission{UserID: *user, URLs: flags.Args(), Note: *note})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(sub)
		id, _, err := s.Enqueue(ctx, "analyze", "cli:"+sub.CacheKey+":"+strconv.FormatInt(sub.ReceivedAt.UnixNano(), 10), raw, 0)
		if err != nil {
			return err
		}
		job, err := s.StartJob(ctx, id)
		if err != nil {
			return err
		}
		if err := w.Process(ctx, job); err != nil {
			return fmt.Errorf("task #%d: %w", id, err)
		}
		job, err = s.Job(ctx, id)
		if err != nil {
			return err
		}
		var result domain.Result
		if err := json.Unmarshal(job.Result, &result); err != nil {
			return err
		}
		fmt.Printf("任务 #%d\n%s\n", id, render.Analysis(result))
		return nil
	case "digest":
		flags := flag.NewFlagSet("digest", flag.ContinueOnError)
		startFlag := flags.String("start", "", "inclusive receipt date YYYY-MM-DD")
		endFlag := flags.String("end", "", "exclusive receipt date YYYY-MM-DD")
		user := flags.Int64("user", 0, "archive owner user ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if len(flags.Args()) != 0 {
			return errors.New("unexpected digest arguments")
		}
		loc, _ := time.LoadLocation(cfg.Weekly.Timezone)
		start, end := digest.PreviousWeek(time.Now(), loc)
		if *startFlag != "" || *endFlag != "" {
			start, err = time.ParseInLocation("2006-01-02", *startFlag, loc)
			if err != nil {
				return err
			}
			end, err = time.ParseInLocation("2006-01-02", *endFlag, loc)
			if err != nil {
				return err
			}
		}
		id, created, err := w.EnqueueDigest(ctx, domain.DigestRequest{UserID: *user, Start: start, End: end}, 0)
		if err != nil {
			return err
		}
		if created {
			job, err := s.StartJob(ctx, id)
			if err != nil {
				return err
			}
			if err := w.Process(ctx, job); err != nil {
				return err
			}
		}
		job, err := s.Job(ctx, id)
		if err != nil {
			return err
		}
		if job.Status != "completed" {
			return fmt.Errorf("digest #%d is %s; start the service or retry the failed task", id, job.Status)
		}
		var report digest.Report
		if err := json.Unmarshal(job.Result, &report); err != nil {
			return err
		}
		fmt.Print(digest.Render(report, cfg.Weekly.Timezone))
		return nil
	case "status", "export", "retry", "retry-delivery":
		if len(args) != 2 {
			return fmt.Errorf("usage: kagari %s TASK_ID", args[0])
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("task ID must be a positive integer")
		}
		switch args[0] {
		case "status":
			text, err := w.Status(ctx, id)
			if err != nil {
				return err
			}
			fmt.Println(text)
			return nil
		case "export":
			job, err := s.Job(ctx, id)
			if err != nil {
				return err
			}
			if job == nil {
				return errors.New("task not found")
			}
			return writeJSON(job)
		case "retry":
			return s.RetryJob(ctx, id)
		case "retry-delivery":
			return s.RetryDeliveries(ctx, id)
		}
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func writeJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
