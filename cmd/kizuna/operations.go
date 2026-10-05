package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/osuki-dev/kizuna/internal/adapter/config"
	"github.com/osuki-dev/kizuna/internal/adapter/presenter"
	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Local IPC uses the loopback-only credential, never a remote node's token.
func localDaemonRequest(ctx context.Context, client *http.Client, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:19800"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer kzn_local")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

// JSON errors must still return a nonzero command exit status for automation.
func printJSONError(writer io.Writer, cause error, fields map[string]any) error {
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["success"] = false
	fields["error"] = cause.Error()
	if err := presenter.PrintJSON(writer, fields); err != nil {
		return fmt.Errorf("%w (writing JSON: %v)", cause, err)
	}
	return cause
}

func loadOperationService(name string) (*entity.Project, *entity.Service, error) {
	path := flagConfig
	if path == "" {
		var err error
		path, err = config.FindConfigFile()
		if err != nil {
			return nil, nil, err
		}
	}
	project, err := config.LoadProjectWithEnv(path, flagEnv)
	if err != nil {
		return nil, nil, err
	}
	svc, ok := project.Services[name]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", domain.ErrServiceNotFound, name)
	}
	return project, svc, nil
}

func operationTarget(project *entity.Project, svc *entity.Service) (*entity.TargetHost, error) {
	if svc.Target != "" {
		return entity.ParseTargetHost(svc.Target), nil
	}
	targets := project.Targets
	if len(targets) > 1 {
		return nil, fmt.Errorf("service %q has multiple targets; set its target explicitly for logs or backup", svc.Name)
	}
	target := project.Target
	if len(targets) == 1 {
		target = targets[0]
	}
	if target == "" || target == "default" {
		target = "localhost"
	}
	return entity.ParseTargetHost(target), nil
}

func operationNode(repo domain.NodeRepository, target *entity.TargetHost) (*entity.Node, error) {
	if target.Type == entity.TargetTypeLocal {
		return &entity.Node{Name: "local-node", Addr: "127.0.0.1:19800", AuthToken: "kzn_local"}, nil
	}
	if target.Type != entity.TargetTypeMesh {
		return nil, fmt.Errorf("this operation requires a mesh agent on SSH targets")
	}
	node, err := repo.GetNode(target.Host)
	if err == nil {
		return node, nil
	}
	nodes, listErr := repo.ListNodes()
	if listErr != nil {
		return nil, listErr
	}
	for _, n := range nodes {
		if n != nil && (n.ID == target.Host || n.Name == target.Host || n.Addr == target.Host) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", domain.ErrNodeNotFound, target.Host)
}

func operationError(err error) error {
	if flagJSON {
		return printJSONError(os.Stdout, err, nil)
	}
	return err
}

func backupConfiguration(svc *entity.Service, target *entity.TargetHost) (*entity.BackupConfig, error) {
	encoded, err := json.Marshal(svc.Backup)
	if err != nil {
		return nil, err
	}
	var cfg entity.BackupConfig
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		return nil, err
	}
	resolve := func(path string) (string, error) {
		if target.Type == entity.TargetTypeLocal {
			path = os.ExpandEnv(path)
			if !filepath.IsAbs(path) {
				path = filepath.Join(svc.Root, path)
			}
			return filepath.Abs(path)
		}
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("remote backup paths must be absolute target paths: %q", path)
		}
		return path, nil
	}
	for i, path := range cfg.Paths {
		cfg.Paths[i], err = resolve(path)
		if err != nil {
			return nil, err
		}
	}
	if cfg.Database != nil && cfg.Database.Path != "" {
		cfg.Database.Path, err = resolve(cfg.Database.Path)
		if err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

func scaleArguments(project *entity.Project, args []string) (string, int, error) {
	var name, count string
	switch len(args) {
	case 1:
		if before, after, ok := strings.Cut(args[0], "="); ok {
			name, count = before, after
			if name == "" {
				return "", 0, fmt.Errorf("service name is required")
			}
		} else {
			count = args[0]
		}
	case 2:
		name, count = args[0], args[1]
	default:
		return "", 0, fmt.Errorf("usage: kizuna scale [service] <replicas>")
	}
	replicas, err := strconv.Atoi(count)
	if err != nil || replicas < 1 || replicas > 1000 {
		return "", 0, fmt.Errorf("replica count must be an integer between 1 and 1000")
	}
	if name == "" {
		if len(project.Services) != 1 {
			return "", 0, fmt.Errorf("specify a service to scale")
		}
		for service := range project.Services {
			name = service
		}
	}
	if _, ok := project.Services[name]; !ok {
		return "", 0, fmt.Errorf("%w: %s", domain.ErrServiceNotFound, name)
	}
	return name, replicas, nil
}
