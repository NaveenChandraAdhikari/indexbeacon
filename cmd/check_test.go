package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/indexbeacon/indexbeacon/internal/db"
)

func TestDoctor(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-5 * time.Minute)

	base := db.Info{
		Role:                   db.RolePrimary,
		ServerVersionNum:       160004,
		ServerVersion:          "PostgreSQL 16.4 (Debian 16.4-1.pgdg120+1) on x86_64",
		User:                   "idxa",
		Database:               "app",
		ReadAllStats:           true,
		SharedPreloadLibraries: "pg_stat_statements",
		Extensions: []db.Extension{
			{Name: "pg_stat_statements", Installed: true},
			{Name: "hypopg", Installed: true},
		},
		TrackActivityQuerySize: 4096,
		StatsReset:             &old,
		ServerNow:              now,
	}

	levels := func(fs []checkFinding) (ok, warn, fail int) {
		for _, f := range fs {
			switch f.Level {
			case "ok":
				ok++
			case "warn":
				warn++
			case "fail":
				fail++
			}
		}
		return
	}

	t.Run("healthy", func(t *testing.T) {
		_, warn, fail := levels(doctor(&base, time.Hour))
		if warn != 0 || fail != 0 {
			t.Fatalf("expected clean report, got warn=%d fail=%d: %+v", warn, fail, doctor(&base, time.Hour))
		}
	})

	t.Run("old server fails", func(t *testing.T) {
		i := base
		i.ServerVersionNum = 110000
		_, _, fail := levels(doctor(&i, time.Hour))
		if fail != 1 {
			t.Fatalf("expected 1 failure, got %d", fail)
		}
	})

	t.Run("missing extensions warn", func(t *testing.T) {
		i := base
		i.Extensions = nil
		fs := doctor(&i, time.Hour)
		_, warn, fail := levels(fs)
		if fail != 0 || warn != 2 {
			t.Fatalf("expected 2 warnings 0 failures, got warn=%d fail=%d", warn, fail)
		}
		joined := ""
		for _, f := range fs {
			joined += f.Message + "\n"
		}
		if !strings.Contains(joined, "pg_stat_statements not installed") || !strings.Contains(joined, "hypopg not installed") {
			t.Fatalf("unexpected messages:\n%s", joined)
		}
	})

	t.Run("pss installed but not preloaded warns", func(t *testing.T) {
		i := base
		i.SharedPreloadLibraries = ""
		_, warn, _ := levels(doctor(&i, time.Hour))
		if warn != 1 {
			t.Fatalf("expected 1 warning, got %d", warn)
		}
	})

	t.Run("fresh stats warn", func(t *testing.T) {
		i := base
		i.StatsReset = &fresh
		_, warn, _ := levels(doctor(&i, time.Hour))
		if warn != 1 {
			t.Fatalf("expected 1 warning, got %d", warn)
		}
	})

	t.Run("no read_all_stats and small query size warn", func(t *testing.T) {
		i := base
		i.ReadAllStats = false
		i.TrackActivityQuerySize = 1024
		_, warn, _ := levels(doctor(&i, time.Hour))
		if warn != 2 {
			t.Fatalf("expected 2 warnings, got %d", warn)
		}
	})
}

func TestSplitDSNList(t *testing.T) {
	got := splitDSNList("a,b\n# comment\n\n c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
