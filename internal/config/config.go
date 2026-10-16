// Package config stores the databases the web interface knows about.
//
// The file holds connection strings, which may contain passwords, so it is
// created with owner-only permissions and every rendering of a connection
// string for display, logging or an error message goes through Redact.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Role records what a node turned out to be when it was last reached.
type Role string

const (
	RolePrimary Role = "primary"
	RoleReplica Role = "replica"
	RoleUnknown Role = "unknown"
)

// Node is one PostgreSQL instance.
type Node struct {
	// Name is a label shown in the interface.
	Name string `json:"name"`
	// DSN is the connection string. It may contain ${VAR} references, which
	// are resolved from the environment when connecting, so a password need
	// never be written to disk.
	DSN string `json:"dsn"`
	// Role is filled in when the node is reached.
	Role Role `json:"role,omitempty"`
}

// ScheduleMode selects how often analysis runs by itself.
type ScheduleMode string

const (
	// ScheduleManual means analysis only runs when asked. This is the
	// default: unattended queries against someone's production database
	// should be a deliberate choice.
	ScheduleManual ScheduleMode = "manual"
	// ScheduleInterval runs every Interval.
	ScheduleInterval ScheduleMode = "interval"
	// ScheduleCron runs on a cron expression in Timezone.
	ScheduleCron ScheduleMode = "cron"
)

// Schedule describes automatic analysis for one cluster.
type Schedule struct {
	Mode ScheduleMode `json:"mode"`
	// Interval is used when Mode is interval.
	Interval Duration `json:"interval,omitempty"`
	// Cron is a standard five field expression, used when Mode is cron.
	Cron string `json:"cron,omitempty"`
	// Timezone is an IANA name. Empty means the server's local zone.
	Timezone string `json:"timezone,omitempty"`
}

// Cluster is one database, which may span a primary and its replicas.
//
// Grouping replicas matters: statistics counters are per instance, so an
// index used only on one replica looks unused when the primary is inspected
// alone, and a table scanned on three replicas is under three times the
// pressure a single node reports.
type Cluster struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Nodes       []Node    `json:"nodes"`
	Schedule    Schedule  `json:"schedule"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Primary returns the node recorded as the primary, falling back to the first
// node when none has been identified yet.
func (c *Cluster) Primary() *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Role == RolePrimary {
			return &c.Nodes[i]
		}
	}
	if len(c.Nodes) > 0 {
		return &c.Nodes[0]
	}
	return nil
}

// Validate reports why a cluster cannot be saved.
func (c *Cluster) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("a name is required")
	}
	if len(c.Nodes) == 0 {
		return errors.New("at least one connection string is required")
	}
	for i, n := range c.Nodes {
		if strings.TrimSpace(n.DSN) == "" {
			return fmt.Errorf("connection string %d is empty", i+1)
		}
	}
	switch c.Schedule.Mode {
	case "", ScheduleManual:
	case ScheduleInterval:
		if c.Schedule.Interval.Duration() < time.Minute {
			return errors.New("the interval must be at least one minute")
		}
	case ScheduleCron:
		if strings.TrimSpace(c.Schedule.Cron) == "" {
			return errors.New("a cron expression is required")
		}
		if c.Schedule.Timezone != "" {
			if _, err := time.LoadLocation(c.Schedule.Timezone); err != nil {
				return fmt.Errorf("unknown timezone %q", c.Schedule.Timezone)
			}
		}
	default:
		return fmt.Errorf("unknown schedule mode %q", c.Schedule.Mode)
	}
	return nil
}

// Settings holds anything that is not per cluster.
type Settings struct {
	// RefreshSeconds is how often the live view asks for new numbers. The
	// load governor may stretch this further on a slow server.
	RefreshSeconds int `json:"refresh_seconds"`
	// DutyCyclePercent is the share of wall-clock time idxa may spend
	// running statements against any one node.
	DutyCyclePercent float64 `json:"duty_cycle_percent"`
}

func defaultSettings() Settings {
	return Settings{RefreshSeconds: 5, DutyCyclePercent: 1}
}

// File is the whole configuration document.
type File struct {
	Version  int        `json:"version"`
	Settings Settings   `json:"settings"`
	Clusters []*Cluster `json:"clusters"`
}

// Store reads and writes the configuration file, safe for concurrent use.
type Store struct {
	path string

	mu   sync.RWMutex
	file File
}

// DefaultPath returns the configuration location, honouring XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "idxa", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "idxa", "config.json"), nil
}

// Open loads the store at path, creating an empty one if it does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path, file: File{Version: 1, Settings: defaultSettings()}}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}

	// An encrypted file needs the key before it can be parsed. Detecting the
	// envelope rather than requiring a flag means encryption can be turned on
	// for an existing installation without a migration.
	if isEncrypted(data) {
		key := encryptionKey()
		if key == nil {
			return nil, ErrKeyRequired
		}
		plain, err := decrypt(data, key)
		if err != nil {
			return nil, err
		}
		data = plain
	}

	if err := json.Unmarshal(data, &s.file); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if s.file.Settings.RefreshSeconds <= 0 {
		s.file.Settings = defaultSettings()
	}
	return s, nil
}

// Path returns the file location.
func (s *Store) Path() string { return s.path }

// Settings returns a copy of the global settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.Settings
}

// SetSettings replaces the global settings and saves.
func (s *Store) SetSettings(v Settings) error {
	s.mu.Lock()
	if v.RefreshSeconds < 1 {
		v.RefreshSeconds = 1
	}
	if v.DutyCyclePercent <= 0 || v.DutyCyclePercent > 100 {
		v.DutyCyclePercent = defaultSettings().DutyCyclePercent
	}
	s.file.Settings = v
	s.mu.Unlock()
	return s.save()
}

// Clusters returns a copy of every configured cluster, ordered by name so the
// interface does not reshuffle between requests.
func (s *Store) Clusters() []*Cluster {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Cluster, 0, len(s.file.Clusters))
	for _, c := range s.file.Clusters {
		clone := *c
		clone.Nodes = append([]Node{}, c.Nodes...)
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Cluster returns one cluster by identifier.
func (s *Store) Cluster(id string) (*Cluster, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.file.Clusters {
		if c.ID == id {
			clone := *c
			clone.Nodes = append([]Node{}, c.Nodes...)
			return &clone, true
		}
	}
	return nil, false
}

// Add stores a new cluster, assigning an identifier.
func (s *Store) Add(c *Cluster) (*Cluster, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if c.ID == "" {
		c.ID = newID(c.Name, s.existingIDs())
	}
	if c.Schedule.Mode == "" {
		c.Schedule.Mode = ScheduleManual
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	s.file.Clusters = append(s.file.Clusters, c)
	s.mu.Unlock()

	if err := s.save(); err != nil {
		return nil, err
	}
	return c, nil
}

// Update replaces a cluster, keeping its identifier and creation time.
func (s *Store) Update(id string, c *Cluster) (*Cluster, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	var found *Cluster
	for i, existing := range s.file.Clusters {
		if existing.ID != id {
			continue
		}
		c.ID = id
		c.CreatedAt = existing.CreatedAt
		c.UpdatedAt = time.Now().UTC()
		if c.Schedule.Mode == "" {
			c.Schedule.Mode = ScheduleManual
		}
		s.file.Clusters[i] = c
		found = c
		break
	}
	s.mu.Unlock()

	if found == nil {
		return nil, fmt.Errorf("no database with id %q", id)
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return found, nil
}

// Delete removes a cluster.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	kept := s.file.Clusters[:0]
	removed := false
	for _, c := range s.file.Clusters {
		if c.ID == id {
			removed = true
			continue
		}
		kept = append(kept, c)
	}
	s.file.Clusters = kept
	s.mu.Unlock()

	if !removed {
		return fmt.Errorf("no database with id %q", id)
	}
	return s.save()
}

// existingIDs collects taken identifiers. Caller must hold the lock.
func (s *Store) existingIDs() map[string]bool {
	out := make(map[string]bool, len(s.file.Clusters))
	for _, c := range s.file.Clusters {
		out[c.ID] = true
	}
	return out
}

// save writes the file atomically with owner-only permissions.
//
// The file holds connection strings, so it is written to a temporary file in
// the same directory and renamed, which avoids ever leaving a partially
// written config behind, and is created 0600 so other users on the machine
// cannot read it.
func (s *Store) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.file, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	// Encrypt when a key is configured. This matters most in a container,
	// where the file lives on a volume that outlives the process and gets
	// snapshotted and backed up.
	if key := encryptionKey(); key != nil {
		sealed, err := encrypt(data, key)
		if err != nil {
			return fmt.Errorf("encrypt config: %w", err)
		}
		data = sealed
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// envRef matches ${NAME} references inside a connection string.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Resolve expands ${VAR} references in a connection string from the
// environment, so a password can live in the environment rather than on disk.
// A reference to an unset variable is an error rather than an empty
// substitution, because silently connecting without a password produces a
// confusing failure later.
func Resolve(dsn string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(dsn, func(match string) string {
		name := envRef.FindStringSubmatch(match)[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("connection string references unset environment variable(s): %s",
			strings.Join(missing, ", "))
	}
	return out, nil
}

// Duration is a time.Duration that serialises as a string such as "6h".
type Duration string

// Duration parses the value, returning zero when it is empty or invalid.
func (d Duration) Duration() time.Duration {
	if d == "" {
		return 0
	}
	v, err := time.ParseDuration(string(d))
	if err != nil {
		return 0
	}
	return v
}

// newID derives a readable, unique identifier from a name.
func newID(name string, taken map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "database"
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}
