package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"kagari/internal/app"
	"kagari/internal/domain"
	"kagari/internal/store"
)

type dbArchiveListOutput struct {
	Path    string                `json:"path"`
	UserID  int64                 `json:"user_id"`
	Limit   int                   `json:"limit"`
	Offset  int                   `json:"offset"`
	Entries []domain.ArchiveEntry `json:"entries"`
}

func runDBArchive(ctx context.Context, path string, args []string, out io.Writer) error {
	if len(args) == 1 && isDBHelp(args[0]) {
		return writeDBHelp(out, "archive")
	}
	if len(args) == 0 {
		return errors.New("usage: kagari db archive {list|show|delete}")
	}
	action := args[0]
	if action != "list" && action != "show" && action != "delete" {
		return fmt.Errorf("unknown archive command %q; usage: kagari db archive {list|show|delete}", action)
	}
	flags := flag.NewFlagSet("db archive "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() { _ = writeDBHelp(out, "archive "+action) }
	user := flags.Int64("user", -1, "archive owner user ID (0 for CLI submissions without -user)")
	limit, offset, yes := 20, 0, false
	if action == "list" {
		flags.IntVar(&limit, "limit", 20, "maximum entries (1..100)")
		flags.IntVar(&offset, "offset", 0, "number of entries to skip")
	}
	if action == "delete" {
		flags.BoolVar(&yes, "yes", false, "confirm irreversible deletion")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *user < 0 {
		return errors.New("-user is required and must be nonnegative")
	}
	var id int64
	if action == "list" {
		if flags.NArg() != 0 {
			return errors.New("usage: kagari db archive list -user ID [-limit 20] [-offset 0]")
		}
		if limit < 1 || limit > 100 || offset < 0 {
			return errors.New("limit must be 1..100 and offset nonnegative")
		}
	} else {
		if flags.NArg() != 1 {
			if action == "delete" {
				return errors.New("usage: kagari db archive delete -user ID -yes TASK_ID")
			}
			return errors.New("usage: kagari db archive show -user ID TASK_ID")
		}
		var err error
		id, err = strconv.ParseInt(flags.Arg(0), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("task ID must be a positive integer")
		}
		if action == "delete" && !yes {
			return errors.New("deletion is irreversible; inspect with `kagari db archive show -user ID TASK_ID` first, then pass -yes")
		}
	}
	absolute, err := absoluteDBPath(path)
	if err != nil {
		return err
	}
	if action == "delete" {
		file, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("database file must already exist: %w", err)
		}
		if !file.Mode().IsRegular() {
			return errors.New("database path must be an existing regular file")
		}
		unlock, err := app.Lock(path)
		if err != nil {
			return err
		}
		defer unlock()
	}
	db, err := store.OpenExisting(ctx, path, action == "delete")
	if err != nil {
		return err
	}
	defer db.Close()
	switch action {
	case "list":
		entries, err := db.ListArchive(ctx, *user, limit, offset)
		if err != nil {
			return err
		}
		return encodeDBJSON(out, dbArchiveListOutput{Path: absolute, UserID: *user, Limit: limit, Offset: offset, Entries: entries})
	case "show":
		entry, err := db.ArchiveEntry(ctx, *user, id)
		if err != nil {
			return err
		}
		if entry == nil {
			return errors.New("archive entry not found or does not belong to this user")
		}
		return encodeDBJSON(out, map[string]any{"path": absolute, "user_id": *user, "entry": entry})
	default: // delete
		deleted, err := db.DeleteArchiveEntry(ctx, *user, id)
		if err != nil {
			return err
		}
		return encodeDBJSON(out, map[string]any{"path": absolute, "user_id": *user, "job_id": id, "deleted": deleted})
	}
}
