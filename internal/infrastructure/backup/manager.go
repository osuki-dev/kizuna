package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/ignore"
)

// Manager implements domain.BackupManager with enterprise-grade AWS SDK v2, database dumps, and retention
type Manager struct {
	baseDir string
}

// NewBackupManager initializes backup manager
func NewBackupManager(baseDir string) domain.BackupManager {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "backups")
	}
	_ = os.MkdirAll(baseDir, 0755)
	return &Manager{baseDir: baseDir}
}

// CreateBackup creates a compressed snapshot of specified paths and/or database dumps and applies retention policy
func (m *Manager) CreateBackup(ctx context.Context, svcName string, cfg *entity.BackupConfig) (*entity.BackupRecord, error) {
	if cfg == nil || (len(cfg.Paths) == 0 && cfg.Database == nil) {
		return nil, fmt.Errorf("no paths or database specified for backup")
	}

	svcBackupDir := filepath.Join(m.baseDir, svcName)
	_ = os.MkdirAll(svcBackupDir, 0755)

	timestamp := time.Now().Format("20060102_150405.000")
	filename := fmt.Sprintf("%s_backup_%s.tar.gz", svcName, timestamp)
	destPath := filepath.Join(svcBackupDir, filename)

	// 1. Create local tar.gz snapshot
	outFile, err := os.Create(destPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = outFile.Close() }()

	gw := gzip.NewWriter(outFile)
	defer func() { _ = gw.Close() }()
	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()

	// Add file paths if specified
	for _, p := range cfg.Paths {
		_ = addPathToTar(tw, p)
	}

	// Add database dump if specified
	if cfg.Database != nil {
		dumpFile, cleanup, err := dumpDatabase(ctx, svcName, cfg.Database)
		if err == nil && dumpFile != "" {
			defer cleanup()
			_ = addSingleFileToTar(tw, dumpFile, fmt.Sprintf("database_%s.dump", cfg.Database.Type))
		}
	}

	_ = tw.Close()
	_ = gw.Close()

	stat, err := os.Stat(destPath)
	if err != nil {
		return nil, err
	}

	storageType := "local"

	// 2. Upload to S3-compatible storage using official AWS SDK v2
	if cfg.Storage != nil && cfg.Storage.Type == "s3" && cfg.Storage.Bucket != "" {
		storageType = "s3"
		if err := m.uploadToS3(ctx, destPath, filename, cfg.Storage); err != nil {
			return nil, fmt.Errorf("local snapshot created, but S3 upload failed: %w", err)
		}
	}

	// 3. Apply retention pruning if configured
	if cfg.Retention != nil {
		_, _ = m.PruneBackups(svcName, cfg.Retention)
	}

	record := &entity.BackupRecord{
		ID:          fmt.Sprintf("bk_%d", time.Now().UnixNano()),
		ServiceName: svcName,
		Filename:    filename,
		Size:        stat.Size(),
		StorageType: storageType,
		CreatedAt:   time.Now(),
	}

	return record, nil
}

// PruneBackups removes old backup files matching the retention policy (keep_days, max_backups)
func (m *Manager) PruneBackups(svcName string, retention *entity.RetentionConfig) (int, error) {
	if retention == nil {
		return 0, nil
	}

	svcBackupDir := filepath.Join(m.baseDir, svcName)
	entries, err := os.ReadDir(svcBackupDir)
	if err != nil {
		return 0, err
	}

	type fileInfo struct {
		name    string
		path    string
		modTime time.Time
	}

	var files []fileInfo
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			fullPath := filepath.Join(svcBackupDir, e.Name())
			if info, err := e.Info(); err == nil {
				files = append(files, fileInfo{
					name:    e.Name(),
					path:    fullPath,
					modTime: info.ModTime(),
				})
			}
		}
	}

	// Sort newest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	prunedCount := 0

	// 1. Prune by max_backups count
	if retention.MaxBackups > 0 && len(files) > retention.MaxBackups {
		toRemove := files[retention.MaxBackups:]
		for _, f := range toRemove {
			if err := os.Remove(f.path); err == nil {
				prunedCount++
			}
		}
		files = files[:retention.MaxBackups]
	}

	// 2. Prune by keep_days age
	if retention.KeepDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retention.KeepDays)
		for _, f := range files {
			if f.modTime.Before(cutoff) {
				if err := os.Remove(f.path); err == nil {
					prunedCount++
				}
			}
		}
	}

	return prunedCount, nil
}

// dumpDatabase executes database dump based on configured type (postgres, mysql, sqlite, custom)
func dumpDatabase(ctx context.Context, svcName string, dbCfg *entity.DatabaseBackupConfig) (string, func(), error) {
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("kizuna_db_%s_*.dump", svcName))
	if err != nil {
		return "", func() {}, err
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	switch strings.ToLower(dbCfg.Type) {
	case "postgres", "pgsql", "postgresql":
		uri := os.ExpandEnv(dbCfg.URI)
		if uri == "" {
			uri = os.Getenv("DATABASE_URL")
		}
		if uri != "" {
			if _, err := exec.LookPath("pg_dump"); err == nil {
				cmd := exec.CommandContext(ctx, "pg_dump", uri)
				out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0600)
				if err == nil {
					cmd.Stdout = out
					defer func() { _ = out.Close() }()
					if err := cmd.Run(); err == nil {
						return tmpPath, cleanup, nil
					}
				}
			}
			// Transient docker container fallback
			if _, err := exec.LookPath("docker"); err == nil {
				cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=host", "postgres:alpine", "pg_dump", uri)
				out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0600)
				if err == nil {
					cmd.Stdout = out
					defer func() { _ = out.Close() }()
					if err := cmd.Run(); err == nil {
						return tmpPath, cleanup, nil
					}
				}
			}
		}

	case "mysql", "mariadb":
		uri := os.ExpandEnv(dbCfg.URI)
		if uri != "" {
			if _, err := exec.LookPath("docker"); err == nil {
				cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=host", "mariadb:alpine", "mysqldump", uri)
				out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0600)
				if err == nil {
					cmd.Stdout = out
					defer func() { _ = out.Close() }()
					if err := cmd.Run(); err == nil {
						return tmpPath, cleanup, nil
					}
				}
			}
		}

	case "sqlite", "sqlite3":
		dbPath := os.ExpandEnv(dbCfg.Path)
		if dbPath != "" {
			if stat, err := os.Stat(dbPath); err == nil && !stat.IsDir() {
				// Copy SQLite file safely
				data, err := os.ReadFile(dbPath)
				if err == nil {
					_ = os.WriteFile(tmpPath, data, 0600)
					return tmpPath, cleanup, nil
				}
			}
		}

	case "custom":
		if dbCfg.Command != "" {
			cmd := exec.CommandContext(ctx, "sh", "-c", dbCfg.Command)
			out, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0600)
			if err == nil {
				cmd.Stdout = out
				defer func() { _ = out.Close() }()
				if err := cmd.Run(); err == nil {
					return tmpPath, cleanup, nil
				}
			}
		}
	}

	return "", cleanup, fmt.Errorf("failed to dump database %s", dbCfg.Type)
}

func (m *Manager) uploadToS3(ctx context.Context, localFilePath, objectKey string, storage *entity.StorageConfig) error {
	region := storage.Region
	if region == "" {
		if storage.Endpoint != "" && (filepath.Base(storage.Endpoint) == "r2.cloudflarestorage.com" || filepath.Dir(storage.Endpoint) == "r2.cloudflarestorage.com") {
			region = "auto"
		} else {
			region = "us-east-1"
		}
	}

	opts := s3.Options{
		Region: region,
	}

	if storage.AccessKey != "" && storage.SecretKey != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(storage.AccessKey, storage.SecretKey, "")
	}

	if storage.Endpoint != "" {
		opts.BaseEndpoint = aws.String(storage.Endpoint)
	}

	if storage.PathStyle != nil {
		opts.UsePathStyle = *storage.PathStyle
	} else if storage.Endpoint != "" {
		opts.UsePathStyle = true
	}

	client := s3.New(opts)

	file, err := os.Open(localFilePath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(storage.Bucket),
		Key:    aws.String(objectKey),
		Body:   file,
	})
	return err
}

// ListBackups lists local backup archives for a given service
func (m *Manager) ListBackups(ctx context.Context, svcName string) ([]*entity.BackupRecord, error) {
	svcBackupDir := filepath.Join(m.baseDir, svcName)
	entries, err := os.ReadDir(svcBackupDir)
	if err != nil {
		return nil, nil
	}

	var records []*entity.BackupRecord
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar.gz") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			records = append(records, &entity.BackupRecord{
				ID:          entry.Name(),
				ServiceName: svcName,
				Filename:    entry.Name(),
				Size:        info.Size(),
				StorageType: "local",
				CreatedAt:   info.ModTime(),
			})
		}
	}

	// Sort newest first
	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})

	return records, nil
}

func addPathToTar(tw *tar.Writer, sourcePath string) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}

	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Clean(sourcePath)
	} else {
		baseDir = filepath.Dir(sourcePath)
	}

	matcher := ignore.NewMatcher(baseDir)

	return filepath.Walk(sourcePath, func(path string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}

		if relPath != "." && matcher.ShouldIgnore(relPath, fileInfo.IsDir()) {
			if fileInfo.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		header, err := tar.FileInfoHeader(fileInfo, fileInfo.Name())
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if fileInfo.Mode().IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()

		_, err = io.Copy(tw, file)
		return err
	})
}

func addSingleFileToTar(tw *tar.Writer, filePath, entryName string) error {
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	header, err := tar.FileInfoHeader(info, info.Name())
	if err != nil {
		return err
	}
	header.Name = entryName
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}
