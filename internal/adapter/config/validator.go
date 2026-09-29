package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// IssueSeverity indicates error or warning
type IssueSeverity string

const (
	SeverityError   IssueSeverity = "ERROR"
	SeverityWarning IssueSeverity = "WARNING"
)

// ValidationIssue describes a configuration problem
type ValidationIssue struct {
	Service    string        `json:"service,omitempty"`
	Field      string        `json:"field"`
	Message    string        `json:"message"`
	Severity   IssueSeverity `json:"severity"`
	Suggestion string        `json:"suggestion,omitempty"`
}

func (v ValidationIssue) String() string {
	prefix := "✖ [ERROR]"
	if v.Severity == SeverityWarning {
		prefix = "⚠ [WARN] "
	}
	svc := ""
	if v.Service != "" {
		svc = fmt.Sprintf("[%s] ", v.Service)
	}
	s := fmt.Sprintf("%s %s%s: %s", prefix, svc, v.Field, v.Message)
	if v.Suggestion != "" {
		s += fmt.Sprintf(" (Tip: %s)", v.Suggestion)
	}
	return s
}

// ValidateProject performs deep validation on a parsed Project configuration
func ValidateProject(proj *entity.Project, baseDir string) []ValidationIssue {
	var issues []ValidationIssue

	if strings.TrimSpace(proj.Name) == "" {
		issues = append(issues, ValidationIssue{
			Field:      "name",
			Message:    "Project name is empty",
			Severity:   SeverityError,
			Suggestion: "Add 'name: my-project' at the top of kizuna.yaml",
		})
	}

	if len(proj.Services) == 0 {
		issues = append(issues, ValidationIssue{
			Field:      "services",
			Message:    "No services or workload definitions found in configuration",
			Severity:   SeverityError,
			Suggestion: "Define at least one service under 'services:' or specify 'type' at root",
		})
		return issues
	}

	for name, svc := range proj.Services {
		issues = append(issues, validateService(svc, name, baseDir)...)
	}

	return issues
}

func validateService(svc *entity.Service, name, baseDir string) []ValidationIssue {
	var issues []ValidationIssue

	// 1. Service Type Validation
	validTypes := map[entity.ServiceType]bool{
		entity.TypeDocker:  true,
		entity.TypeCompose: true,
		entity.TypeBun:     true,
		entity.TypeNode:    true,
		entity.TypeProcess: true,
	}

	if !validTypes[svc.Type] {
		issues = append(issues, ValidationIssue{
			Service:    name,
			Field:      "type",
			Message:    fmt.Sprintf("Invalid service type '%s'", svc.Type),
			Severity:   SeverityError,
			Suggestion: "Supported types: 'docker', 'compose', 'bun', 'node', 'process'",
		})
	}

	// 2. Root directory check
	workDir := filepath.Join(baseDir, svc.Root)
	if stat, err := os.Stat(workDir); err != nil || !stat.IsDir() {
		issues = append(issues, ValidationIssue{
			Service:    name,
			Field:      "root",
			Message:    fmt.Sprintf("Working root directory '%s' does not exist", svc.Root),
			Severity:   SeverityWarning,
			Suggestion: "Ensure directory exists or set root: '.'",
		})
	}

	// 3. Workload specific checks
	switch svc.Type {
	case entity.TypeDocker:
		if svc.Dockerfile != "" {
			dfPath := filepath.Join(workDir, svc.Dockerfile)
			if _, err := os.Stat(dfPath); err != nil {
				issues = append(issues, ValidationIssue{
					Service:    name,
					Field:      "dockerfile",
					Message:    fmt.Sprintf("Dockerfile not found at '%s'", dfPath),
					Severity:   SeverityError,
					Suggestion: "Check file path relative to service root",
				})
			}
		} else if svc.Image == "" {
			// Neither dockerfile nor image
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "dockerfile / image",
				Message:    "Docker service requires either 'dockerfile' or 'image'",
				Severity:   SeverityError,
				Suggestion: "Provide dockerfile: 'Dockerfile' or image: 'nginx:latest'",
			})
		}

	case entity.TypeCompose:
		cf := svc.ComposeFile
		if cf == "" {
			cf = "docker-compose.yml"
		}
		cfPath := filepath.Join(workDir, cf)
		if _, err := os.Stat(cfPath); err != nil {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "compose_file",
				Message:    fmt.Sprintf("Compose file not found at '%s'", cfPath),
				Severity:   SeverityError,
				Suggestion: "Check that compose_file points to an existing docker-compose.yml",
			})
		}

	case entity.TypeBun, entity.TypeNode, entity.TypeProcess:
		if svc.Deploy == nil || svc.Deploy.Command == "" {
			// Check if package.json exists in root
			pkgJSON := filepath.Join(workDir, "package.json")
			if _, err := os.Stat(pkgJSON); err != nil {
				issues = append(issues, ValidationIssue{
					Service:    name,
					Field:      "deploy.command",
					Message:    "No start command specified for process service",
					Severity:   SeverityWarning,
					Suggestion: "Add deploy.command: 'bun run start'",
				})
			}
		}
	}

	// 4. Ports validation
	for _, p := range svc.Ports {
		parts := strings.Split(p, ":")
		if len(parts) != 2 {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "ports",
				Message:    fmt.Sprintf("Invalid port mapping '%s', must be 'host:container'", p),
				Severity:   SeverityError,
			})
		} else {
			if _, err := strconv.Atoi(parts[0]); err != nil {
				issues = append(issues, ValidationIssue{
					Service:  name,
					Field:    "ports",
					Message:  fmt.Sprintf("Invalid host port '%s'", parts[0]),
					Severity: SeverityError,
				})
			}
			if _, err := strconv.Atoi(parts[1]); err != nil {
				issues = append(issues, ValidationIssue{
					Service:  name,
					Field:    "ports",
					Message:  fmt.Sprintf("Invalid container port '%s'", parts[1]),
					Severity: SeverityError,
				})
			}
		}
	}

	// 5. Ingress validation
	if svc.Ingress != nil && svc.Ingress.Provider == "caddy" {
		if strings.TrimSpace(svc.Ingress.Domain) == "" {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "ingress.domain",
				Message:    "Ingress domain cannot be empty when Caddy provider is enabled",
				Severity:   SeverityError,
				Suggestion: "Set ingress.domain: 'my-site.example.com'",
			})
		}
		if svc.Ingress.UpstreamPort <= 0 || svc.Ingress.UpstreamPort > 65535 {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "ingress.upstream_port",
				Message:    fmt.Sprintf("Invalid upstream port %d, must be between 1 and 65535", svc.Ingress.UpstreamPort),
				Severity:   SeverityError,
				Suggestion: "Set ingress.upstream_port to the port your app listens on (e.g. 3000)",
			})
		}
	}

	// 6. Backup validation
	if svc.Backup != nil {
		if len(svc.Backup.Paths) == 0 {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "backup.paths",
				Message:    "Backup configuration has no target paths",
				Severity:   SeverityWarning,
				Suggestion: "Specify at least one directory or file under backup.paths",
			})
		}
		if svc.Backup.Storage != nil && svc.Backup.Storage.Type == "s3" {
			if strings.TrimSpace(svc.Backup.Storage.Bucket) == "" {
				issues = append(issues, ValidationIssue{
					Service:    name,
					Field:      "backup.storage.bucket",
					Message:    "S3 backup requires 'bucket' name",
					Severity:   SeverityError,
					Suggestion: "Set backup.storage.bucket: 'my-bucket-name'",
				})
			}
		}
	}

	return issues
}
