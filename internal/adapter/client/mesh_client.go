package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// MeshClient implements usecase.PairingClient and usecase.NodeClient
type MeshClient struct {
	mesh domain.MeshGateway
}

// NewMeshClient initializes MeshClient
func NewMeshClient(mesh domain.MeshGateway) *MeshClient {
	return &MeshClient{mesh: mesh}
}

func (c *MeshClient) getHTTPClient(ctx context.Context, addr string, port uint16) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return c.mesh.Dial(ctx, addr, port)
			},
		},
	}
}

// Pair sends pairing credentials to remote agent
func (c *MeshClient) Pair(ctx context.Context, addr string, port uint16, pin, clientName string) (*entity.PairingResult, error) {
	client := c.getHTTPClient(ctx, addr, port)

	payload := entity.PairingPayload{
		ClientName: clientName,
		PIN:        pin,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/pair", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var result entity.PairingResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// DeployService uploads manifest and artifact to agent
func (c *MeshClient) DeployService(ctx context.Context, node *entity.Node, svc *entity.Service, artifact io.Reader) error {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// 1. Add manifest JSON
	manifestBytes, err := json.Marshal(svc)
	if err != nil {
		return err
	}
	if err := writer.WriteField("manifest", string(manifestBytes)); err != nil {
		return err
	}

	// 2. Add artifact if present
	if artifact != nil {
		part, err := writer.CreateFormFile("artifact", "artifact.tar.gz")
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, artifact); err != nil {
			return err
		}
	}
	_ = writer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/deploy", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}

// ConfigureIngress updates Caddy routes on agent
func (c *MeshClient) ConfigureIngress(ctx context.Context, node *entity.Node, ingress *entity.IngressConfig) error {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	bodyBytes, _ := json.Marshal(ingress)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/ingress", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ingress failed: %s", string(b))
	}
	return nil
}

// GetStatus retrieves node stats and service statuses
func (c *MeshClient) GetStatus(ctx context.Context, node *entity.Node) (*entity.Node, []*entity.Service, error) {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/api/v1/status", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var data struct {
		Node     entity.Node       `json:"node"`
		Services []*entity.Service `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, nil, err
	}

	return &data.Node, data.Services, nil
}

// StreamLogs streams logs from agent to writer
func (c *MeshClient) StreamLogs(ctx context.Context, node *entity.Node, serviceName string, writer io.Writer) error {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	url := fmt.Sprintf("http://node/api/v1/logs?service=%s", serviceName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	_, err = io.Copy(writer, resp.Body)
	return err
}

// TriggerBackup sends backup request to remote agent
func (c *MeshClient) TriggerBackup(ctx context.Context, node *entity.Node, serviceName string, cfg *entity.BackupConfig) (*entity.BackupRecord, error) {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	reqPayload := map[string]any{
		"service": serviceName,
		"config":  cfg,
	}
	bodyBytes, _ := json.Marshal(reqPayload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/backup", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("backup failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var record entity.BackupRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// UpdateNodeMeta updates remote node's metadata (tags, host, ip)
func (c *MeshClient) UpdateNodeMeta(ctx context.Context, node *entity.Node, meta entity.NodeMetaUpdate) error {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	bodyBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/node/meta", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remote update failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// RevokeClient requests the remote node to revoke a client's access
func (c *MeshClient) RevokeClient(ctx context.Context, node *entity.Node, nameOrID string) error {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	payload := map[string]string{"name_or_id": nameOrID}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/auth/revoke", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.AuthToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revocation failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// SendNodeSync sends mesh synchronization payload to remote node
func (c *MeshClient) SendNodeSync(ctx context.Context, node *entity.Node, msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	bodyBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/v1/node/sync", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if node.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+node.AuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sync failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var reply entity.GossipMessage
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// GetNodeMembers fetches all cluster members discovered by remote node
func (c *MeshClient) GetNodeMembers(ctx context.Context, node *entity.Node) ([]*entity.Node, error) {
	client := c.getHTTPClient(ctx, node.Addr, 19800)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/api/v1/node/members", nil)
	if err != nil {
		return nil, err
	}
	if node.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+node.AuthToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get node members (status %d)", resp.StatusCode)
	}

	var members []*entity.Node
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, err
	}
	return members, nil
}
