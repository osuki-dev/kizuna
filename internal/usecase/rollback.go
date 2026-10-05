package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
)

// RollbackUseCase restores a recorded application configuration and immutable image.
// It does not restore databases, volumes, or mutable build artifacts.
type RollbackUseCase struct {
	deployUC *DeployUseCase
	baseDir  string
}

func NewRollbackUseCase(deployUC *DeployUseCase, baseDir string) *RollbackUseCase {
	if baseDir == "" {
		baseDir = deployUC.releaseDir
	}
	return &RollbackUseCase{deployUC: deployUC, baseDir: baseDir}
}

var releaseMu sync.Mutex

type storedRelease struct {
	*entity.ReleaseRecord
	Service *entity.Service `json:"service,omitempty"`
}

func releaseBase(baseDir string) (string, error) {
	if baseDir != "" {
		return baseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kizuna", "releases"), nil
}

func historyPath(baseDir, key string) (string, error) {
	base, err := releaseBase(baseDir)
	if err != nil {
		return "", err
	}
	// Hash even legacy keys: service names must never become filesystem paths.
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, hex.EncodeToString(sum[:]), "history.json"), nil
}

func projectReleaseKey(project *entity.Project, svc *entity.Service) string {
	targets := serviceTargets(project, svc)
	for i, target := range targets {
		host := entity.ParseTargetHost(target)
		targets[i] = fmt.Sprintf("%s:%s:%s:%d", host.Type, host.User, host.Host, host.Port)
	}
	sort.Strings(targets)
	key, _ := json.Marshal([]any{project.Name, project.ActiveEnv, targets, svc.Name})
	return string(key)
}

func cloneService(svc *entity.Service) (*entity.Service, error) {
	data, err := json.Marshal(svc)
	if err != nil {
		return nil, err
	}
	var clone entity.Service
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func readHistory(baseDir, key string) ([]*storedRelease, error) {
	path, err := historyPath(baseDir, key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var history []*storedRelease
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("invalid release history: %w", err)
	}
	for _, rec := range history {
		if rec == nil || rec.ReleaseRecord == nil {
			return nil, fmt.Errorf("invalid release history entry")
		}
	}
	return history, nil
}

func appendRelease(baseDir, key string, rec *storedRelease) error {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	path, err := historyPath(baseDir, key)
	if err != nil {
		return err
	}
	history, err := readHistory(baseDir, key)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	history = append(history, rec)
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// RecordRelease records legacy unscoped history. Unscoped records are never used
// for rollback because their project, environment and target cannot be verified.
func RecordRelease(baseDir string, rec *entity.ReleaseRecord) error {
	if rec == nil {
		return fmt.Errorf("release record is required")
	}
	return appendRelease(baseDir, rec.ServiceName, &storedRelease{ReleaseRecord: rec})
}

func recordProjectRelease(baseDir string, project *entity.Project, svc *entity.Service, rec *entity.ReleaseRecord) error {
	snapshot, err := cloneService(svc)
	if err != nil {
		return err
	}
	return appendRelease(baseDir, projectReleaseKey(project, svc), &storedRelease{ReleaseRecord: rec, Service: snapshot})
}

// GetReleaseHistory returns legacy unscoped release records, newest first.
func GetReleaseHistory(baseDir, svcName string) ([]*entity.ReleaseRecord, error) {
	history, err := readHistory(baseDir, svcName)
	if err != nil {
		return nil, err
	}
	result := make([]*entity.ReleaseRecord, len(history))
	for i, rec := range history {
		result[len(history)-1-i] = rec.ReleaseRecord
	}
	return result, nil
}

func ingressIdentity(svc *entity.Service) string {
	if svc.Ingress == nil || svc.Ingress.Provider == "" || svc.Ingress.Provider == "none" || svc.Ingress.Domain == "" {
		return ""
	}
	return svc.Ingress.Provider + ":" + svc.Ingress.Domain
}

func (uc *RollbackUseCase) Execute(ctx context.Context, project *entity.Project, serviceName string, logWriter io.Writer) (*entity.ReleaseRecord, error) {
	if logWriter == nil {
		logWriter = os.Stdout
	}
	svc, ok := project.Services[serviceName]
	if !ok {
		return nil, fmt.Errorf("service %q not found in configuration", serviceName)
	}
	history, err := readHistory(uc.baseDir, projectReleaseKey(project, svc))
	if err != nil {
		return nil, fmt.Errorf("read scoped release history: %w", err)
	}
	if len(history) < 2 {
		return nil, fmt.Errorf("no previous revision available for service %q", serviceName)
	}
	current, target := history[len(history)-1], history[len(history)-2]
	if target.Service == nil {
		return nil, fmt.Errorf("release has no complete service snapshot; rollback is unsafe")
	}
	restored, err := cloneService(target.Service)
	if err != nil {
		return nil, err
	}
	if restored.Type != entity.TypeDocker || (!strings.HasPrefix(restored.Image, "sha256:") && !strings.Contains(restored.Image, "@sha256:")) {
		return nil, fmt.Errorf("rollback requires a recorded immutable Docker image; Compose, process and mutable artifact rollback are unsupported")
	}
	if len(serviceTargets(project, restored)) > 1 && restored.Dockerfile != "" {
		return nil, fmt.Errorf("multi-target build rollback requires per-target immutable artifact records")
	}
	if ingressIdentity(svc) != ingressIdentity(restored) {
		return nil, fmt.Errorf("rollback changes the ingress domain or enables/disables ingress; migrate the current route manually before rollback")
	}
	// Do not rerun a local build or rebuild changed Dockerfile sources.
	restored.Build = nil
	restored.Dockerfile = ""
	_, _ = fmt.Fprintln(logWriter, i18n.T("rollback_starting", serviceName, current.Revision, target.Revision))
	copiedProject := *project
	copiedProject.Services = make(map[string]*entity.Service, len(project.Services))
	for name, service := range project.Services {
		copiedProject.Services[name] = service
	}
	copiedProject.Services[serviceName] = restored
	// Record exactly once in the same history directory as rollback's lookup.
	deploy := *uc.deployUC
	deploy.releaseDir = uc.baseDir
	if err := deploy.Execute(ctx, &copiedProject, serviceName, logWriter); err != nil {
		return nil, fmt.Errorf("rollback failed: %w", err)
	}
	project.Services[serviceName] = restored
	_, _ = fmt.Fprintln(logWriter, i18n.T("rollback_success", serviceName, target.Revision))
	return target.ReleaseRecord, nil
}
