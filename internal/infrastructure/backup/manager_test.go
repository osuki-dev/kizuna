package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/backup"
)

func TestBackupCreationAndRetentionPruning(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	_ = os.MkdirAll(dataDir, 0755)
	_ = os.WriteFile(filepath.Join(dataDir, "app.txt"), []byte("hello kizuna"), 0644)

	// Create a dummy SQLite DB
	dbPath := filepath.Join(tmpDir, "test.db")
	_ = os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0644)

	backupDir := filepath.Join(tmpDir, "backups")
	mgr := backup.NewBackupManager(backupDir)

	cfg := &entity.BackupConfig{
		Paths: []string{dataDir},
		Database: &entity.DatabaseBackupConfig{
			Type: "sqlite",
			Path: dbPath,
		},
		Retention: &entity.RetentionConfig{
			MaxBackups: 2,
		},
	}

	// 1. Create first backup
	rec1, err := mgr.CreateBackup(context.Background(), "my-app", cfg)
	if err != nil {
		t.Fatalf("failed to create backup 1: %v", err)
	}
	if rec1.Size <= 0 {
		t.Errorf("expected positive backup size, got %d", rec1.Size)
	}

	time.Sleep(10 * time.Millisecond)

	// 2. Create second backup
	_, err = mgr.CreateBackup(context.Background(), "my-app", cfg)
	if err != nil {
		t.Fatalf("failed to create backup 2: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// 3. Create third backup (should trigger retention prune of oldest backup)
	_, err = mgr.CreateBackup(context.Background(), "my-app", cfg)
	if err != nil {
		t.Fatalf("failed to create backup 3: %v", err)
	}

	list, err := mgr.ListBackups(context.Background(), "my-app")
	if err != nil {
		t.Fatalf("failed to list backups: %v", err)
	}

	// Because MaxBackups is 2, the total remaining backups must be exactly 2!
	if len(list) != 2 {
		t.Fatalf("expected 2 backups after retention prune, got %d", len(list))
	}
}
