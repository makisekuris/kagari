package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"kagari/internal/config"
	"kagari/internal/reader"

	"go.uber.org/zap"
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
		return errors.New("commands: run, weixin, channel, attachments, events, trace, tasks, runs, memories, memory-history, schedules, schedule-history")
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
	switch args[0] {
	case "weixin", "channel", "attachments":
		return runChannelCommand(ctx, cfg, args, os.Stdin, os.Stdout)
	case "events":
		return inspectEvents(ctx, cfg, args[1:], os.Stdout)
	case "trace":
		return inspectTrace(ctx, cfg, args[1:], os.Stdout)
	case "schedules":
		return inspectSchedules(ctx, cfg, args[1:], os.Stdout)
	case "schedule-history":
		return inspectScheduleHistory(ctx, cfg, args[1:], os.Stdout)
	case "tasks", "runs", "memories", "memory-history":
		return inspectPersonal(ctx, cfg, args, os.Stdout)
	case "run":
		if len(args) != 1 {
			return errors.New("usage: kagari run")
		}
		r := reader.New(reader.Options{Timeout: cfg.Reader.Timeout, MaxBytes: cfg.Reader.MaxBytes, MaxContentChars: cfg.Reader.MaxContentChars, MaxLinks: cfg.Reader.MaxLinks, AllowedNonPublicCIDRs: cfg.Reader.AllowedNonPublicCIDRs})
		return runPersonal(ctx, cfg, r.Read, log)
	}
	return fmt.Errorf("unknown command %q", args[0])
}
