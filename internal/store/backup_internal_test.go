package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestBackupContextCancelsWhileWaitingForLock(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	<-st.backupSem
	defer func() { st.backupSem <- struct{}{} }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.BackupContext(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
