package usecase

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

type testStrategy struct {
	deploy func(*entity.TargetHost, *entity.Service) error
}

func (s testStrategy) Protocol() entity.TargetType { return entity.TargetTypeSSH }
func (s testStrategy) Deploy(_ context.Context, target *entity.TargetHost, svc *entity.Service, _ io.Reader) (*entity.DeployResult, error) {
	err := s.deploy(target, svc)
	return &entity.DeployResult{Success: err == nil}, err
}
func (s testStrategy) Stop(context.Context, *entity.TargetHost, string) error { return nil }
func (s testStrategy) GetStatus(context.Context, *entity.TargetHost, string) (*entity.Service, error) {
	return nil, nil
}

type testResolver struct {
	strategy domain.TargetDeploymentStrategy
}

func (r testResolver) GetStrategy(*entity.TargetHost) (domain.TargetDeploymentStrategy, error) {
	return r.strategy, nil
}

type failedIngress struct{}

func (failedIngress) ConfigureRoute(context.Context, *entity.IngressConfig) error {
	return errors.New("reload failed")
}
func (failedIngress) RemoveRoute(context.Context, string) error { return nil }
func (failedIngress) Reload(context.Context) error              { return nil }

type missingRepo struct{}

func (missingRepo) GetNode(string) (*entity.Node, error) { return nil, errors.New("missing") }
func (missingRepo) ListNodes() ([]*entity.Node, error)   { return []*entity.Node{{Name: "wrong"}}, nil }
func (missingRepo) SaveNode(*entity.Node) error          { return nil }
func (missingRepo) DeleteNode(string) error              { return nil }

func testProject() *entity.Project {
	return &entity.Project{Name: "app", ActiveEnv: "production", Targets: []string{"localhost"}, Services: map[string]*entity.Service{"web": {Name: "web", Type: entity.TypeDocker, Image: "sha256:aaa"}}}
}
func TestMissingNodeNeverSelectsFirst(t *testing.T) {
	uc := NewDeployUseCase(missingRepo{}, nil)
	if node, err := uc.resolveNode("typo"); err == nil || node != nil {
		t.Fatalf("got node %v error %v", node, err)
	}
}
func TestDeploymentFailureNeverRecordsSuccess(t *testing.T) {
	for _, kind := range []string{"ingress", "partial targets", "missing strategy", "history write"} {
		t.Run(kind, func(t *testing.T) {
			project := testProject()
			dir := t.TempDir()
			uc := NewDeployUseCase(nil, nil).WithReleaseDirectory(dir).WithStrategyResolver(testResolver{testStrategy{func(target *entity.TargetHost, _ *entity.Service) error {
				if target.Host == "bad" {
					return errors.New("unreachable")
				}
				return nil
			}}})
			switch kind {
			case "ingress":
				project.Services["web"].Ingress = &entity.IngressConfig{Provider: "caddy", Domain: "app.example", UpstreamPort: 80}
				uc.WithIngressManager(failedIngress{})
			case "partial targets":
				project.Targets = []string{"localhost", "ssh://bad"}
			case "missing strategy":
				uc.WithStrategyResolver(testResolver{})
			case "history write":
				file := filepath.Join(dir, "file")
				if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				uc.WithReleaseDirectory(file)
			}
			if err := uc.Execute(context.Background(), project, "web", io.Discard); err == nil {
				t.Fatal("expected non-success result")
			}
			if history, err := readHistory(dir, projectReleaseKey(project, project.Services["web"])); err == nil || len(history) > 0 {
				t.Fatalf("unexpected successful history: %v %v", history, err)
			}
		})
	}
}
func TestReleaseScopesDoNotOverlap(t *testing.T) {
	project := testProject()
	base := t.TempDir()
	svc := project.Services["web"]
	if err := recordProjectRelease(base, project, svc, &entity.ReleaseRecord{Revision: "one", ServiceName: svc.Name}); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"project", "environment", "target"} {
		copy := *project
		switch variant {
		case "project":
			copy.Name = "other"
		case "environment":
			copy.ActiveEnv = "staging"
		case "target":
			copy.Targets = []string{"ssh://other"}
		}
		if _, err := readHistory(base, projectReleaseKey(&copy, svc)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s history leaked: %v", variant, err)
		}
	}
	path, err := historyPath(base, projectReleaseKey(project, svc))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("secret-bearing history mode %o", stat.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recordProjectRelease(base, project, svc, &entity.ReleaseRecord{Revision: "two"}); err == nil {
		t.Fatal("corrupt history was silently overwritten")
	}
}
func TestRollbackRejectsMutableImagesBeforeDeploy(t *testing.T) {
	project := testProject()
	base := t.TempDir()
	svc := project.Services["web"]
	svc.Image = "app:latest"
	for _, rev := range []string{"one", "two"} {
		if err := recordProjectRelease(base, project, svc, &entity.ReleaseRecord{Revision: rev, ServiceName: "web"}); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	uc := NewDeployUseCase(nil, nil).WithStrategyResolver(testResolver{testStrategy{func(*entity.TargetHost, *entity.Service) error { called = true; return nil }}})
	_, err := NewRollbackUseCase(uc, base).Execute(context.Background(), project, "web", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "immutable") || called {
		t.Fatalf("unsafe rollback: called=%v err=%v", called, err)
	}
}
func TestServiceTargetAndDeploymentOrder(t *testing.T) {
	project := testProject()
	project.Services["a"] = &entity.Service{Name: "a", Type: entity.TypeDocker, Image: "sha256:aaa", Target: "ssh://dedicated"}
	var calls []string
	uc := NewDeployUseCase(nil, nil).WithReleaseDirectory(t.TempDir()).WithStrategyResolver(testResolver{testStrategy{func(target *entity.TargetHost, svc *entity.Service) error {
		calls = append(calls, svc.Name+"@"+target.Host)
		return nil
	}}})
	if err := uc.Execute(context.Background(), project, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "a@dedicated,web@127.0.0.1" {
		t.Fatalf("wrong order/targets: %v", calls)
	}
}

func TestMultiTargetsUseIndependentSnapshots(t *testing.T) {
	project := testProject()
	project.Targets = []string{"ssh://one", "ssh://two"}
	project.Services["web"].Env = map[string]string{"SHARED": "original"}
	var mu sync.Mutex
	snapshots := map[string]*entity.Service{}
	uc := NewDeployUseCase(nil, nil).WithReleaseDirectory(t.TempDir()).WithStrategyResolver(testResolver{testStrategy{func(target *entity.TargetHost, svc *entity.Service) error {
		svc.Env["SHARED"] = target.Host
		svc.Image = "sha256:resolved"
		mu.Lock()
		snapshots[target.Host] = svc
		mu.Unlock()
		return nil
	}}})
	if err := uc.Execute(context.Background(), project, "web", io.Discard); err != nil {
		t.Fatal(err)
	}
	if snapshots["one"] == snapshots["two"] || snapshots["one"].Env["SHARED"] != "one" || snapshots["two"].Env["SHARED"] != "two" {
		t.Fatal("targets shared deployment state")
	}
	if project.Services["web"].Env["SHARED"] != "original" || project.Services["web"].Image != "sha256:resolved" {
		t.Fatalf("wrong source configuration: %#v", project.Services["web"])
	}
}

type artifactCaptureStrategy struct {
	testStrategy
	files map[string]string
}

func (s *artifactCaptureStrategy) Deploy(_ context.Context, _ *entity.TargetHost, _ *entity.Service, artifact io.Reader) (*entity.DeployResult, error) {
	if artifact == nil {
		return nil, errors.New("source artifact is missing")
	}
	gz, err := gzip.NewReader(artifact)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	s.files = map[string]string{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		s.files[header.Name] = string(data)
	}
	return &entity.DeployResult{Success: true}, nil
}
func TestProcessWorkloadsPackageSourcesWithoutBuildArtifacts(t *testing.T) {
	for _, kind := range []entity.ServiceType{entity.TypeNode, entity.TypeBun, entity.TypeProcess} {
		for _, target := range []string{"localhost", "ssh://remote"} {
			t.Run(string(kind)+"/"+target, func(t *testing.T) {
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "index.js"), []byte("console.log('hello')"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("private"), 0600); err != nil {
					t.Fatal(err)
				}
				strategy := &artifactCaptureStrategy{}
				project := testProject()
				project.Targets = []string{target}
				svc := project.Services["web"]
				svc.Type = kind
				svc.Image = ""
				svc.Root = root
				uc := NewDeployUseCase(nil, nil).WithReleaseDirectory(t.TempDir()).WithStrategyResolver(testResolver{strategy})
				if err := uc.Execute(context.Background(), project, "web", io.Discard); err != nil {
					t.Fatal(err)
				}
				if strategy.files["index.js"] != "console.log('hello')" {
					t.Fatalf("missing process source: %v", strategy.files)
				}
				if _, ok := strategy.files[".git/config"]; ok {
					t.Fatal("ignored source metadata packaged")
				}
			})
		}
	}
}

func TestRollbackRejectsIngressIdentityChanges(t *testing.T) {
	for _, kind := range []string{"removed", "changed domain"} {
		t.Run(kind, func(t *testing.T) {
			project := testProject()
			svc := project.Services["web"]
			base := t.TempDir()
			if kind == "changed domain" {
				svc.Ingress = &entity.IngressConfig{Provider: "caddy", Domain: "old.example", UpstreamPort: 80}
			}
			if err := recordProjectRelease(base, project, svc, &entity.ReleaseRecord{Revision: "one", ServiceName: "web"}); err != nil {
				t.Fatal(err)
			}
			svc.Ingress = &entity.IngressConfig{Provider: "caddy", Domain: "current.example", UpstreamPort: 80}
			if err := recordProjectRelease(base, project, svc, &entity.ReleaseRecord{Revision: "two", ServiceName: "web"}); err != nil {
				t.Fatal(err)
			}
			called := false
			uc := NewDeployUseCase(nil, nil).WithStrategyResolver(testResolver{testStrategy{func(*entity.TargetHost, *entity.Service) error { called = true; return nil }}})
			_, err := NewRollbackUseCase(uc, base).Execute(context.Background(), project, "web", io.Discard)
			if err == nil || !strings.Contains(err.Error(), "ingress") || called {
				t.Fatalf("unsafe ingress rollback: %v called=%v", err, called)
			}
		})
	}
}

type recordingIngress struct{ upstreams []string }

func (m *recordingIngress) ConfigureRoute(_ context.Context, config *entity.IngressConfig) error {
	m.upstreams = append([]string(nil), config.Upstreams...)
	return nil
}
func (m *recordingIngress) RemoveRoute(context.Context, string) error { return nil }
func (m *recordingIngress) Reload(context.Context) error              { return nil }
func TestSSHDeploymentPreservesExplicitUpstreams(t *testing.T) {
	project := testProject()
	project.Targets = []string{"ssh://server"}
	explicit := []string{"proxy.internal:9000", "proxy.internal:9001"}
	project.Services["web"].Ingress = &entity.IngressConfig{Provider: "caddy", Domain: "app.example", UpstreamPort: 8080, Upstreams: explicit}
	ingress := &recordingIngress{}
	uc := NewDeployUseCase(nil, nil).WithReleaseDirectory(t.TempDir()).WithStrategyResolver(testResolver{testStrategy{func(*entity.TargetHost, *entity.Service) error { return nil }}}).WithIngressManager(ingress)
	if err := uc.Execute(context.Background(), project, "web", io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ingress.upstreams, ",") != strings.Join(explicit, ",") {
		t.Fatalf("explicit upstreams overwritten: %v", ingress.upstreams)
	}
}
