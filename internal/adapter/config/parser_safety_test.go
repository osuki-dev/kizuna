package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osuki-dev/kizuna/internal/adapter/config"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kizuna.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigurationRejectsAmbiguousInput(t *testing.T) {
	t.Setenv("KIZUNA_ENV", "")
	tests := []struct{ name, text, environment, want string }{
		{"unknown root field", "name: app\nreplicass: 2\n", "", "field replicass"},
		{"unknown nested field", "name: app\ntype: docker\nimage: nginx\ndeploy:\n  commmand: run\n", "", "field commmand"},
		{"unknown environment", "name: app\ntype: docker\nimage: nginx\n", "prod", "unknown environment"},
		{"unknown service override", "name: app\nservices:\n  api:\n    image: nginx\nenvironments:\n  prod:\n    services:\n      typo:\n        replicas: 2\n", "prod", "unknown service"},
		{"null service", "services:\n  app: null\n", "", "cannot be null"},
		{"null environment", "environments:\n  prod: null\n", "", "cannot be null"},
		{"multiple documents", "name: app\n---\nname: other\n", "", "exactly one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.LoadProjectWithEnv(writeConfig(t, tt.text), tt.environment)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
		})
	}
}

func TestPathsAndComposeReferences(t *testing.T) {
	t.Setenv("KIZUNA_ENV", "")
	path := writeConfig(t, `name: app
services:
  api:
    root: src
    dockerfile: containers/Dockerfile
  web:
    compose_file: compose.yaml
    compose:
      project_name: production
      services: [web]
      env_files: [/etc/app/secret.env]
      profiles: [frontend]
environments:
  prod:
    services:
      web:
        target: ssh://deploy@example.com
        compose:
          project_name: production
          services: [web, worker]
          env_files: [/etc/app/prod.env]
`)
	proj, err := config.LoadProjectWithEnv(path, "prod")
	if err != nil {
		t.Fatal(err)
	}
	api := proj.Services["api"]
	if api.Root != filepath.Join(filepath.Dir(path), "src") || api.Dockerfile != "containers/Dockerfile" {
		t.Fatalf("incorrect build paths: %+v", api)
	}
	web := proj.Services["web"]
	if web.Root != filepath.Dir(path) || web.ComposeFile != "compose.yaml" || web.Type != entity.TypeCompose {
		t.Fatalf("incorrect compose paths: %+v", web)
	}
	if web.Target != "ssh://deploy@example.com" || len(web.Compose.Services) != 2 || web.Compose.EnvFiles[0] != "/etc/app/prod.env" {
		t.Fatalf("incorrect override: %+v", web)
	}
}

func TestImageInfersDockerWorkload(t *testing.T) {
	t.Setenv("KIZUNA_ENV", "")
	proj, err := config.LoadProject(writeConfig(t, "name: app\nimage: nginx:stable\n"))
	if err != nil {
		t.Fatal(err)
	}
	if proj.Services["app"].Type != entity.TypeDocker {
		t.Fatalf("image inferred as %s", proj.Services["app"].Type)
	}
}

func TestPortMappingValidation(t *testing.T) {
	for _, mapping := range []string{"8080:80", "127.0.0.1:5433:5432", "[::1]:8080:80/tcp", "53:53/udp", "80", "8000-8002:80-82", "8000-8002:80", ":80", "65535:65535/sctp"} {
		if err := entity.ValidatePortMapping(mapping); err != nil {
			t.Errorf("rejected %q: %v", mapping, err)
		}
	}
	for _, mapping := range []string{"0:80", "65536:80", "-1:80", "80:0", "1-2:80-82", "8080:80/https", "[bad]:80:80", "::1:80:80", "localhost:80:80", "8002-8000:80", "80:80:80:80", "80\n:80", "[::1]:80"} {
		if err := entity.ValidatePortMapping(mapping); err == nil {
			t.Errorf("accepted %q", mapping)
		}
	}
}

func TestUnsafeNamesAndBounds(t *testing.T) {
	for _, name := range []string{"../other", "app;run", "-flag", "", strings.Repeat("x", 64)} {
		if err := entity.ValidateServiceName(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	svc := &entity.Service{Type: entity.TypeDocker, Root: t.TempDir(), Image: "nginx", Replicas: -1, Logging: &entity.LoggingConfig{MaxFile: -1}, Backup: &entity.BackupConfig{Retention: &entity.RetentionConfig{KeepDays: -1}}}
	issues := config.ValidateProject(&entity.Project{Name: "app", Services: map[string]*entity.Service{"../app": svc}}, ".")
	fields := map[string]bool{}
	for _, issue := range issues {
		if issue.Severity == config.SeverityError {
			fields[issue.Field] = true
		}
	}
	for _, field := range []string{"name", "replicas", "logging.max_file", "backup.retention"} {
		if !fields[field] {
			t.Errorf("missing %s error", field)
		}
	}
}

func TestExplicitIngressUpstreams(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		address string
		valid   bool
	}{
		{"localhost:3000", true}, {"10.0.0.9:8096", true}, {"[::1]:8080", true},
		{"http://app:8080", true}, {"https://app.example.com:443", true}, {"h2c://app:8080", true}, {"my_app:8080", true},
		{"", false}, {"app", false}, {"app:0", false}, {"app:65536", false}, {"app:80-81", false},
		{"ftp://app:21", false}, {"https://app:443/path", false}, {"https://app:443/", false}, {"https://user:pass@app:443", false},
		{"https://app:443?x=1", false}, {"https://app:443#fragment", false}, {"app:80\n}", false}, {"::1:80", false},
		{"-bad:80", false}, {"bad..host:80", false},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			svc := &entity.Service{Name: "app", Type: entity.TypeDocker, Root: root, Image: "nginx", Ingress: &entity.IngressConfig{Provider: "caddy", Domain: "app.example.com", Upstreams: []string{tt.address}}}
			issues := config.ValidateProject(&entity.Project{Name: "app", Services: map[string]*entity.Service{"app": svc}}, root)
			invalid := false
			for _, issue := range issues {
				if issue.Field == "ingress.upstream_port" {
					t.Fatalf("explicit upstream required fallback port: %+v", issue)
				}
				if issue.Field == "ingress.upstreams" && issue.Severity == config.SeverityError {
					invalid = true
				}
			}
			if invalid == tt.valid {
				t.Fatalf("upstream %q valid=%t issues=%+v", tt.address, tt.valid, issues)
			}
		})
	}
}
