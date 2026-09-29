package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
)

// RollbackUseCase handles one-click rollback to the previous healthy revision
type RollbackUseCase struct {
	deployUC *DeployUseCase
	baseDir  string
}

// NewRollbackUseCase initializes RollbackUseCase
func NewRollbackUseCase(deployUC *DeployUseCase, baseDir string) *RollbackUseCase {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "releases")
	}
	_ = os.MkdirAll(baseDir, 0755)
	return &RollbackUseCase{
		deployUC: deployUC,
		baseDir:  baseDir,
	}
}

// RecordRelease appends a new release record to the service history
func RecordRelease(baseDir string, rec *entity.ReleaseRecord) error {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "releases")
	}
	svcDir := filepath.Join(baseDir, rec.ServiceName)
	_ = os.MkdirAll(svcDir, 0755)
	historyFile := filepath.Join(svcDir, "history.json")

	var history []*entity.ReleaseRecord
	if data, err := os.ReadFile(historyFile); err == nil {
		_ = json.Unmarshal(data, &history)
	}

	history = append(history, rec)
	// Keep up to 20 releases
	if len(history) > 20 {
		history = history[len(history)-20:]
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(historyFile, data, 0644)
}

// GetReleaseHistory returns release records for a service (newest first)
func GetReleaseHistory(baseDir, svcName string) ([]*entity.ReleaseRecord, error) {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna", "releases")
	}
	historyFile := filepath.Join(baseDir, svcName, "history.json")
	data, err := os.ReadFile(historyFile)
	if err != nil {
		return nil, fmt.Errorf("no release history found for service '%s'", svcName)
	}

	var history []*entity.ReleaseRecord
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, err
	}

	// Reverse to newest first
	n := len(history)
	for i := 0; i < n/2; i++ {
		history[i], history[n-1-i] = history[n-1-i], history[i]
	}
	return history, nil
}

// Execute rolls back the given service to its previous release
func (uc *RollbackUseCase) Execute(ctx context.Context, project *entity.Project, serviceName string, logWriter io.Writer) (*entity.ReleaseRecord, error) {
	if logWriter == nil {
		logWriter = os.Stdout
	}

	svc, ok := project.Services[serviceName]
	if !ok {
		if len(project.Services) == 1 {
			for _, s := range project.Services {
				svc = s
				serviceName = s.Name
				break
			}
		} else {
			return nil, fmt.Errorf("service '%s' not found in configuration", serviceName)
		}
	}

	history, err := GetReleaseHistory(uc.baseDir, serviceName)
	if err != nil || len(history) < 2 {
		return nil, fmt.Errorf("no previous revision available to rollback for service '%s'", serviceName)
	}

	currentRev := history[0]
	targetRev := history[1]

	_, _ = fmt.Fprintln(logWriter, i18n.T("rollback_starting", serviceName, currentRev.Revision, targetRev.Revision))

	// Revert service attributes to previous revision
	if targetRev.Image != "" {
		svc.Image = targetRev.Image
	}
	if len(targetRev.Ports) > 0 {
		svc.Ports = targetRev.Ports
	}
	if targetRev.Replicas > 0 {
		svc.Replicas = targetRev.Replicas
	}
	if svc.Ingress != nil && len(targetRev.Upstreams) > 0 {
		svc.Ingress.Upstreams = targetRev.Upstreams
	}

	// Deploy restored revision
	if err := uc.deployUC.Execute(ctx, project, serviceName, logWriter); err != nil {
		_, _ = fmt.Fprintln(logWriter, i18n.T("rollback_failed", serviceName, err))
		return nil, err
	}

	// Record the rollback as a new release event
	newRec := &entity.ReleaseRecord{
		Revision:    fmt.Sprintf("rollback-to-%s", targetRev.Revision),
		ServiceName: serviceName,
		Image:       svc.Image,
		Ports:       svc.Ports,
		Replicas:    svc.Replicas,
		Upstreams:   targetRev.Upstreams,
		CreatedAt:   time.Now(),
	}
	_ = RecordRelease(uc.baseDir, newRec)

	_, _ = fmt.Fprintln(logWriter, i18n.T("rollback_success", serviceName, targetRev.Revision))
	return targetRev, nil
}
