package gossip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// Transport defines the communication interface for sending Gossip protocol messages
type Transport interface {
	SendMessage(ctx context.Context, targetAddr string, port uint16, msg *entity.GossipMessage) (*entity.GossipMessage, error)
}

// MeshTransport implements Transport using Kizuna's MeshGateway (Tailcat / TCP)
type MeshTransport struct {
	mesh domain.MeshGateway
}

// NewMeshTransport creates a new MeshTransport
func NewMeshTransport(mesh domain.MeshGateway) *MeshTransport {
	return &MeshTransport{mesh: mesh}
}

// SendMessage sends an HTTP POST request carrying the GossipMessage envelope to the target node
func (t *MeshTransport) SendMessage(ctx context.Context, targetAddr string, port uint16, msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	if port == 0 {
		port = 19800
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gossip message: %w", err)
	}

	httpClient := &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return t.mesh.Dial(ctx, targetAddr, port)
			},
		},
	}

	url := "http://node/api/v1/node/sync"
	if strings.Contains(targetAddr, ":") || strings.HasPrefix(targetAddr, "127.0.0.1") || strings.HasPrefix(targetAddr, "localhost") {
		hostOnly := targetAddr
		if strings.Contains(hostOnly, ":") {
			hostOnly, _, _ = net.SplitHostPort(targetAddr)
		}
		url = fmt.Sprintf("http://%s:%d/api/v1/node/sync", hostOnly, port)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gossip message returned HTTP status %d", resp.StatusCode)
	}

	var reply entity.GossipMessage
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("failed to decode gossip reply: %w", err)
	}

	return &reply, nil
}
