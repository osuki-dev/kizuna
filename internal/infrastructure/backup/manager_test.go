package backup_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/backup"
)

func TestBackupCreationAndRetentionPruning(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "app.txt"), []byte("hello kizuna"), 0644); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(tmpDir, "backups")
	mgr := backup.NewBackupManager(backupDir)

	cfg := &entity.BackupConfig{
		Paths: []string{dataDir},
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

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gz.Close(); err != nil {
			t.Error(err)
		}
	}()
	tr := tar.NewReader(gz)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = data
	}
	// Read gzip to EOF too, which verifies its checksum/trailer.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestFailedBackupDoesNotPublishOrPrune(t *testing.T) {
	for _, mode := range []string{"missing-path", "failed-dump", "empty-dump", "invalid-storage", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			mgr := backup.NewBackupManager(dir)
			data := filepath.Join(dir, "data")
			if err := os.WriteFile(data, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			good := &entity.BackupConfig{Paths: []string{data}}
			first, err := mgr.CreateBackup(context.Background(), "app", good)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &entity.BackupConfig{Paths: []string{data}, Retention: &entity.RetentionConfig{MaxBackups: 1}}
			ctx := context.Background()
			switch mode {
			case "missing-path":
				cfg.Paths = []string{filepath.Join(dir, "missing")}
			case "failed-dump":
				cfg.Database = &entity.DatabaseBackupConfig{Type: "custom", Command: "printf incomplete; exit 1"}
			case "empty-dump":
				cfg.Database = &entity.DatabaseBackupConfig{Type: "custom", Command: "true"}
			case "invalid-storage":
				cfg.Storage = &entity.StorageConfig{Type: "s3"}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if rec, err := mgr.CreateBackup(ctx, "app", cfg); err == nil || rec != nil {
				t.Fatalf("failure returned record=%v, error=%v", rec, err)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "app"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != first.Filename {
				t.Fatalf("failed backup changed completed backups: %v", entries)
			}
			readArchive(t, filepath.Join(dir, "app", first.Filename))
		})
	}
}

func TestSQLiteBackupIncludesWALAndRestores(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 CLI is required for SQLite recovery integration test")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "live.db")
	// Keep this connection open, preventing SQLite from checkpointing WAL on close.
	cmd := exec.Command(sqlite, db)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Errorf("SQLite fixture exited: %v: %s", err, stderr.String())
		}
	}()
	if _, err := io.WriteString(input, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE items(value TEXT); INSERT INTO items VALUES('committed-in-wal'); SELECT 'ready';\n"); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(output)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "ready" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("failed to initialize SQLite: %s", stderr.String())
	}
	if stat, err := os.Stat(db + "-wal"); err != nil || stat.Size() == 0 {
		t.Fatalf("expected uncheckpointed WAL: %v", err)
	}
	mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
	rec, err := mgr.CreateBackup(context.Background(), "sqlite-app", &entity.BackupConfig{Database: &entity.DatabaseBackupConfig{Type: "sqlite", Path: db}})
	if err != nil {
		t.Fatal(err)
	}
	entries := readArchive(t, filepath.Join(dir, "backups", "sqlite-app", rec.Filename))
	restored := filepath.Join(dir, "restored.db")
	if err := os.WriteFile(restored, entries["database_sqlite.dump"], 0600); err != nil {
		t.Fatal(err)
	}
	data, err := exec.Command(sqlite, restored, "PRAGMA integrity_check; SELECT value FROM items;").CombinedOutput()
	if err != nil {
		t.Fatalf("restore query failed: %v: %s", err, data)
	}
	if string(data) != "ok\ncommitted-in-wal\n" {
		t.Fatalf("restored DB lost committed WAL data: %s", data)
	}
}

func TestSQLiteBackupRequiresCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	db := filepath.Join(dir, "db.sqlite")
	if err := os.WriteFile(db, []byte("SQLite format 3\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
	if _, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Database: &entity.DatabaseBackupConfig{Type: "sqlite", Path: db}}); err == nil {
		t.Fatal("must reject missing backup tool rather than copy SQLite main file")
	}
}

func TestBackupRejectsTraversal(t *testing.T) {
	mgr := backup.NewBackupManager(t.TempDir())
	for _, name := range []string{"", "..", "../escaped", "a/b", "a\\b"} {
		if _, err := mgr.CreateBackup(context.Background(), name, &entity.BackupConfig{Paths: []string{"."}}); err == nil {
			t.Fatalf("accepted service %q", name)
		}
		if _, err := mgr.ListBackups(context.Background(), name); err == nil {
			t.Fatalf("listed service %q", name)
		}
		if _, err := mgr.(*backup.Manager).PruneBackups(name, &entity.RetentionConfig{MaxBackups: 1}); err == nil {
			t.Fatalf("pruned service %q", name)
		}
	}
}

func TestBackupS3FailureDoesNotPublishOrPrune(t *testing.T) {
	dir := t.TempDir()
	mgr := backup.NewBackupManager(dir)
	data := filepath.Join(dir, "data")
	if err := os.WriteFile(data, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Paths: []string{data}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Paths: []string{data}, Storage: &entity.StorageConfig{Type: "s3", Bucket: "test", Endpoint: "://invalid", AccessKey: "test", SecretKey: "test"}, Retention: &entity.RetentionConfig{MaxBackups: 1}})
	if err == nil {
		t.Fatal("expected S3 upload failure")
	}
	uploadErr := err
	entries, err := os.ReadDir(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != first.Filename {
		t.Fatalf("failed upload changed backups: %v", entries)
	}
	if strings.Contains(uploadErr.Error(), "SecretKey") {
		t.Fatal("error leaked credentials")
	}
}

func TestDatabaseContainerExecutionAndCredentialHandling(t *testing.T) {
	for _, dbType := range []string{"postgres", "mysql"} {
		t.Run(dbType, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "args")
			t.Setenv("CAPTURE_ARGS", capture)
			t.Setenv("PATH", dir)
			// A fake CLI captures arguments and outputs a small valid test dump.
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_ARGS\"\nif [ \"${PGPASSWORD:-${MYSQL_PWD:-}}\" != test-password ]; then exit 3; fi\nprintf 'test-dump'\n"
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
			uri := "postgresql://user:test-password@localhost:5432/app"
			if dbType == "mysql" {
				uri = "mysql://user:test-password@localhost:3306/app"
			}
			rec, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Database: &entity.DatabaseBackupConfig{Type: dbType, URI: uri, Container: "existing-database"}})
			if err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "exec\n--env\n") || !strings.Contains(string(args), "existing-database\n") {
				t.Fatalf("did not exec selected container: %s", args)
			}
			if strings.Contains(string(args), "test-password") || strings.Contains(string(args), "run\n") {
				t.Fatalf("unsafe dump arguments: %s", args)
			}
			entries := readArchive(t, filepath.Join(dir, "backups", "app", rec.Filename))
			if string(entries["database_"+dbType+".dump"]) != "test-dump" {
				t.Fatal("dump was not archived")
			}
		})
	}
}

func TestDatabaseFailureCleansDumpAndDoesNotFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "pg_dump"), []byte("#!/bin/sh\nprintf partial; exit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf fallback-would-hide-the-error\n"), 0700); err != nil {
		t.Fatal(err)
	}
	mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
	if _, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Database: &entity.DatabaseBackupConfig{Type: "postgres", URI: "postgresql://user@localhost/app"}}); err == nil {
		t.Fatal("failed pg_dump was hidden")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "kizuna-db-") {
			t.Fatalf("temporary dump leaked: %s", entry.Name())
		}
	}
}

func TestBackupIncludesExplicitDataEvenWhenGitIgnored(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{".gitignore": ".env\napp.log\n", ".env": "TEST_VALUE=fixture", "app.log": "important-data"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
	rec, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Paths: []string{data}})
	if err != nil {
		t.Fatal(err)
	}
	entries := readArchive(t, filepath.Join(dir, "backups", "app", rec.Filename))
	if string(entries[".env"]) != "TEST_VALUE=fixture" || string(entries["app.log"]) != "important-data" {
		t.Fatal("backup silently applied source-control ignore rules")
	}
}

func TestMultipleBackupPathsPreserveMatchingFilenames(t *testing.T) {
	for _, filesOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("files=%t", filesOnly), func(t *testing.T) {
			dir := t.TempDir()
			var paths []string
			for index, value := range []string{"first-source-data", "second-source-data"} {
				source := filepath.Join(dir, fmt.Sprintf("source-%d", index))
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(source, "app.txt")
				if err := os.WriteFile(file, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
				if filesOnly {
					paths = append(paths, file)
				} else {
					paths = append(paths, source)
				}
			}
			mgr := backup.NewBackupManager(filepath.Join(dir, "backups"))
			rec, err := mgr.CreateBackup(context.Background(), "app", &entity.BackupConfig{Paths: paths})
			if err != nil {
				t.Fatal(err)
			}
			entries := readArchive(t, filepath.Join(dir, "backups", "app", rec.Filename))
			if string(entries["paths/001/app.txt"]) != "first-source-data" || string(entries["paths/002/app.txt"]) != "second-source-data" {
				t.Fatalf("source data collided in archive: %v", entries)
			}
		})
	}
}
