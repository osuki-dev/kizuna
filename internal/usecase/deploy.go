package usecase

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// Execute deploys one or all services from a project configuration to single or multiple targets
func (uc *DeployUseCase) Execute(ctx context.Context, project *entity.Project, targetService string, logWriter io.Writer) error {
	if logWriter == nil {
		logWriter = os.Stdout
	}

	// 1. Resolve targets (single or multi-node scale-out cluster)
	targets := project.Targets
	if len(targets) == 0 {
		if project.Target != "" && project.Target != "default" {
			targets = []string{project.Target}
		} else {
			targets = []string{"localhost"}
		}
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
		for _, svc := range project.Services {
			toDeploy = append(toDeploy, svc)
		}
	}

	for _, svc := range toDeploy {
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
		} else if (svc.Type == entity.TypeDocker && svc.Image == "") || svc.Type == entity.TypeCompose || svc.Dockerfile != "" {
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
		revID := fmt.Sprintf("rev-%d", time.Now().Unix())
		var upstreams []string
		if svc.Ingress != nil {
			upstreams = svc.Ingress.Upstreams
		}
		_ = RecordRelease("", &entity.ReleaseRecord{
			Revision:    revID,
			ServiceName: svc.Name,
			Image:       svc.Image,
			Ports:       svc.Ports,
			Replicas:    svc.Replicas,
			Upstreams:   upstreams,
			CreatedAt:   time.Now(),
		})

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
		if err == nil && strategy != nil {
			result, err := strategy.Deploy(ctx, targetHost, svc, artifactReader)
			if err != nil {
				_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_failed", svc.Name, err))
				return err
			}
			if result != nil && !result.Success {
				return fmt.Errorf("deploy failed on %s: %s", targetHost.Address(), result.Error)
			}
			// Configure Ingress if enabled
			uc.configureIngressForTarget(ctx, svc, targetHost, logWriter)
			return nil
		}
	}

	// 2. Fallback to mesh client if available
	node, err := uc.resolveNode(targetHost.Host)
	if err != nil {
		return err
	}

	if uc.client != nil {
		if err := uc.client.DeployService(ctx, node, svc, artifactReader); err != nil {
			_, _ = fmt.Fprintln(logWriter, i18n.T("deploy_failed", svc.Name, err))
			return err
		}

		if svc.Ingress != nil && svc.Ingress.Provider == "caddy" && svc.Ingress.Domain != "" {
			if err := uc.client.ConfigureIngress(ctx, node, svc.Ingress); err != nil {
				_, _ = fmt.Fprintf(logWriter, "[warning] Ingress setup failed: %v\n", err)
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
				res, err := strategy.Deploy(ctx, tHost, svc, r)
				outcomes[idx] = outcome{targetHost: tHost, result: res, err: err}
				return
			}

			// Fallback: Mesh RPC
			node, err := uc.resolveNode(tHost.Host)
			if err != nil {
				outcomes[idx] = outcome{targetHost: tHost, err: err}
				return
			}
			start := time.Now()
			err = uc.client.DeployService(ctx, node, svc, r)
			outcomes[idx] = outcome{
				targetHost: tHost,
				result:     &entity.DeployResult{Target: tHost.Address(), ServiceName: svc.Name, Success: err == nil, Duration: time.Since(start)},
				err:        err,
			}
		}(i, tStr)
	}

	wg.Wait()

	var successfulUpstreams []string
	var failCount int

	for _, oc := range outcomes {
		if oc.err != nil || (oc.result != nil && !oc.result.Success) {
			failCount++
			errMsg := oc.err
			if errMsg == nil && oc.result != nil {
				errMsg = fmt.Errorf("%s", oc.result.Error)
			}
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
				// Parse host port
				p := svc.Ports[0]
				var hostPort string
				if idx := filepath.Clean(p); idx != "" {
					parts := bytes.Split([]byte(p), []byte(":"))
					if len(parts) >= 2 {
						hostPort = string(parts[0])
					}
				}
				if hostPort != "" {
					_, _ = fmt.Sscanf(hostPort, "%d", &upstreamPort)
				}
			}

			upstreamAddr := fmt.Sprintf("%s:%d", oc.targetHost.Host, upstreamPort)
			if oc.targetHost.Type == entity.TargetTypeLocal {
				upstreamAddr = fmt.Sprintf("127.0.0.1:%d", upstreamPort)
			}
			successfulUpstreams = append(successfulUpstreams, upstreamAddr)
		}
	}

	if len(successfulUpstreams) == 0 {
		return fmt.Errorf("all %d target deployment(s) failed", len(targets))
	}

	// D. Synchronize Ingress Load Balancing across all successful targets
	if svc.Ingress != nil && svc.Ingress.Provider == "caddy" && svc.Ingress.Domain != "" {
		svc.Ingress.Upstreams = successfulUpstreams
		if svc.Ingress.LBPolicy == "" {
			svc.Ingress.LBPolicy = "round_robin"
		}

		if uc.ingressMgr != nil {
			if err := uc.ingressMgr.ConfigureRoute(ctx, svc.Ingress); err != nil {
				_, _ = fmt.Fprintf(logWriter, "[warning] Load balancer configuration failed: %v\n", err)
			} else {
				_, _ = fmt.Fprintln(logWriter, i18n.T("ingress_lb_configured", svc.Ingress.Domain, len(successfulUpstreams), svc.Ingress.LBPolicy))
			}
		}
	}

	return nil
}

func (uc *DeployUseCase) configureIngressForTarget(ctx context.Context, svc *entity.Service, target *entity.TargetHost, logWriter io.Writer) {
	if svc.Ingress == nil || svc.Ingress.Provider != "caddy" || svc.Ingress.Domain == "" {
		return
	}

	if target.Type == entity.TargetTypeSSH && target.Host != "" {
		svc.Ingress.Upstreams = []string{fmt.Sprintf("%s:%d", target.Host, svc.Ingress.UpstreamPort)}
	}

	if uc.ingressMgr != nil {
		if err := uc.ingressMgr.ConfigureRoute(ctx, svc.Ingress); err != nil {
			_, _ = fmt.Fprintf(logWriter, "[warning] Ingress setup failed: %v\n", err)
		} else {
			_, _ = fmt.Fprintln(logWriter, i18n.T("ingress_configured", svc.Ingress.Domain, svc.Ingress.UpstreamPort))
		}
	}
}

func (uc *DeployUseCase) resolveNode(name string) (*entity.Node, error) {
	if uc.nodeRepo != nil {
		node, err := uc.nodeRepo.GetNode(name)
		if err == nil {
			return node, nil
		}
		nodes, _ := uc.nodeRepo.ListNodes()
		if len(nodes) > 0 {
			return nodes[0], nil
		}
	}

	return &entity.Node{
		Name:      name,
		Addr:      name,
		AuthToken: "kzn_default",
		IsOnline:  true,
	}, nil
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
		header, err := tar.FileInfoHeader(f, f.Name())
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if f.IsDir() {
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

	_ = tw.Close()
	_ = gw.Close()
	return &buf, err
}
