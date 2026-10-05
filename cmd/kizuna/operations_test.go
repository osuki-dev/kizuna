package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/osuki-dev/kizuna/internal/adapter/config"
	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLocalDaemonRequestUsesLoopbackCredential(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "127.0.0.1:19800" || r.Header.Get("Authorization") != "Bearer kzn_local" {
			t.Fatalf("unexpected request: %s %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}
	resp, err := localDaemonRequest(context.Background(), client, http.MethodGet, "/api/v1/node/members", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestJSONFailureReturnsOriginalError(t *testing.T) {
	var output bytes.Buffer
	cause := errors.New("deployment failed")
	if err := printJSONError(&output, cause, nil); !errors.Is(err, cause) {
		t.Fatalf("failure became success: %v", err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"success": false`)) || !bytes.Contains(output.Bytes(), []byte(cause.Error())) {
		t.Fatalf("invalid JSON failure: %s", output.String())
	}
}

func TestOperationTargetNeverChoosesFirstOfMultiple(t *testing.T) {
	project := &entity.Project{Targets: []string{"first", "second"}}
	svc := &entity.Service{Name: "web"}
	if _, err := operationTarget(project, svc); err == nil {
		t.Fatal("ambiguous target accepted")
	}
	svc.Target = "second"
	target, err := operationTarget(project, svc)
	if err != nil || target.Host != "second" {
		t.Fatalf("service target override lost: %v %v", target, err)
	}
}

func TestOperationNodeRejectsMissingName(t *testing.T) {
	repo, err := config.NewNodeRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveNode(&entity.Node{Name: "first", Addr: "first-address"}); err != nil {
		t.Fatal(err)
	}
	if _, err := operationNode(repo, entity.ParseTargetHost("missing")); !errors.Is(err, domain.ErrNodeNotFound) {
		t.Fatalf("wrong destination accepted: %v", err)
	}
}

func TestBackupPathsUseServiceRootAndRequireRemoteAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	svc := &entity.Service{Root: root, Backup: &entity.BackupConfig{Paths: []string{"data"}, Database: &entity.DatabaseBackupConfig{Type: "sqlite", Path: "db.sqlite"}}}
	cfg, err := backupConfiguration(svc, entity.ParseTargetHost("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Paths[0] != filepath.Join(root, "data") || cfg.Database.Path != filepath.Join(root, "db.sqlite") {
		t.Fatalf("wrong local paths: %+v", cfg)
	}
	if svc.Backup.Paths[0] != "data" || svc.Backup.Database.Path != "db.sqlite" {
		t.Fatal("mutated project configuration")
	}
	if _, err := backupConfiguration(svc, entity.ParseTargetHost("remote-node")); err == nil {
		t.Fatal("ambiguous remote paths accepted")
	}
}

func TestScaleArgumentsRequireCountAndExactService(t *testing.T) {
	project := &entity.Project{Services: map[string]*entity.Service{"web": {Name: "web"}}}
	for _, args := range [][]string{{"3"}, {"web", "3"}, {"web=3"}} {
		name, replicas, err := scaleArguments(project, args)
		if err != nil || name != "web" || replicas != 3 {
			t.Fatalf("%v: %s %d %v", args, name, replicas, err)
		}
	}
	for _, args := range [][]string{{"web"}, {"web", "oops"}, {"missing=3"}, {"web", "3", "extra"}, {"0"}, {"1001"}, {"=3"}} {
		if _, _, err := scaleArguments(project, args); err == nil {
			t.Fatalf("invalid scale accepted: %v", args)
		}
	}
}
