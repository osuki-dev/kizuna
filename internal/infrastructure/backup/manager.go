package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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
)

// Manager creates local archives, optionally uploads them to S3, and applies local retention.
type Manager struct {
	baseDir string
}

// NewBackupManager initializes backup manager
func NewBackupManager(baseDir string) domain.BackupManager {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "backups")
	}
	return &Manager{baseDir: baseDir}
}

// CreateBackup creates a compressed snapshot of specified paths and/or database dumps and applies retention policy
func (m *Manager) CreateBackup(ctx context.Context, svcName string, cfg *entity.BackupConfig) (*entity.BackupRecord, error) {
	if err := validateServiceName(svcName); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil || (len(cfg.Paths) == 0 && cfg.Database == nil) {
		return nil, fmt.Errorf("no paths or database specified for backup")
	}
	if cfg.Storage != nil {
		if cfg.Storage.Type != "" && cfg.Storage.Type != "local" && cfg.Storage.Type != "s3" {
			return nil, fmt.Errorf("unsupported backup storage type")
		}
		if cfg.Storage.Type == "s3" && cfg.Storage.Bucket == "" {
			return nil, fmt.Errorf("S3 backup bucket is required")
		}
		if cfg.Storage.LocalDir != "" && filepath.Clean(cfg.Storage.LocalDir) != filepath.Clean(m.baseDir) {
			return nil, fmt.Errorf("storage.local_dir must match the backup manager directory; per-service directories are not supported")
		}
	}
	svcBackupDir := filepath.Join(m.baseDir, svcName)
	if err := os.MkdirAll(svcBackupDir, 0700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	outFile, err := os.CreateTemp(svcBackupDir, ".backup-*.partial")
	if err != nil {
		return nil, err
	}
	tmpPath := outFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	defer func() { _ = outFile.Close() }()
	gw := gzip.NewWriter(outFile)
	defer func() { _ = gw.Close() }()
	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()
	for index, p := range cfg.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prefix := ""
		if len(cfg.Paths) > 1 {
			prefix = fmt.Sprintf("paths/%03d", index+1)
		}
		if err := addPathToTar(tw, p, prefix); err != nil {
			return nil, fmt.Errorf("archive backup path: %w", err)
		}
	}
	if cfg.Database != nil {
		dumpFile, cleanup, err := dumpDatabase(ctx, svcName, cfg.Database)
		defer cleanup()
		if err != nil {
			return nil, err
		}
		if err := addSingleFileToTar(tw, dumpFile, fmt.Sprintf("database_%s.dump", strings.ToLower(cfg.Database.Type))); err != nil {
			return nil, fmt.Errorf("archive database dump: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("finish gzip archive: %w", err)
	}
	if err := outFile.Sync(); err != nil {
		return nil, err
	}
	if err := outFile.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat, err := os.Stat(tmpPath)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	filename := fmt.Sprintf("%s_backup_%s_%s.tar.gz", svcName, now.Format("20060102_150405.000"), strings.TrimSuffix(strings.TrimPrefix(filepath.Base(tmpPath), ".backup-"), ".partial"))
	storageType := "local"
	if cfg.Storage != nil && cfg.Storage.Type == "s3" {
		if err := m.uploadToS3(ctx, tmpPath, svcName+"/"+filename, cfg.Storage); err != nil {
			return nil, fmt.Errorf("S3 upload failed: %w", err)
		}
		storageType = "s3"
	}
	if err := os.Rename(tmpPath, filepath.Join(svcBackupDir, filename)); err != nil {
		return nil, fmt.Errorf("publish backup: %w", err)
	}
	record := &entity.BackupRecord{ID: filename, ServiceName: svcName, Filename: filename, Size: stat.Size(), StorageType: storageType, CreatedAt: now}
	if cfg.Retention != nil {
		if _, err := m.PruneBackups(svcName, cfg.Retention); err != nil {
			return record, fmt.Errorf("backup created, retention failed: %w", err)
		}
	}
	return record, nil
}

func validateServiceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid backup service name")
	}
	return nil
}

// PruneBackups removes old backup files matching the retention policy (keep_days, max_backups)
func (m *Manager) PruneBackups(svcName string, retention *entity.RetentionConfig) (int, error) {
	if err := validateServiceName(svcName); err != nil {
		return 0, err
	}
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
			info, err := e.Info()
			if err != nil {
				return 0, err
			}
			files = append(files, fileInfo{
				name: e.Name(), path: fullPath, modTime: info.ModTime(),
			})
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
			if err := os.Remove(f.path); err != nil {
				return prunedCount, err
			}
			prunedCount++
		}
		files = files[:retention.MaxBackups]
	}

	// 2. Prune by keep_days age
	if retention.KeepDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retention.KeepDays)
		for _, f := range files {
			if f.modTime.Before(cutoff) {
				if err := os.Remove(f.path); err != nil {
					return prunedCount, err
				}
				prunedCount++
			}
		}
	}

	return prunedCount, nil
}

// dumpDatabase executes database dump based on configured type (postgres, mysql, sqlite, custom)
func dumpDatabase(ctx context.Context, svcName string, dbCfg *entity.DatabaseBackupConfig) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "kizuna-db-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	tmpPath := filepath.Join(tmpDir, "database.dump")
	fail := func(err error) (string, func(), error) { cleanup(); return "", cleanup, err }
	var program string
	var args []string
	var env []string
	switch strings.ToLower(dbCfg.Type) {
	case "postgres", "pgsql", "postgresql":
		program = "pg_dump"
		uri := os.ExpandEnv(dbCfg.URI)
		if uri == "" {
			uri = os.Getenv("DATABASE_URL")
		}
		if uri != "" {
			parsed, err := url.Parse(uri)
			if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
				return fail(fmt.Errorf("invalid PostgreSQL connection URI"))
			}
			if parsed.User != nil {
				if password, ok := parsed.User.Password(); ok {
					env = append(env, "PGPASSWORD="+password)
					parsed.User = url.User(parsed.User.Username())
				}
			}
			query := parsed.Query()
			if password := query.Get("password"); password != "" {
				env = append(env, "PGPASSWORD="+password)
				query.Del("password")
				parsed.RawQuery = query.Encode()
			}
			args = []string{parsed.String()}
		} else if dbCfg.Container == "" {
			return fail(fmt.Errorf("PostgreSQL backup requires a URI or container"))
		}
	case "mysql", "mariadb":
		program = "mysqldump"
		uri := os.ExpandEnv(dbCfg.URI)
		if uri == "" {
			return fail(fmt.Errorf("MySQL backup requires a connection URI"))
		}
		parsed, err := url.Parse(uri)
		if err != nil || parsed.Scheme != "mysql" || parsed.Hostname() == "" || strings.TrimPrefix(parsed.Path, "/") == "" {
			return fail(fmt.Errorf("invalid MySQL connection URI"))
		}
		args = []string{"--single-transaction", "--host=" + parsed.Hostname()}
		if parsed.Port() != "" {
			args = append(args, "--port="+parsed.Port())
		}
		if parsed.User != nil {
			args = append(args, "--user="+parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				env = append(env, "MYSQL_PWD="+password)
			}
		}
		args = append(args, "--", strings.TrimPrefix(parsed.Path, "/"))
	case "sqlite", "sqlite3":
		if dbCfg.Container != "" {
			return fail(fmt.Errorf("SQLite container backup is not supported; configure a host-mounted database path"))
		}
		dbPath := os.ExpandEnv(dbCfg.Path)
		info, err := os.Stat(dbPath)
		if err != nil || !info.Mode().IsRegular() {
			return fail(fmt.Errorf("SQLite backup requires an existing regular database file"))
		}
		dbPath, err = filepath.Abs(dbPath)
		if err != nil {
			return fail(err)
		}
		program = "sqlite3"
		args = []string{"-readonly", dbPath, ".backup '" + strings.ReplaceAll(tmpPath, "'", "''") + "'"}
	case "custom":
		if dbCfg.Command == "" {
			return fail(fmt.Errorf("custom database backup command is required"))
		}
		program = "sh"
		args = []string{"-c", dbCfg.Command}
	default:
		return fail(fmt.Errorf("unsupported database backup type"))
	}
	if dbCfg.Container != "" {
		dockerArgs := []string{"exec"}
		for _, value := range env {
			dockerArgs = append(dockerArgs, "--env", strings.SplitN(value, "=", 2)[0])
		}
		dockerArgs = append(dockerArgs, "--", dbCfg.Container, program)
		args = append(dockerArgs, args...)
		program = "docker"
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), env...)
	if strings.ToLower(dbCfg.Type) != "sqlite" && strings.ToLower(dbCfg.Type) != "sqlite3" {
		out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return fail(err)
		}
		cmd.Stdout = out
		runErr := cmd.Run()
		closeErr := out.Close()
		if err := errors.Join(runErr, closeErr); err != nil {
			return fail(fmt.Errorf("database dump command failed: %w", err))
		}
	} else if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("SQLite backup command failed: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fail(err)
	}
	if info.Size() == 0 {
		return fail(fmt.Errorf("database dump is empty"))
	}
	return tmpPath, cleanup, nil
}

func (m *Manager) uploadToS3(ctx context.Context, localFilePath, objectKey string, storage *entity.StorageConfig) error {
	region := storage.Region
	if region == "" {
		endpoint, _ := url.Parse(storage.Endpoint)
		if endpoint != nil && (endpoint.Hostname() == "r2.cloudflarestorage.com" || strings.HasSuffix(endpoint.Hostname(), ".r2.cloudflarestorage.com")) {
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
	if err := validateServiceName(svcName); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	svcBackupDir := filepath.Join(m.baseDir, svcName)
	entries, err := os.ReadDir(svcBackupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var records []*entity.BackupRecord
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar.gz") {
			info, err := entry.Info()
			if err != nil {
				return nil, err
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

func addPathToTar(tw *tar.Writer, sourcePath, prefix string) error {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}

	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Clean(sourcePath)
	} else {
		baseDir = filepath.Dir(sourcePath)
	}

	return filepath.Walk(sourcePath, func(path string, fileInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}

		if !fileInfo.Mode().IsRegular() && !fileInfo.IsDir() && fileInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported file type in backup")
		}
		header, err := tar.FileInfoHeader(fileInfo, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(filepath.Join(prefix, relPath))

		if fileInfo.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = link
			return tw.WriteHeader(header)
		}

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
