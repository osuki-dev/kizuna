package config_test

import (
	"os"
	"testing"

	"github.com/osuki-dev/kizuna/internal/adapter/config"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func TestLoadMonorepoConfig(t *testing.T) {
	proj, err := config.LoadProject("../../../examples/monorepo/kizuna.yaml")
	if err != nil {
		t.Fatalf("failed to load monorepo config: %v", err)
	}

	if proj.Name != "fullstack-monorepo" {
		t.Errorf("expected project name 'fullstack-monorepo', got '%s'", proj.Name)
	}

	if proj.Theme != "tokyonight" {
		t.Errorf("expected theme 'tokyonight', got '%s'", proj.Theme)
	}

	if len(proj.Services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(proj.Services))
	}

	web := proj.Services["web"]
	if web == nil || web.Type != entity.TypeBun {
		t.Errorf("expected web service with bun type, got %+v", web)
	}
	if web.Ingress == nil || web.Ingress.Domain != "app.example.com" {
		t.Errorf("unexpected ingress for web: %+v", web.Ingress)
	}

	api := proj.Services["api"]
	if api == nil || api.Type != entity.TypeDocker || api.Dockerfile != "Dockerfile" {
		t.Errorf("unexpected api service: %+v", api)
	}

	db := proj.Services["infrastructure"]
	if db == nil || db.Type != entity.TypeCompose {
		t.Errorf("unexpected compose service: %+v", db)
	}
}

func TestLoadSingleDockerConfig(t *testing.T) {
	proj, err := config.LoadProject("../../../examples/single-docker/kizuna.yaml")
	if err != nil {
		t.Fatalf("failed to load single docker config: %v", err)
	}

	if len(proj.Services) != 1 {
		t.Fatalf("expected 1 normalized service, got %d", len(proj.Services))
	}

	svc := proj.Services["my-docker-site"]
	if svc == nil || svc.Type != entity.TypeDocker {
		t.Errorf("expected docker service, got %+v", svc)
	}
}

func TestValidateProject(t *testing.T) {
	invalidProj := &entity.Project{
		Name: "",
		Services: map[string]*entity.Service{
			"bad-svc": {
				Type: "unsupported-type",
				Ports: []string{"invalid-port-format"},
				Ingress: &entity.IngressConfig{
					Provider: "caddy",
					Domain: "",
					UpstreamPort: -1,
				},
			},
		},
	}

	issues := config.ValidateProject(invalidProj, ".")
	if len(issues) == 0 {
		t.Fatalf("expected validation issues, got 0")
	}

	hasTypeError := false
	hasPortError := false
	hasDomainError := false
	for _, issue := range issues {
		if issue.Field == "type" && issue.Severity == config.SeverityError {
			hasTypeError = true
		}
		if issue.Field == "ports" && issue.Severity == config.SeverityError {
			hasPortError = true
		}
		if issue.Field == "ingress.domain" && issue.Severity == config.SeverityError {
			hasDomainError = true
		}
	}

	if !hasTypeError || !hasPortError || !hasDomainError {
		t.Errorf("expected type, port, and domain errors, got issues: %+v", issues)
	}
}

func TestMultiEnvironmentConfig(t *testing.T) {
	yamlContent := `
version: "1"
name: "multi-env-app"
target: "prod-server"
theme: "tokyonight"

type: "docker"
dockerfile: "Dockerfile"
ports:
  - "3000:3000"
env:
  NODE_ENV: "production"
  DATABASE_URL: "postgres://prod-db/app"
ingress:
  provider: "caddy"
  domain: "app.example.com"
  upstream_port: 3000

environments:
  development:
    target: "localhost"
    ports:
      - "3001:3000"
    env:
      NODE_ENV: "development"
      DATABASE_URL: "postgres://localhost/dev-db"
    ingress:
      domain: "dev.example.local"
      upstream_port: 3001

  staging:
    target: "staging-server"
    ports:
      - "3002:3000"
    ingress:
      domain: "staging.example.com"
      upstream_port: 3002
`
	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/kizuna.yaml"
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write tmp config: %v", err)
	}

	// 1. Default (production / base)
	baseProj, err := config.LoadProject(cfgPath)
	if err != nil {
		t.Fatalf("failed to load base config: %v", err)
	}
	if baseProj.Target != "prod-server" {
		t.Errorf("expected target 'prod-server', got '%s'", baseProj.Target)
	}
	if len(baseProj.Environments) != 2 {
		t.Errorf("expected 2 environments in list, got %d", len(baseProj.Environments))
	}

	// 2. Development environment override
	devProj, err := config.LoadProjectWithEnv(cfgPath, "development")
	if err != nil {
		t.Fatalf("failed to load dev config: %v", err)
	}
	if devProj.Target != "localhost" {
		t.Errorf("expected dev target 'localhost', got '%s'", devProj.Target)
	}
	devSvc := devProj.Services["multi-env-app"]
	if devSvc == nil {
		t.Fatalf("service multi-env-app not found in dev")
	}
	if devSvc.Ports[0] != "3001:3000" {
		t.Errorf("expected dev port 3001:3000, got %s", devSvc.Ports[0])
	}
	if devSvc.Env["NODE_ENV"] != "development" {
		t.Errorf("expected dev NODE_ENV, got %s", devSvc.Env["NODE_ENV"])
	}
	if devSvc.Ingress.Domain != "dev.example.local" {
		t.Errorf("expected dev ingress domain, got %s", devSvc.Ingress.Domain)
	}

	// 3. Staging environment override
	stagingProj, err := config.LoadProjectWithEnv(cfgPath, "staging")
	if err != nil {
		t.Fatalf("failed to load staging config: %v", err)
	}
	if stagingProj.Target != "staging-server" {
		t.Errorf("expected staging target, got '%s'", stagingProj.Target)
	}
	stagingSvc := stagingProj.Services["multi-env-app"]
	if stagingSvc.Ingress.Domain != "staging.example.com" {
		t.Errorf("expected staging ingress domain, got %s", stagingSvc.Ingress.Domain)
	}
}

func TestScaleOutMultiTargetConfig(t *testing.T) {
	yamlContent := `
version: "1"
name: "scale-app"
targets:
  - "10.0.0.1"
  - "10.0.0.2"

type: "docker"
image: "my-app:1.0"
ports:
  - "3000:3000"

environments:
  production:
    targets:
      - "ssh://deploy@192.168.1.101:22"
      - "ssh://deploy@192.168.1.102:22"
      - "ssh://deploy@192.168.1.103:22"
    ingress:
      domain: "app.prod.com"
      lb_policy: "round_robin"
`
	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/kizuna.yaml"
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	baseProj, err := config.LoadProject(cfgPath)
	if err != nil {
		t.Fatalf("failed to load base: %v", err)
	}
	if len(baseProj.Targets) != 2 {
		t.Fatalf("expected 2 base targets, got %d", len(baseProj.Targets))
	}
	if baseProj.Target != "10.0.0.1" {
		t.Errorf("expected primary target 10.0.0.1, got %s", baseProj.Target)
	}

	prodProj, err := config.LoadProjectWithEnv(cfgPath, "production")
	if err != nil {
		t.Fatalf("failed to load prod: %v", err)
	}
	if len(prodProj.Targets) != 3 {
		t.Fatalf("expected 3 prod targets, got %d", len(prodProj.Targets))
	}
	if prodProj.Targets[0] != "ssh://deploy@192.168.1.101:22" {
		t.Errorf("unexpected first prod target: %s", prodProj.Targets[0])
	}
}

func TestLoadSSHConfig(t *testing.T) {
	yamlContent := `
version: "1.0"
name: "secure-app"
ssh:
  enabled: true
  allow_tags:
    - admin
    - devops
  deny_tags:
    - guest
  allow_nodes:
    - bastion
  deny_nodes:
    - untrusted-node
`
	tmpDir := t.TempDir()
	cfgPath := tmpDir + "/kizuna.yaml"
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	proj, err := config.LoadProject(cfgPath)
	if err != nil {
		t.Fatalf("failed to load project with ssh config: %v", err)
	}

	if proj.SSH == nil {
		t.Fatalf("expected SSH config to be non-nil")
	}

	if !proj.SSH.IsEnabled() {
		t.Errorf("expected SSH to be enabled")
	}

	if len(proj.SSH.AllowTags) != 2 || proj.SSH.AllowTags[0] != "admin" {
		t.Errorf("expected allow_tags [admin, devops], got %v", proj.SSH.AllowTags)
	}

	if len(proj.SSH.DenyTags) != 1 || proj.SSH.DenyTags[0] != "guest" {
		t.Errorf("expected deny_tags [guest], got %v", proj.SSH.DenyTags)
	}

	if len(proj.SSH.AllowNodes) != 1 || proj.SSH.AllowNodes[0] != "bastion" {
		t.Errorf("expected allow_nodes [bastion], got %v", proj.SSH.AllowNodes)
	}

	if len(proj.SSH.DenyNodes) != 1 || proj.SSH.DenyNodes[0] != "untrusted-node" {
		t.Errorf("expected deny_nodes [untrusted-node], got %v", proj.SSH.DenyNodes)
	}
}
