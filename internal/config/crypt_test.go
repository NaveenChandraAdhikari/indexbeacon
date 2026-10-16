package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The configuration outlives the process in a container: it sits on a volume
// that gets snapshotted, backed up and copied. These tests cover that case.

func TestEncryptedRoundTrip(t *testing.T) {
	t.Setenv(keyEnv, "a-long-passphrase-from-a-secret")
	path := filepath.Join(t.TempDir(), "config.json")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.Add(&Cluster{
		Name:  "Production",
		Nodes: []Node{{DSN: "postgres://app:hunter2@db.internal:5432/orders"}},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The password must not be recoverable from the file itself.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Error("the password is readable in the file on disk")
	}
	if strings.Contains(string(raw), "db.internal") {
		t.Error("the hostname is readable in the file on disk")
	}
	if !isEncrypted(raw) {
		t.Error("the file was not written with the encrypted envelope")
	}

	// Reopening with the key must return exactly what went in, which is what
	// makes a pod restart survivable.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("re-open with the key: %v", err)
	}
	got, ok := reopened.Cluster("production")
	if !ok {
		t.Fatal("cluster did not survive the round trip")
	}
	if got.Nodes[0].DSN != "postgres://app:hunter2@db.internal:5432/orders" {
		t.Errorf("connection string came back as %q", got.Nodes[0].DSN)
	}
}

func TestEncryptedFileRefusesToOpenWithoutKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	t.Setenv(keyEnv, "the-key")
	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	t.Setenv(keyEnv, "")
	_, err := Open(path)
	if !errors.Is(err, ErrKeyRequired) {
		t.Errorf("Open without a key = %v, want ErrKeyRequired", err)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	t.Setenv(keyEnv, "the-right-key")
	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	t.Setenv(keyEnv, "a-different-key")
	_, err := Open(path)
	if !errors.Is(err, ErrWrongKey) {
		t.Errorf("Open with the wrong key = %v, want ErrWrongKey", err)
	}
}

func TestTamperedFileIsRejected(t *testing.T) {
	// Authenticated encryption means an edited file fails to open rather
	// than silently returning altered connection strings.
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(keyEnv, "the-key")

	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	raw, _ := os.ReadFile(path)
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Error("a tampered file was accepted")
	}
}

func TestStrippingTheHeaderIsRejected(t *testing.T) {
	// The header is authenticated, so a file cannot be downgraded to look
	// like plaintext.
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv(keyEnv, "the-key")

	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, raw[len(magic):], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("a file with its header stripped was accepted as plaintext")
	}
}

func TestPlaintextFileStillOpensWithAKeySet(t *testing.T) {
	// Turning encryption on for an existing installation must not require a
	// migration step: the plaintext file is read, then encrypted on the next
	// save.
	path := filepath.Join(t.TempDir(), "config.json")

	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	t.Setenv(keyEnv, "newly-added-key")
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("plaintext file with a key set: %v", err)
	}
	if _, ok := reopened.Cluster("x"); !ok {
		t.Fatal("existing cluster was lost")
	}

	// Saving anything now encrypts the file.
	if _, err := reopened.Add(&Cluster{Name: "y", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !isEncrypted(raw) {
		t.Error("the file was not encrypted after a save with a key set")
	}
}

func TestRawKeyIsUsedDirectly(t *testing.T) {
	// A proper 32 byte key needs no stretching, and the two encodings a
	// secret manager is likely to produce must both work.
	hexKey := strings.Repeat("ab", 32)
	b64Key := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

	for _, key := range []string{hexKey, b64Key} {
		path := filepath.Join(t.TempDir(), "config.json")
		t.Setenv(keyEnv, key)

		store, _ := Open(path)
		if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := Open(path); err != nil {
			t.Errorf("round trip with key %q: %v", key[:8]+"…", err)
		}
	}
}

func TestStatusExplainsWhatIsProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	store, _ := Open(path)
	if _, err := store.Add(&Cluster{Name: "x", Nodes: []Node{{DSN: "postgres://h/d"}}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	st := store.Status()
	if st.Enabled || st.FileEncrypted {
		t.Errorf("status reported protection that is not there: %+v", st)
	}
	if st.Advice == "" {
		t.Error("an unencrypted configuration should explain the options")
	}

	t.Setenv(keyEnv, "a-key")
	st = store.Status()
	if !st.Enabled {
		t.Error("a configured key was not reported")
	}
	if st.Advice == "" {
		t.Error("a key set against a plaintext file should say the file is not encrypted yet")
	}
}
