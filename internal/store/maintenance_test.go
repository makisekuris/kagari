package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func maintenanceFixture(t *testing.T) (string, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maintenance.sqlite")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return path, s
}

func seedMaintenanceRows(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO jobs(kind,key,payload,result,targets,status,next_attempt_at,created_at)
		VALUES('analyze','one',? ,NULL,'[]','completed','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, []byte(`{"cache_key":"one","n":9007199254740993}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(job_id,channel,address,part,text,status,next_attempt_at) VALUES(1,'telegram','7',1,'part','sent','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO source_cache(url,source,cached_at) VALUES
		('https://a.example', ?, '2026-01-01T00:00:00Z'),
		('https://b.example', ?, '2026-01-02T00:00:00Z')`, []byte(`{"ok":true}`), []byte(`not-json`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('z','last'),('a','first')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE update_state SET next_offset=42 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
}

func TestOpenExistingAndInspection(t *testing.T) {
	path, source := maintenanceFixture(t)
	seedMaintenanceRows(t, source)
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := OpenExisting(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	info, err := s.DatabaseInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantTables := []TableInfo{{Name: "jobs", Rows: 1}, {Name: "deliveries", Rows: 1}, {Name: "source_cache", Rows: 2}, {Name: "meta", Rows: 2}, {Name: "update_state", Rows: 1}}
	if !reflect.DeepEqual(info.Tables, wantTables) || !reflect.DeepEqual(info.JobStatuses, map[string]int64{"completed": 1}) || !reflect.DeepEqual(info.DeliveryStatuses, map[string]int64{"sent": 1}) || info.NextTelegramOffset != 42 {
		t.Fatalf("DatabaseInfo() = %+v", info)
	}
	first, err := s.TableRows(ctx, "jobs", 1, 0)
	if err != nil || len(first) != 1 || first[0]["key"] != "one" || first[0]["result"] != nil {
		t.Fatalf("TableRows(jobs, first page) = (%v, %v)", first, err)
	}
	payload, ok := first[0]["payload"].(json.RawMessage)
	if !ok || !strings.Contains(string(payload), "9007199254740993") {
		t.Fatalf("JSON BLOB lost its exact number: %#v", first[0]["payload"])
	}
	encoded, err := json.Marshal(first[0])
	if err != nil || !strings.Contains(string(encoded), `"payload":{"cache_key":"one","n":9007199254740993}`) {
		t.Fatalf("JSON BLOB was not emitted as an object: %s (%v)", encoded, err)
	}
	cache, err := s.TableRows(ctx, "source_cache", 1, 0)
	if err != nil || len(cache) != 1 || cache[0]["url"] != "https://b.example" || cache[0]["source"] != "not-json" {
		t.Fatalf("TableRows(source_cache) order/BLOB = (%v, %v)", cache, err)
	}
	meta, err := s.TableRows(ctx, "meta", 10, 0)
	if err != nil || len(meta) != 2 || meta[0]["key"] != "a" || meta[1]["key"] != "z" {
		t.Fatalf("TableRows(meta) order = (%v, %v)", meta, err)
	}
	empty, err := s.TableRows(ctx, "jobs", 10, 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty page = (%v, %v), want empty array", empty, err)
	}
	for _, tc := range []struct {
		table  string
		limit  int
		offset int
	}{{"jobs; DROP TABLE jobs", 10, 0}, {"jobs", 0, 0}, {"jobs", 101, 0}, {"jobs", 10, -1}} {
		if _, err := s.TableRows(ctx, tc.table, tc.limit, tc.offset); err == nil {
			t.Fatalf("TableRows(%q,%d,%d) unexpectedly succeeded", tc.table, tc.limit, tc.offset)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE meta SET value='changed' WHERE key='a'`); err == nil {
		t.Fatal("read-only database accepted a write")
	}
	if deleted, err := s.Clear(ctx, "cache"); err == nil || deleted != nil {
		t.Fatalf("read-only Clear() = (%v, %v), want error without report", deleted, err)
	}
}

func TestOpenExistingRejectsCreationAndUnsupportedPaths(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "does-not-exist.sqlite")
	if _, err := OpenExisting(ctx, missing, false); err == nil {
		t.Fatal("OpenExisting() accepted a missing path")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: stat error %v", err)
	}
	for _, path := range []string{"", "   ", ":memory:", "file:/tmp/kagari.sqlite"} {
		if _, err := OpenExisting(ctx, path, false); err == nil {
			t.Fatalf("OpenExisting(%q) unexpectedly succeeded", path)
		}
	}
	if _, err := OpenExisting(ctx, t.TempDir(), false); err == nil {
		t.Fatal("OpenExisting() accepted a directory")
	}
}

func TestDatabaseInfoSnapshotOnMemoryStore(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := s.DatabaseInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Tables) != len(maintenanceTables) || info.NextTelegramOffset != 0 || len(info.JobStatuses) != 0 || len(info.DeliveryStatuses) != 0 {
		t.Fatalf("empty-store snapshot = %+v", info)
	}
}

func TestClearScopesForeignKeysAndReuse(t *testing.T) {
	ctx := context.Background()
	for _, scope := range []string{"cache", "data", "all"} {
		t.Run(scope, func(t *testing.T) {
			path, source := maintenanceFixture(t)
			seedMaintenanceRows(t, source)
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := OpenExisting(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var foreignKeys, busyTimeout int
			if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
				t.Fatal(err)
			}
			if foreignKeys != 1 || busyTimeout != 5000 {
				t.Fatalf("open pragmas foreign_keys=%d busy_timeout=%d", foreignKeys, busyTimeout)
			}

			deleted, err := s.Clear(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int64{}
			switch scope {
			case "cache":
				want["source_cache"] = 2
			case "data":
				want["deliveries"], want["jobs"], want["source_cache"] = 1, 1, 2
			case "all":
				want["deliveries"], want["jobs"], want["source_cache"], want["meta"] = 1, 1, 2, 2
			}
			if !reflect.DeepEqual(deleted, want) {
				t.Fatalf("Clear(%s) deleted %v, want %v", scope, deleted, want)
			}
			info, err := s.DatabaseInfo(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantOffset := int64(42)
			if scope == "all" {
				wantOffset = 0
			}
			if info.NextTelegramOffset != wantOffset {
				t.Fatalf("Clear(%s) offset = %d, want %d", scope, info.NextTelegramOffset, wantOffset)
			}
			if scope == "cache" && (info.Tables[0].Rows != 1 || info.Tables[1].Rows != 1 || info.Tables[3].Rows != 2) {
				t.Fatalf("cache clear removed durable data: %+v", info.Tables)
			}
			if scope == "data" && (info.Tables[0].Rows != 0 || info.Tables[1].Rows != 0 || info.Tables[3].Rows != 2) {
				t.Fatalf("data clear scope mismatch: %+v", info.Tables)
			}
			if scope == "all" && (info.Tables[0].Rows != 0 || info.Tables[1].Rows != 0 || info.Tables[2].Rows != 0 || info.Tables[3].Rows != 0 || info.Tables[4].Rows != 1) {
				t.Fatalf("all clear scope mismatch: %+v", info.Tables)
			}
			if scope != "cache" {
				if _, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(job_id,channel,address,part,text,status,next_attempt_at) VALUES(999,'telegram','7',1,'orphan','pending','2026-01-01T00:00:00Z')`); err == nil {
					t.Fatal("foreign-key check allowed orphan delivery")
				}
			}
			if _, _, err := s.Enqueue(ctx, "notice", "after-clear", []byte(`{}`), nil); err != nil {
				t.Fatalf("store not reusable after %s clear: %v", scope, err)
			}
		})
	}
}

func TestClearRollsBackWhenLaterDeleteFails(t *testing.T) {
	ctx := context.Background()
	path, source := maintenanceFixture(t)
	seedMaintenanceRows(t, source)
	if _, err := source.db.ExecContext(ctx, `CREATE TRIGGER block_job_delete BEFORE DELETE ON jobs BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenExisting(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if deleted, err := s.Clear(ctx, "data"); err == nil || deleted != nil {
		t.Fatalf("Clear(data) = (%v, %v), want atomic failure", deleted, err)
	}
	info, err := s.DatabaseInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tables[0].Rows != 1 || info.Tables[1].Rows != 1 || info.Tables[2].Rows != 2 {
		t.Fatalf("failed clear partially committed: %+v", info.Tables)
	}
}
