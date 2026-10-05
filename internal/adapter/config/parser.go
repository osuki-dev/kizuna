package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"gopkg.in/yaml.v3"
)

// RawConfig represents the YAML structure supporting monorepo, single-project, and multi-environment
type RawConfig struct {
	Compose      *entity.ComposeConfig                `yaml:"compose,omitempty"`
	Version      string                               `yaml:"version"`
	Name         string                               `yaml:"name"`
	Target       string                               `yaml:"target,omitempty"`
	Targets      []string                             `yaml:"targets,omitempty"`
	Theme        string                               `yaml:"theme,omitempty"`
	CustomTheme  *entity.Theme                        `yaml:"custom_theme,omitempty"`
	Services     map[string]*RawServiceConfig         `yaml:"services,omitempty"`
	Environments map[string]*entity.EnvironmentConfig `yaml:"environments,omitempty"`
	DERP         *entity.DERPConfig                   `yaml:"derp,omitempty"`
	SSH          *entity.SSHConfig                    `yaml:"ssh,omitempty"`
	Mesh         *entity.MeshConfig                   `yaml:"mesh,omitempty"`

	// Shorthand fields for single-service projects
	Type        string                `yaml:"type,omitempty"`
	Root        string                `yaml:"root,omitempty"`
	Dockerfile  string                `yaml:"dockerfile,omitempty"`
	ComposeFile string                `yaml:"compose_file,omitempty"`
	Image       string                `yaml:"image,omitempty"`
	Ports       []string              `yaml:"ports,omitempty"`
	Replicas    int                   `yaml:"replicas,omitempty"`
	Env         map[string]string     `yaml:"env,omitempty"`
	Build       *entity.BuildConfig   `yaml:"build,omitempty"`
	Deploy      *entity.DeployConfig  `yaml:"deploy,omitempty"`
	Ingress     *entity.IngressConfig `yaml:"ingress,omitempty"`
	Backup      *entity.BackupConfig  `yaml:"backup,omitempty"`
	Logging     *entity.LoggingConfig `yaml:"logging,omitempty"`
}

// RawServiceConfig defines YAML service configuration
type RawServiceConfig struct {
	Target      string                `yaml:"target,omitempty"`
	Compose     *entity.ComposeConfig `yaml:"compose,omitempty"`
	Type        string                `yaml:"type"`
	Root        string                `yaml:"root,omitempty"`
	Dockerfile  string                `yaml:"dockerfile,omitempty"`
	ComposeFile string                `yaml:"compose_file,omitempty"`
	Image       string                `yaml:"image,omitempty"`
	Ports       []string              `yaml:"ports,omitempty"`
	Replicas    int                   `yaml:"replicas,omitempty"`
	Env         map[string]string     `yaml:"env,omitempty"`
	Build       *entity.BuildConfig   `yaml:"build,omitempty"`
	Deploy      *entity.DeployConfig  `yaml:"deploy,omitempty"`
	Ingress     *entity.IngressConfig `yaml:"ingress,omitempty"`
	Backup      *entity.BackupConfig  `yaml:"backup,omitempty"`
	Logging     *entity.LoggingConfig `yaml:"logging,omitempty"`
}

// FindConfigFile finds kizuna.yaml or kizuna.yml in current or parent directory
func FindConfigFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		for _, name := range []string{"kizuna.yaml", "kizuna.yml"} {
			p := filepath.Join(dir, name)
			if stat, err := os.Stat(p); err == nil && !stat.IsDir() {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("kizuna.yaml not found in current or parent directories")
}

// LoadProject parses and normalizes a kizuna.yaml using default/environment resolution
func LoadProject(filePath string) (*entity.Project, error) {
	return LoadProjectWithEnv(filePath, "")
}

// LoadProjectWithEnv parses kizuna.yaml and applies specific environment overrides (dev, staging, prod, etc.)
func LoadProjectWithEnv(filePath string, envName string) (*entity.Project, error) {
	filePath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var raw RawConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to parse yaml: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	for name, svc := range raw.Services {
		if svc == nil {
			return nil, fmt.Errorf("service %q cannot be null", name)
		}
	}
	for name, env := range raw.Environments {
		if env == nil {
			return nil, fmt.Errorf("environment %q cannot be null", name)
		}
		for serviceName, override := range env.Services {
			_, ok := raw.Services[serviceName]
			shorthandName := raw.Name
			if shorthandName == "" {
				shorthandName = "app"
			}
			if !ok && (len(raw.Services) != 0 || serviceName != shorthandName) {
				return nil, fmt.Errorf("environment %q references unknown service %q", name, serviceName)
			}
			if override == nil {
				return nil, fmt.Errorf("environment %q service %q cannot be null", name, serviceName)
			}
		}
	}
	if envName == "" {
		envName = os.Getenv("KIZUNA_ENV")
	}

	if envName != "" {
		if _, ok := raw.Environments[envName]; !ok {
			return nil, fmt.Errorf("unknown environment %q", envName)
		}
	}

	var envList []string
	for e := range raw.Environments {
		envList = append(envList, e)
	}
	sort.Strings(envList)

	project := &entity.Project{
		Version:      raw.Version,
		Name:         raw.Name,
		Target:       raw.Target,
		Targets:      raw.Targets,
		Theme:        raw.Theme,
		ActiveEnv:    envName,
		Environments: envList,
		Services:     make(map[string]*entity.Service),
		DERP:         raw.DERP,
		SSH:          raw.SSH,
		Mesh:         raw.Mesh,
	}
	if project.Name == "" {
		project.Name = filepath.Base(filepath.Dir(filePath))
	}
	if len(project.Targets) > 0 && project.Target == "" {
		project.Target = project.Targets[0]
	}
	if project.Target == "" {
		project.Target = "default"
	}
	if len(project.Targets) == 0 && project.Target != "default" {
		project.Targets = []string{project.Target}
	}

	// 1. Process explicit services (Monorepo)
	for name, s := range raw.Services {
		root := s.Root
		if root == "" {
			root = "."
		}
		svcType := entity.ServiceType(s.Type)
		if svcType == "" {
			if s.ComposeFile != "" || s.Compose != nil {
				svcType = entity.TypeCompose
			} else if s.Dockerfile != "" || s.Image != "" {
				svcType = entity.TypeDocker
			} else {
				svcType = entity.TypeProcess
			}
		}

		project.Services[name] = &entity.Service{
			Name:        name,
			Target:      s.Target,
			Compose:     s.Compose,
			Type:        svcType,
			Root:        root,
			Dockerfile:  s.Dockerfile,
			ComposeFile: s.ComposeFile,
			Image:       s.Image,
			Ports:       s.Ports,
			Replicas:    s.Replicas,
			Env:         copyEnv(s.Env),
			Build:       s.Build,
			Deploy:      s.Deploy,
			Ingress:     s.Ingress,
			Backup:      s.Backup,
			Logging:     s.Logging,
			State:       entity.StateStopped,
			UpdatedAt:   time.Now(),
		}
	}

	// 2. Process shorthand single-service config if services map was empty
	if len(project.Services) == 0 && (raw.Type != "" || raw.Dockerfile != "" || raw.ComposeFile != "" || raw.Compose != nil || raw.Build != nil || raw.Deploy != nil || raw.Image != "") {
		name := raw.Name
		if name == "" {
			name = "app"
		}
		svcType := entity.ServiceType(raw.Type)
		if svcType == "" {
			if raw.ComposeFile != "" || raw.Compose != nil {
				svcType = entity.TypeCompose
			} else if raw.Dockerfile != "" || raw.Image != "" {
				svcType = entity.TypeDocker
			} else {
				svcType = entity.TypeProcess
			}
		}

		project.Services[name] = &entity.Service{
			Name:        name,
			Compose:     raw.Compose,
			Type:        svcType,
			Root:        raw.Root,
			Dockerfile:  raw.Dockerfile,
			ComposeFile: raw.ComposeFile,
			Image:       raw.Image,
			Ports:       raw.Ports,
			Replicas:    raw.Replicas,
			Env:         copyEnv(raw.Env),
			Build:       raw.Build,
			Deploy:      raw.Deploy,
			Ingress:     raw.Ingress,
			Backup:      raw.Backup,
			Logging:     raw.Logging,
			State:       entity.StateStopped,
			UpdatedAt:   time.Now(),
		}
	}

	// 3. Apply Environment Overrides if specified
	if envName != "" && raw.Environments != nil {
		if envCfg, ok := raw.Environments[envName]; ok {
			applyEnvOverrides(project, envCfg)
		}
	}

	for _, svc := range project.Services {
		if !filepath.IsAbs(svc.Root) {
			svc.Root = filepath.Join(filepath.Dir(filePath), svc.Root)
		}
		svc.Root = filepath.Clean(svc.Root)
	}
	return project, nil
}

func applyEnvOverrides(project *entity.Project, envCfg *entity.EnvironmentConfig) {
	if len(envCfg.Targets) > 0 {
		project.Targets = envCfg.Targets
		if envCfg.Target == "" {
			project.Target = envCfg.Targets[0]
		}
	}
	if envCfg.Target != "" {
		project.Target = envCfg.Target
		if len(envCfg.Targets) == 0 {
			project.Targets = []string{envCfg.Target}
		}
	}
	if envCfg.Theme != "" {
		project.Theme = envCfg.Theme
	}

	// For shorthand/single-service project
	if len(project.Services) == 1 {
		for _, svc := range project.Services {
			if envCfg.Compose != nil {
				svc.Compose = envCfg.Compose
			}
			if len(envCfg.Ports) > 0 {
				svc.Ports = envCfg.Ports
			}
			if len(envCfg.Env) > 0 {
				if svc.Env == nil {
					svc.Env = make(map[string]string)
				}
				for k, v := range envCfg.Env {
					svc.Env[k] = v
				}
			}
			if envCfg.Build != nil {
				svc.Build = envCfg.Build
			}
			if envCfg.Deploy != nil {
				svc.Deploy = envCfg.Deploy
			}
			if envCfg.Ingress != nil {
				svc.Ingress = envCfg.Ingress
			}
			if envCfg.Backup != nil {
				svc.Backup = envCfg.Backup
			}
			if envCfg.Replicas != 0 {
				svc.Replicas = envCfg.Replicas
			}
			if envCfg.Logging != nil {
				svc.Logging = envCfg.Logging
			}
		}
	}

	// For multi-service projects with specific service overrides
	for sName, sOverride := range envCfg.Services {
		if svc, ok := project.Services[sName]; ok && sOverride != nil {
			if sOverride.Target != "" {
				svc.Target = sOverride.Target
			}
			if sOverride.Compose != nil {
				svc.Compose = sOverride.Compose
			}
			if len(sOverride.Ports) > 0 {
				svc.Ports = sOverride.Ports
			}
			if len(sOverride.Env) > 0 {
				if svc.Env == nil {
					svc.Env = make(map[string]string)
				}
				for k, v := range sOverride.Env {
					svc.Env[k] = v
				}
			}
			if sOverride.Build != nil {
				svc.Build = sOverride.Build
			}
			if sOverride.Deploy != nil {
				svc.Deploy = sOverride.Deploy
			}
			if sOverride.Ingress != nil {
				svc.Ingress = sOverride.Ingress
			}
			if sOverride.Backup != nil {
				svc.Backup = sOverride.Backup
			}
			if sOverride.Replicas != 0 {
				svc.Replicas = sOverride.Replicas
			}
			if sOverride.Logging != nil {
				svc.Logging = sOverride.Logging
			}
		}
	}
}

func copyEnv(src map[string]string) map[string]string {
	if src == nil {
		return make(map[string]string)
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
