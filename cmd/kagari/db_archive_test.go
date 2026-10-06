package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kagari/internal/app"
	"kagari/internal/domain"
	"kagari/internal/store"
)

func TestDBArchiveReadAndDelete(t *testing.T) {
	path := seedDebugDB(t)
	ctx := context.Background()
	var out bytes.Buffer
	unlock, err := app.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	if err := runDB(ctx, path, []string{"archive", "list", "-user", "1", "-limit", "1"}, &out); err != nil {
		t.Fatalf("list while processor holds lock: %v", err)
	}
	var listing dbArchiveListOutput
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil || listing.UserID != 1 || listing.Limit != 1 || listing.Offset != 0 || len(listing.Entries) != 1 || listing.Entries[0].JobID != 1 {
		t.Fatalf("list = %s, %v", out.String(), err)
	}
	out.Reset()
	if err := runDB(ctx, path, []string{"archive", "show", "-user", "1", "1"}, &out); err != nil {
		t.Fatalf("show while processor holds lock: %v", err)
	}
	var shown struct {
		UserID int64               `json:"user_id"`
		Entry  domain.ArchiveEntry `json:"entry"`
	}
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil || shown.UserID != 1 || shown.Entry.JobID != 1 || shown.Entry.Submission.UserID != 1 {
		t.Fatalf("show = %s, %v", out.String(), err)
	}
	out.Reset()
	if err := runDB(ctx, path, []string{"archive", "delete", "-user", "1", "-yes", "1"}, &out); err == nil || !strings.Contains(err.Error(), "lock") || out.Len() != 0 {
		t.Fatalf("delete with processor lock = %v, output %s", err, out.String())
	}
	unlock()
	for _, args := range [][]string{
		{"archive", "delete", "-user", "1", "1"},         // confirmation required
		{"archive", "delete", "-user", "2", "-yes", "1"}, // wrong owner
		{"archive", "delete", "-user", "2", "-yes", "2"}, // pending
		{"archive", "show", "-user", "2", "1"},
	} {
		if err := runDB(ctx, path, args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("runDB(%v) = %v, output %s", args, err, out.String())
		}
	}
	if err := runDB(ctx, path, []string{"archive", "delete", "-user", "1", "-yes", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	var deleted struct {
		UserID  int64            `json:"user_id"`
		JobID   int64            `json:"job_id"`
		Deleted map[string]int64 `json:"deleted"`
	}
	if err := json.Unmarshal(out.Bytes(), &deleted); err != nil || deleted.UserID != 1 || deleted.JobID != 1 || deleted.Deleted["jobs"] != 1 || deleted.Deleted["deliveries"] != 1 {
		t.Fatalf("delete = %s, %v", out.String(), err)
	}
	db, err := store.OpenExisting(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info, err := db.DatabaseInfo(ctx)
	if err != nil || info.Tables[0].Rows != 1 || info.Tables[1].Rows != 0 || info.Tables[2].Rows != 1 || info.NextTelegramOffset != 42 {
		t.Fatalf("after delete = %+v, %v", info, err)
	}
	out.Reset()
	if err := runDB(ctx, path, []string{"archive", "list", "-user", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"entries": []`) {
		t.Fatalf("empty archive output = %s", out.String())
	}
}

func TestDBArchiveArgsAndHelpNeverCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{}, "usage"},
		{[]string{"unknown"}, "unknown"},
		{[]string{"list"}, "-user"},
		{[]string{"list", "-user", "-2"}, "nonnegative"},
		{[]string{"list", "-user", "1", "-limit", "0"}, "limit"},
		{[]string{"list", "-user", "1", "-limit", "101"}, "limit"},
		{[]string{"list", "-user", "1", "-offset", "-1"}, "offset"},
		{[]string{"list", "-user", "1", "extra"}, "usage"},
		{[]string{"show", "-user", "1"}, "usage"},
		{[]string{"show", "-user", "1", "0"}, "positive"},
		{[]string{"show", "-user", "1", "oops"}, "positive"},
		{[]string{"show", "1", "-user", "1"}, "-user"},
		{[]string{"delete", "-user", "1", "1"}, "-yes"},
		{[]string{"delete", "-user", "1", "-yes"}, "usage"},
		{[]string{"delete", "-yes", "1"}, "-user"},
		{[]string{"delete", "-user", "1", "-yes", "1"}, "must already exist"},
		{[]string{"list", "-user", "0"}, "stat database"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		err := runDB(context.Background(), path, append([]string{"archive"}, tc.args...), &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("archive %v = %v, output %s; want %q", tc.args, err, out.String(), tc.want)
		}
	}
	for _, args := range [][]string{{"-h"}, {"list", "-h"}, {"show", "-h"}, {"delete", "-yes", "-h"}} {
		var out bytes.Buffer
		if err := runDB(context.Background(), path, append([]string{"archive"}, args...), &out); err != nil || !strings.Contains(out.String(), "usage: kagari db archive") {
			t.Fatalf("help %v = %v, output %s", args, err, out.String())
		}
	}
	for _, file := range []string{path, path + ".lock"} {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("argument validation/help created %s: %v", file, err)
		}
	}
}
