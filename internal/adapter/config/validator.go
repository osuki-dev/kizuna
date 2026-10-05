package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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

	if proj.Mesh != nil {
		if proj.Mesh.MaxUploadMB < 0 || proj.Mesh.MaxUploadMB > 102400 {
			issues = append(issues, ValidationIssue{Field: "mesh.max_upload_mb", Message: "Upload limit must be between 0 and 102400 MB", Severity: SeverityError})
		}
		for field, value := range map[string]string{"probe_timeout": proj.Mesh.ProbeTimeout, "gossip_interval": proj.Mesh.GossipInterval} {
			if value != "" {
				if d, err := time.ParseDuration(value); err != nil || d <= 0 {
					issues = append(issues, ValidationIssue{Field: "mesh." + field, Message: "Duration must be positive", Severity: SeverityError})
				}
			}
		}
	}
	if proj.DERP != nil {
		if proj.DERP.Port < 0 || proj.DERP.Port > 65535 || proj.DERP.STUNPort < 0 || proj.DERP.STUNPort > 65535 {
			issues = append(issues, ValidationIssue{Field: "derp.port", Message: "Ports must be between 0 and 65535", Severity: SeverityError})
		}
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
	addError := func(field, message string) {
		issues = append(issues, ValidationIssue{Service: name, Field: field, Message: message, Severity: SeverityError})
	}
	if err := entity.ValidateServiceName(name); err != nil {
		addError("name", err.Error())
	}
	if svc == nil {
		addError("service", "Service cannot be null")
		return issues
	}
	if svc.Replicas < 0 || svc.Replicas > 1000 {
		addError("replicas", "Replicas must be between 0 and 1000")
	}
	if svc.Logging != nil && (svc.Logging.MaxFile < 0 || svc.Logging.MaxFile > 1000) {
		addError("logging.max_file", "Max file count must be between 0 and 1000")
	}
	if svc.Backup != nil && svc.Backup.Retention != nil && (svc.Backup.Retention.KeepDays < 0 || svc.Backup.Retention.MaxBackups < 0) {
		addError("backup.retention", "Retention values cannot be negative")
	}
	if svc.Compose != nil {
		if svc.Type != entity.TypeCompose {
			addError("compose", "Compose options require compose workload type")
		}
		if svc.Compose.ProjectName != "" && !composeProjectPattern.MatchString(svc.Compose.ProjectName) {
			addError("compose.project_name", "Project name must start with a lowercase letter or digit and contain lowercase letters, digits, underscores or hyphens")
		}
		for _, selection := range svc.Compose.Services {
			if err := entity.ValidateServiceName(selection); err != nil {
				addError("compose.services", err.Error())
			}
		}
		for _, profile := range svc.Compose.Profiles {
			if !composeProfilePattern.MatchString(profile) {
				addError("compose.profiles", "Invalid Compose profile name")
			}
		}
		for _, path := range svc.Compose.EnvFiles {
			if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\x00\r\n") {
				addError("compose.env_files", "Environment file reference must be a nonempty path")
			}
		}
	}

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
	workDir := resolvePath(baseDir, svc.Root)
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
			dfPath := resolvePath(workDir, svc.Dockerfile)
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
		cfPath := resolvePath(workDir, cf)
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
	for _, mapping := range svc.Ports {
		if err := entity.ValidatePortMapping(mapping); err != nil {
			addError("ports", fmt.Sprintf("Invalid port mapping %q: %v", mapping, err))
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
		if len(svc.Ingress.Upstreams) == 0 && (svc.Ingress.UpstreamPort <= 0 || svc.Ingress.UpstreamPort > 65535) {
			issues = append(issues, ValidationIssue{
				Service:    name,
				Field:      "ingress.upstream_port",
				Message:    fmt.Sprintf("Invalid upstream port %d, must be between 1 and 65535", svc.Ingress.UpstreamPort),
				Severity:   SeverityError,
				Suggestion: "Set ingress.upstream_port to the port your app listens on (e.g. 3000)",
			})
		}
		for _, upstream := range svc.Ingress.Upstreams {
			if err := entity.ValidateUpstreamAddress(upstream); err != nil {
				addError("ingress.upstreams", fmt.Sprintf("Invalid upstream %q: %v", upstream, err))
			}
		}
	}

	// 6. Backup validation
	if svc.Backup != nil {
		if len(svc.Backup.Paths) == 0 && svc.Backup.Database == nil {
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

var composeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var composeProfilePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func resolvePath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
