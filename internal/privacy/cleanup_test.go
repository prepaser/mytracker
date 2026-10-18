package privacy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

func TestCleanupSyncFailurePreservesPendingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SafetyBackupName)
	if err := os.WriteFile(path, []byte("safety"), 0600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("sync failed")
	if err := clearSafetyBackup(dir, func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
	if pending, err := Pending(dir); err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("safety removal=%v", err)
	}
	if err := ClearSafetyBackup(dir); err != nil {
		t.Fatal(err)
	}
	if pending, err := Pending(dir); err != nil || pending {
		t.Fatalf("after retry pending=%v err=%v", pending, err)
	}
}

func TestMarkerCleanupSyncFailureKeepsCompletedRemoval(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SafetyBackupName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := clearSafetyBackup(dir, func(string) error {
		calls++
		if calls == 2 {
			return errors.New("marker sync failed")
		}
		return atomicfile.SyncDir(dir)
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("sync calls=%d", calls)
	}
	if pending, err := Pending(dir); err != nil || pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}
