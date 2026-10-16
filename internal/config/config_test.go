package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func sampleCluster() *Cluster {
	return &Cluster{
		Name:  "Production",
		Nodes: []Node{{Name: "primary", DSN: "postgres://u:p@localhost:5432/app"}},
	}
}

func TestAddAndReadBack(t *testing.T) {
	s := newStore(t)
	added, err := s.Add(sampleCluster())
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID != "production" {
		t.Errorf("id = %q, want a readable slug", added.ID)
	}
	if added.Schedule.Mode != ScheduleManual {
		t.Errorf("schedule = %q, want manual by default: automatic queries against production must be opt-in",
			added.Schedule.Mode)
	}

	// Re-open from disk to prove it persisted.
	reopened, err := Open(s.Path())
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	got, ok := reopened.Cluster("production")
	if !ok {
		t.Fatal("cluster did not survive a round trip")
	}
	if got.Name != "Production" || len(got.Nodes) != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestFilePermissionsAreOwnerOnly(t *testing.T) {
	// The file holds connection strings, so other users on the machine must
	// not be able to read it.
	s := newStore(t)
	if _, err := s.Add(sampleCluster()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions = %o, want 600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("config directory permissions = %o, want 700", perm)
	}
}

func TestNoTemporaryFilesLeftBehind(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		c := sampleCluster()
		c.Name = "db" + string(rune('a'+i))
		if _, err := s.Add(c); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestIdentifiersDoNotCollide(t *testing.T) {
	s := newStore(t)
	first, _ := s.Add(&Cluster{Name: "Prod", Nodes: []Node{{DSN: "postgres://h/d"}}})
	second, err := s.Add(&Cluster{Name: "Prod", Nodes: []Node{{DSN: "postgres://h/d"}}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("both clusters got id %q", first.ID)
	}
}

func TestUpdateKeepsIdentityAndCreationTime(t *testing.T) {
	s := newStore(t)
	added, _ := s.Add(sampleCluster())
	created := added.CreatedAt

	time.Sleep(2 * time.Millisecond)
	updated, err := s.Update(added.ID, &Cluster{
		Name:  "Renamed",
		Nodes: []Node{{DSN: "postgres://h/d"}},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ID != added.ID {
		t.Errorf("id changed on update: %q -> %q", added.ID, updated.ID)
	}
	if !updated.CreatedAt.Equal(created) {
		t.Error("creation time was overwritten on update")
	}
	if !updated.UpdatedAt.After(created) {
		t.Error("update time was not advanced")
	}
}

func TestUpdateAndDeleteRejectUnknownIDs(t *testing.T) {
	s := newStore(t)
	if _, err := s.Update("missing", sampleCluster()); err == nil {
		t.Error("expected an error updating a cluster that does not exist")
	}
	if err := s.Delete("missing"); err == nil {
		t.Error("expected an error deleting a cluster that does not exist")
	}
}

func TestDeleteRemovesOnlyTheTarget(t *testing.T) {
	s := newStore(t)
	a, _ := s.Add(&Cluster{Name: "alpha", Nodes: []Node{{DSN: "postgres://h/a"}}})
	b, _ := s.Add(&Cluster{Name: "beta", Nodes: []Node{{DSN: "postgres://h/b"}}})

	if err := s.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Cluster(a.ID); ok {
		t.Error("deleted cluster is still present")
	}
	if _, ok := s.Cluster(b.ID); !ok {
		t.Error("deleting one cluster removed another")
	}
}

func TestClustersAreReturnedInStableOrder(t *testing.T) {
	// The sidebar must not reshuffle between requests.
	s := newStore(t)
	for _, name := range []string{"zulu", "alpha", "Mike"} {
		if _, err := s.Add(&Cluster{Name: name, Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	want := []string{"alpha", "Mike", "zulu"}
	for i := 0; i < 3; i++ {
		got := s.Clusters()
		for j, c := range got {
			if c.Name != want[j] {
				t.Fatalf("order = %v, want %v", names(got), want)
			}
		}
	}
}

func TestReturnedClustersAreCopies(t *testing.T) {
	// A caller mutating what it reads must not corrupt the store.
	s := newStore(t)
	added, _ := s.Add(sampleCluster())

	got, _ := s.Cluster(added.ID)
	got.Name = "mutated"
	got.Nodes[0].DSN = "mutated"

	fresh, _ := s.Cluster(added.ID)
	if fresh.Name == "mutated" || fresh.Nodes[0].DSN == "mutated" {
		t.Error("the store handed out a reference to its own state")
	}
}

func TestValidationRejectsBadClusters(t *testing.T) {
	cases := map[string]*Cluster{
		"no name":           {Nodes: []Node{{DSN: "postgres://h/d"}}},
		"no nodes":          {Name: "x"},
		"empty dsn":         {Name: "x", Nodes: []Node{{DSN: "   "}}},
		"short interval":    {Name: "x", Nodes: []Node{{DSN: "d"}}, Schedule: Schedule{Mode: ScheduleInterval, Interval: "10s"}},
		"cron without expr": {Name: "x", Nodes: []Node{{DSN: "d"}}, Schedule: Schedule{Mode: ScheduleCron}},
		"unknown timezone":  {Name: "x", Nodes: []Node{{DSN: "d"}}, Schedule: Schedule{Mode: ScheduleCron, Cron: "0 3 * * *", Timezone: "Mars/Olympus"}},
		"unknown mode":      {Name: "x", Nodes: []Node{{DSN: "d"}}, Schedule: Schedule{Mode: "hourly-ish"}},
	}
	for name, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation to fail", name)
		}
	}

	valid := &Cluster{
		Name:     "x",
		Nodes:    []Node{{DSN: "postgres://h/d"}},
		Schedule: Schedule{Mode: ScheduleCron, Cron: "0 3 * * *", Timezone: "Asia/Kolkata"},
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid cluster rejected: %v", err)
	}
}

func TestPrimaryFallsBackToFirstNode(t *testing.T) {
	c := &Cluster{Nodes: []Node{
		{Name: "a", Role: RoleReplica},
		{Name: "b", Role: RolePrimary},
	}}
	if got := c.Primary(); got.Name != "b" {
		t.Errorf("primary = %v, want the node marked primary", got)
	}

	unknown := &Cluster{Nodes: []Node{{Name: "a"}, {Name: "b"}}}
	if got := unknown.Primary(); got.Name != "a" {
		t.Errorf("primary = %v, want the first node when none is identified", got)
	}

	if (&Cluster{}).Primary() != nil {
		t.Error("a cluster with no nodes should have no primary")
	}
}

func TestResolveExpandsEnvironmentReferences(t *testing.T) {
	t.Setenv("IDXA_TEST_PASSWORD", "s3cret")
	got, err := Resolve("postgres://user:${IDXA_TEST_PASSWORD}@host/db")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "postgres://user:s3cret@host/db" {
		t.Errorf("resolved = %q", got)
	}
}

func TestResolveFailsLoudlyOnMissingVariable(t *testing.T) {
	// Silently substituting an empty password produces a confusing
	// authentication failure much later, so this must fail immediately.
	_, err := Resolve("postgres://user:${IDXA_DEFINITELY_UNSET}@host/db")
	if err == nil {
		t.Fatal("expected an error for an unset variable")
	}
	if !strings.Contains(err.Error(), "IDXA_DEFINITELY_UNSET") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestSettingsRoundTripAndClamp(t *testing.T) {
	s := newStore(t)
	if got := s.Settings().RefreshSeconds; got != 5 {
		t.Errorf("default refresh = %d, want 5", got)
	}

	if err := s.SetSettings(Settings{RefreshSeconds: 0, DutyCyclePercent: 0}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	got := s.Settings()
	if got.RefreshSeconds < 1 {
		t.Errorf("refresh = %d, want a clamp to at least 1", got.RefreshSeconds)
	}
	if got.DutyCyclePercent <= 0 {
		t.Errorf("duty cycle = %v, want a sane default", got.DutyCyclePercent)
	}
}

func TestOpenHandlesMissingAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()

	if _, err := Open(filepath.Join(dir, "nope.json")); err != nil {
		t.Errorf("opening a missing config should succeed: %v", err)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(empty); err != nil {
		t.Errorf("opening an empty config should succeed: %v", err)
	}

	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(broken); err == nil {
		t.Error("expected an error for a corrupt config")
	}
}

func TestDurationParsing(t *testing.T) {
	if got := Duration("6h").Duration(); got != 6*time.Hour {
		t.Errorf("6h = %v", got)
	}
	if got := Duration("").Duration(); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	if got := Duration("nonsense").Duration(); got != 0 {
		t.Errorf("invalid = %v, want 0", got)
	}
}

func names(cs []*Cluster) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}
