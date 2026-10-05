package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osuki-dev/kizuna/internal/adapter/api"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/auth"
)

type securityRunner struct {
	services    []*entity.Service
	deployments int
	fail        bool
}

func (r *securityRunner) Deploy(context.Context, *entity.Service, io.Reader) error {
	r.deployments++
	return nil
}
func (r *securityRunner) Stop(context.Context, string) error                         { return nil }
func (r *securityRunner) GetStatus(context.Context, string) (*entity.Service, error) { return nil, nil }
func (r *securityRunner) ListServices(context.Context) ([]*entity.Service, error) {
	if r.fail {
		return nil, fmt.Errorf("unavailable")
	}
	return r.services, nil
}
func (r *securityRunner) StreamLogs(context.Context, string, io.Writer) error {
	return fmt.Errorf("missing logs")
}

type securityGossip struct{ calls int }

func (g *securityGossip) HandleMessage(m *entity.GossipMessage) (*entity.GossipMessage, error) {
	g.calls++
	return &entity.GossipMessage{Type: entity.GossipMsgAck}, nil
}
func (*securityGossip) GetMembers() []*entity.Node {
	return []*entity.Node{{Name: "node", AuthToken: "member-secret"}}
}
func (*securityGossip) GetStatus() entity.GossipEngineStatus   { return entity.GossipEngineStatus{} }
func (*securityGossip) UpdateLocalMeta(*entity.NodeMetaUpdate) {}
func (*securityGossip) RemoveMember(string) bool               { return true }
func (*securityGossip) AddOrUpdateMember(*entity.Node)         {}

func securityServer(t *testing.T, runner *securityRunner) (*api.Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	store, err := auth.NewAuthStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin := store.GeneratePIN()
	token, err := store.VerifyPIN(pin, "client")
	if err != nil {
		t.Fatal(err)
	}
	return api.NewServer(store, runner, nil, nil, "id", "node"), token
}
func request(s *api.Server, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}
func TestMembershipRequiresAuthorization(t *testing.T) {
	s, token := securityServer(t, &securityRunner{})
	s.SetGossipEngine(&securityGossip{})
	for _, path := range []string{"sync", "members", "add", "remove"} {
		for _, bad := range []string{"", "invalid"} {
			rr := request(s, http.MethodPost, "/api/v1/node/"+path, bad, strings.NewReader(`{"sender_id":"trusted-node"}`))
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%s with %q returned %d", path, bad, rr.Code)
			}
		}
	}
	rr := request(s, http.MethodGet, "/api/v1/node/members", token, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "member-secret") {
		t.Fatalf("members: %d %s", rr.Code, rr.Body.String())
	}
}
func TestSharedGossipTokenOnlyAuthorizesSync(t *testing.T) {
	s, paired := securityServer(t, &securityRunner{})
	g := &securityGossip{}
	s.SetGossipEngine(g)
	s.SetGossipToken("shared-secret")
	for _, token := range []string{"shared-secret", paired} {
		rr := request(s, http.MethodPost, "/api/v1/node/sync", token, strings.NewReader(`{"type":"ping"}`))
		if rr.Code != 200 {
			t.Fatalf("sync: %d %s", rr.Code, rr.Body.String())
		}
	}
	if g.calls != 2 {
		t.Fatal("sync not delivered")
	}
	rr := request(s, http.MethodPost, "/api/v1/node/remove", "shared-secret", strings.NewReader(`{"name":"node"}`))
	if rr.Code != 401 {
		t.Fatal("gossip token authorized management")
	}
}
func deployRequest(s *api.Server, token, manifest string, artifactSize int) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("manifest", manifest)
	part, _ := writer.CreateFormFile("artifact", "artifact.tar.gz")
	_, _ = part.Write(bytes.Repeat([]byte("x"), artifactSize))
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deploy", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}
func TestDeployEnforcesSizeAndValidatesManifest(t *testing.T) {
	runner := &securityRunner{}
	s, token := securityServer(t, runner)
	s.SetMaxUploadBytes(1024)
	rr := deployRequest(s, token, `{"name":"web","type":"docker","image":"nginx"}`, 2048)
	if rr.Code != 413 {
		t.Fatalf("oversize returned %d: %s", rr.Code, rr.Body.String())
	}
	for _, manifest := range []string{`{"name":"../escape","type":"docker"}`, `{"name":"web","type":"unknown"}`, `{"name":"web","type":"docker","unknown":true}`, `{"name":"web","type":"docker","ports":["70000:80"]}`} {
		rr = deployRequest(s, token, manifest, 0)
		if rr.Code != 400 {
			t.Fatalf("manifest %s returned %d", manifest, rr.Code)
		}
	}
	if runner.deployments != 0 {
		t.Fatal("invalid request deployed")
	}
}
func TestStatusDoesNotExportConfigurationSecrets(t *testing.T) {
	service := &entity.Service{Name: "web", Type: entity.TypeDocker, Image: "nginx", Env: map[string]string{"PASSWORD": "secret"}, Deploy: &entity.DeployConfig{Command: "secret-command", Env: map[string]string{"TOKEN": "secret"}}, Backup: &entity.BackupConfig{Database: &entity.DatabaseBackupConfig{URI: "secret-uri"}}, Ingress: &entity.IngressConfig{DNSToken: "secret-dns"}}
	runner := &securityRunner{services: []*entity.Service{service}}
	s, token := securityServer(t, runner)
	rr := request(s, http.MethodGet, "/api/v1/status", token, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if service.Env["PASSWORD"] != "secret" || service.Backup == nil {
		t.Fatal("redaction mutated runner configuration")
	}
	runner.fail = true
	rr = request(s, http.MethodGet, "/api/v1/status", token, nil)
	if rr.Code != 500 {
		t.Fatal("list failure hidden")
	}
}
func TestRequestDecodingAndLogErrors(t *testing.T) {
	s, token := securityServer(t, &securityRunner{})
	rr := request(s, http.MethodPost, "/api/v1/node/meta", token, strings.NewReader(`{"host":"web"} {"host":"other"}`))
	if rr.Code != 400 {
		t.Fatal("accepted multiple values")
	}
	rr = request(s, http.MethodGet, "/api/v1/logs?service=../escape", token, nil)
	if rr.Code != 400 {
		t.Fatal("accepted path traversal")
	}
	rr = request(s, http.MethodGet, "/api/v1/logs?service=web", token, nil)
	if rr.Code != 500 {
		t.Fatal("log error hidden")
	}
}
