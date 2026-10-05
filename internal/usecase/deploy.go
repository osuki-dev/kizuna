package usecase

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/i18n"
	"github.com/osuki-dev/kizuna/internal/infrastructure/ignore"
)

// NodeClient defines RPC client operations to a remote mesh agent
type NodeClient interface {
	DeployService(ctx context.Context, node *entity.Node, svc *entity.Service, artifact io.Reader) error
	ConfigureIngress(ctx context.Context, node *entity.Node, ingress *entity.IngressConfig) error
	GetStatus(ctx context.Context, node *entity.Node) (*entity.Node, []*entity.Service, error)
	StreamLogs(ctx context.Context, node *entity.Node, serviceName string, writer io.Writer) error
}

// StrategyResolver resolves the deployment strategy for a given target host (Factory Pattern)
type StrategyResolver interface {
	GetStrategy(target *entity.TargetHost) (domain.TargetDeploymentStrategy, error)
}

// DeployUseCase orchestrates building, packaging, deploying, and scale-out sync across nodes
type DeployUseCase struct {
	nodeRepo         domain.NodeRepository
	client           NodeClient
	strategyResolver StrategyResolver
	ingressMgr       domain.IngressManager
	releaseDir       string
}

// NewDeployUseCase initializes DeployUseCase
func NewDeployUseCase(repo domain.NodeRepository, client NodeClient) *DeployUseCase {
	return &DeployUseCase{
		nodeRepo: repo,
		client:   client,
	}
}

// WithStrategyResolver injects a custom strategy resolver (Factory Pattern)
func (uc *DeployUseCase) WithStrategyResolver(resolver StrategyResolver) *DeployUseCase {
	uc.strategyResolver = resolver
	return uc
}

// WithIngressManager injects the ingress / load balancer controller
func (uc *DeployUseCase) WithIngressManager(mgr domain.IngressManager) *DeployUseCase {
	uc.ingressMgr = mgr
	return uc
}

// WithReleaseDirectory configures where successful deployment history is persisted.
func (uc *DeployUseCase) WithReleaseDirectory(dir string) *DeployUseCase {
	uc.releaseDir = dir
	return uc
}

func projectTargets(project *entity.Project) []string {
	if len(project.Targets) > 0 {
		return append([]string(nil), project.Targets...)
	}
	if project.Target != "" && project.Target != "default" {
		return []string{project.Target}
	}
	return []string{"localhost"}
}

func serviceTargets(project *entity.Project, svc *entity.Service) []string {
	if svc.Target != "" {
		return []string{svc.Target}
	}
	return projectTargets(project)
}

// Execute deploys one or all services from a project configuration to single or multiple targets
func (uc *DeployUseCase) Execute(ctx context.Context, project *entity.Project, targetService string, logWriter io.Writer) error {
	if logWriter == nil {
		logWriter = os.Stdout
	}

	// 2. Select services to deploy
	var toDeploy []*entity.Service
	if targetService != "" {
		svc, ok := project.Services[targetService]
		if !ok {
			return fmt.Errorf("%w: '%s'", domain.ErrServiceNotFound, targetService)
		}
		toDeploy = append(toDeploy, svc)
	} else {
		names := make([]string, 0, len(project.Services))
		for name := range project.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			toDeploy = append(toDeploy, project.Services[name])
		}
	}

	for _, svc := range toDeploy {
		targets := serviceTargets(project, svc)
		// A. Local build step if specified
		if svc.Build != nil && svc.Build.Local != "" {
			_, _ = fmt.Fprintln(logWriter, i18n.T("build_running", svc.Build.Local))
			if err := uc.runLocalBuild(ctx, svc, logWriter); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrBuildFailed, err)
			}
			_, _ = fmt.Fprintln(logWriter, i18n.T("build_success"))
		}

		// B. Package artifacts into memory bytes so multiple targets can consume it
		var artifactBytes []byte
		if svc.Build != nil && svc.Build.Artifact != "" {
			artifactPath := svc.Build.Artifact
			if !filepath.IsAbs(artifactPath) {
				artifactPath = filepath.Join(svc.Root, artifactPath)
			}
			buf, err := createTarGzArchive(artifactPath)
			if err != nil {
				return fmt.Errorf("failed to package artifact at %s: %w", artifactPath, err)
			}
			artifactBytes = buf.Bytes()
		} else if (svc.Type == entity.TypeDocker && svc.Image == "") || svc.Type == entity.TypeCompose || svc.Dockerfile != "" ||
			svc.Type == entity.TypeNode || svc.Type == entity.TypeBun || svc.Type == entity.TypeProcess {
			srcDir := svc.Root
			if srcDir == "" {
				srcDir = "."
			}
			buf, err := createTarGzArchive(srcDir)
			if err != nil {
				return fmt.Errorf("failed to package context directory at %s: %w", srcDir, err)
			}
			artifactBytes = buf.Bytes()
		}

		// C. Multi-target scale-out vs single-target execution
		if len(targets) > 1 {
			if err := uc.deployToMultipleTargets(ctx, svc, targets, artifactBytes, logWriter); err != nil {
				return err
			}
		} else {
			if err := uc.deployToSingleTarget(ctx, svc, targets[0], artifactBytes, logWriter); err != nil {
				return err
			}
		}

		// D. Record release revision for rollback support
		revID := fmt.Sprintf("rev-%d", time.Now().UnixNano())
		var upstreams []string
		if svc.Ingress != nil {
			upstreams = svc.Ingress.Upstreams
		}
		err := recordProjectRelease(uc.releaseDir, project, svc, &entity.ReleaseRecord{
			Revision:    revID,
			ServiceName: svc.Name,
			Image:       svc.Image,
			Ports:       svc.Ports,
			Replicas:    svc.Replicas,
			Upstreams:   upstreams,
			CreatedAt:   time.Now(),
		})
		if err != nil {
			return fmt.Errorf("deployment completed but release history could not be saved: %w", err)
		}

		_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_success", svc.Name, revID))
	}

	return nil
}

func (uc *DeployUseCase) deployToSingleTarget(ctx context.Context, svc *entity.Service, targetStr string, artifactBytes []byte, logWriter io.Writer) error {
	targetHost := entity.ParseTargetHost(targetStr)
	_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_starting", svc.Name, svc.Type, targetHost.Address()))

	var artifactReader io.Reader
	if len(artifactBytes) > 0 {
		artifactReader = bytes.NewReader(artifactBytes)
	}

	// 1. If strategy resolver is provided, use strategy pattern
	if uc.strategyResolver != nil {
		strategy, err := uc.strategyResolver.GetStrategy(targetHost)
		if err != nil {
			return fmt.Errorf("resolve deployment strategy: %w", err)
		}
		if strategy == nil {
			return fmt.Errorf("no deployment strategy for %s", targetStr)
		}
		{
			result, err := strategy.Deploy(ctx, targetHost, svc, artifactReader)
			if err != nil {
				_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_failed", svc.Name, err))
				return err
			}
			if result == nil {
				return fmt.Errorf("deployment returned no result for %s", targetStr)
			}
			if !result.Success {
				return fmt.Errorf("deploy failed on %s: %s", targetHost.Address(), result.Error)
			}
			// Configure Ingress if enabled
			return uc.configureIngressForTarget(ctx, svc, targetHost, logWriter)
		}
	}

	// Only mesh targets may use the mesh client.
	if targetHost.Type != entity.TargetTypeMesh {
		return fmt.Errorf("deployment strategy unavailable for target %s", targetStr)
	}
	node, err := uc.resolveNode(targetHost.Host)
	if err != nil {
		return err
	}

	if uc.client == nil {
		return fmt.Errorf("no deployment client for target %s", targetStr)
	}
	{
		if err := uc.client.DeployService(ctx, node, svc, artifactReader); err != nil {
			_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_failed", svc.Name, err))
			return err
		}

		if svc.Ingress != nil && svc.Ingress.Provider == "caddy" && svc.Ingress.Domain != "" {
			if err := uc.client.ConfigureIngress(ctx, node, svc.Ingress); err != nil {
				return fmt.Errorf("application deployed but ingress setup failed: %w", err)
			} else {
				_, _ = fmt.Fprintln(logWriter, i18n.T("ingress_configured", svc.Ingress.Domain, svc.Ingress.UpstreamPort))
			}
		}
	}

	return nil
}

func (uc *DeployUseCase) deployToMultipleTargets(ctx context.Context, svc *entity.Service, targets []string, artifactBytes []byte, logWriter io.Writer) error {
	_, _ = fmt.Fprintln(logWriter, i18n.T("scale_sync_starting", len(targets)))

	type outcome struct {
		targetHost *entity.TargetHost
		result     *entity.DeployResult
		service    *entity.Service
		err        error
	}

	outcomes := make([]outcome, len(targets))
	var wg sync.WaitGroup

	for i, tStr := range targets {
		wg.Add(1)
		go func(idx int, targetStr string) {
			defer wg.Done()
			tHost := entity.ParseTargetHost(targetStr)
			var r io.Reader
			if len(artifactBytes) > 0 {
				r = bytes.NewReader(artifactBytes)
			}

			if uc.strategyResolver != nil {
				strategy, err := uc.strategyResolver.GetStrategy(tHost)
				if err != nil {
					outcomes[idx] = outcome{targetHost: tHost, err: err}
					return
				}
				if strategy == nil {
					outcomes[idx] = outcome{targetHost: tHost, err: fmt.Errorf("no deployment strategy")}
					return
				}
				// Each target receives an independent configuration snapshot.
				targetSvc, err := cloneService(svc)
				if err != nil {
					outcomes[idx] = outcome{targetHost: tHost, err: err}
					return
				}
				res, err := strategy.Deploy(ctx, tHost, targetSvc, r)
				if res == nil && err == nil {
					err = fmt.Errorf("deployment returned no result")
				}
				outcomes[idx] = outcome{targetHost: tHost, result: res, service: targetSvc, err: err}
				return
			}

			if tHost.Type != entity.TargetTypeMesh {
				outcomes[idx] = outcome{targetHost: tHost, err: fmt.Errorf("deployment strategy unavailable")}
				return
			}
			// Fallback: Mesh RPC
			node, err := uc.resolveNode(tHost.Host)
			if err != nil {
				outcomes[idx] = outcome{targetHost: tHost, err: err}
				return
			}
			if uc.client == nil {
				outcomes[idx] = outcome{targetHost: tHost, err: fmt.Errorf("no deployment client")}
				return
			}
			targetSvc, err := cloneService(svc)
			if err != nil {
				outcomes[idx] = outcome{targetHost: tHost, err: err}
				return
			}
			start := time.Now()
			err = uc.client.DeployService(ctx, node, targetSvc, r)
			outcomes[idx] = outcome{
				targetHost: tHost,
				service:    targetSvc,
				result:     &entity.DeployResult{Target: tHost.Address(), ServiceName: svc.Name, Success: err == nil, Duration: time.Since(start)},
				err:        err,
			}
		}(i, tStr)
	}

	wg.Wait()

	var successfulUpstreams []string
	var failures []error

	for _, oc := range outcomes {
		if oc.err != nil || (oc.result != nil && !oc.result.Success) {

			errMsg := oc.err
			if errMsg == nil && oc.result != nil {
				errMsg = fmt.Errorf("%s", oc.result.Error)
			}
			failures = append(failures, fmt.Errorf("%s: %w", oc.targetHost.Address(), errMsg))
			_, _ = fmt.Fprintln(logWriter, i18n.T("scale_target_failed", oc.targetHost.Address(), errMsg))
		} else {
			durationStr := "done"
			if oc.result != nil {
				durationStr = oc.result.Duration.Round(time.Millisecond).String()
			}
			_, _ = fmt.Fprintln(logWriter, i18n.T("scale_target_success", oc.targetHost.Address(), durationStr))

			// Collect upstream address:port for load balancer
			upstreamPort := 80
			if svc.Ingress != nil && svc.Ingress.UpstreamPort > 0 {
				upstreamPort = svc.Ingress.UpstreamPort
			} else if len(svc.Ports) > 0 {
				parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(svc.Ports[0], "/tcp"), "/udp"), ":")
				if len(parts) >= 2 {
					_, _ = fmt.Sscanf(parts[len(parts)-2], "%d", &upstreamPort)
				}
			}

			upstreamAddr := net.JoinHostPort(oc.targetHost.Host, fmt.Sprint(upstreamPort))
			if oc.targetHost.Type == entity.TargetTypeLocal {
				upstreamAddr = fmt.Sprintf("127.0.0.1:%d", upstreamPort)
			}
			successfulUpstreams = append(successfulUpstreams, upstreamAddr)
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d of %d target deployments failed (successful targets remain deployed): %w", len(failures), len(targets), errors.Join(failures...))
	}
	// A shared immutable reference is safe to reuse across these targets.
	// Different local build IDs must not be treated as a reproducible revision.
	image := ""
	if len(outcomes) > 0 && outcomes[0].service != nil {
		image = outcomes[0].service.Image
	}
	for _, oc := range outcomes {
		if oc.service == nil || oc.service.Image != image {
			image = ""
			break
		}
	}
	if image != "" {
		svc.Image = image
	}

	// D. Synchronize Ingress Load Balancing across all successful targets
	if svc.Ingress != nil && svc.Ingress.Provider == "caddy" && svc.Ingress.Domain != "" {
		svc.Ingress.Upstreams = successfulUpstreams
		if svc.Ingress.LBPolicy == "" {
			svc.Ingress.LBPolicy = "round_robin"
		}

		controllerConfigured := false
		for _, oc := range outcomes {
			if oc.targetHost.Type == entity.TargetTypeMesh {
				if err := uc.configureIngressForTarget(ctx, svc, oc.targetHost, logWriter); err != nil {
					return err
				}
			} else if !controllerConfigured {
				if uc.ingressMgr == nil {
					return fmt.Errorf("application deployed but ingress manager is unavailable")
				}
				if err := uc.ingressMgr.ConfigureRoute(ctx, svc.Ingress); err != nil {
					return fmt.Errorf("application deployed but load balancer configuration failed: %w", err)
				}
				controllerConfigured = true
			}
		}
		_, _ = fmt.Fprintln(logWriter, i18n.T("ingress_lb_configured", svc.Ingress.Domain, len(successfulUpstreams), svc.Ingress.LBPolicy))
	}

	return nil
}

func (uc *DeployUseCase) configureIngressForTarget(ctx context.Context, svc *entity.Service, target *entity.TargetHost, logWriter io.Writer) error {
	if svc.Ingress == nil || svc.Ingress.Provider != "caddy" || svc.Ingress.Domain == "" {
		return nil
	}

	if target.Type == entity.TargetTypeMesh {
		if uc.client == nil {
			return fmt.Errorf("application deployed but mesh ingress client is unavailable")
		}
		node, err := uc.resolveNode(target.Host)
		if err != nil {
			return err
		}
		if err := uc.client.ConfigureIngress(ctx, node, svc.Ingress); err != nil {
			return fmt.Errorf("application deployed but mesh ingress setup failed: %w", err)
		}
		return nil
	}

	if target.Type == entity.TargetTypeSSH && target.Host != "" && len(svc.Ingress.Upstreams) == 0 {
		svc.Ingress.Upstreams = []string{net.JoinHostPort(target.Host, fmt.Sprint(svc.Ingress.UpstreamPort))}
	}

	if uc.ingressMgr == nil {
		return fmt.Errorf("application deployed but ingress manager is unavailable")
	}
	if err := uc.ingressMgr.ConfigureRoute(ctx, svc.Ingress); err != nil {
		return fmt.Errorf("application deployed but ingress setup failed: %w", err)
	}
	_, _ = fmt.Fprintln(logWriter, i18n.T("ingress_configured", svc.Ingress.Domain, svc.Ingress.UpstreamPort))
	return nil
}

func (uc *DeployUseCase) resolveNode(name string) (*entity.Node, error) {
	if uc.nodeRepo == nil {
		return nil, fmt.Errorf("node repository unavailable for target %q", name)
	}
	node, err := uc.nodeRepo.GetNode(name)
	if err != nil {
		return nil, fmt.Errorf("target node %q not found: %w", name, err)
	}
	if node == nil {
		return nil, fmt.Errorf("target node %q not found", name)
	}
	return node, nil
}

func (uc *DeployUseCase) runLocalBuild(ctx context.Context, svc *entity.Service, logWriter io.Writer) error {
	cmdParts := []string{"sh", "-c", svc.Build.Local}
	cmd := exec.CommandContext(ctx, cmdParts[0], cmdParts[1:]...)
	if svc.Root != "" {
		cmd.Dir = svc.Root
	}
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	return cmd.Run()
}

func createTarGzArchive(srcPath string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	info, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}

	baseDir := srcPath
	if !info.IsDir() {
		baseDir = filepath.Dir(srcPath)
	}

	matcher := ignore.NewMatcher(baseDir)

	err = filepath.Walk(srcPath, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		if rel != "." && matcher.ShouldIgnore(rel, f.IsDir()) {
			if f.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		link := ""
		if f.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(f, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !f.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		return errors.Join(copyErr, file.Close())
	})

	err = errors.Join(err, tw.Close(), gw.Close())
	return &buf, err
}
