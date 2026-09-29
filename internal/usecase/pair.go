package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

// PairingClient defines the RPC client port for pairing
type PairingClient interface {
	Pair(ctx context.Context, addr string, port uint16, pin, clientName string) (*entity.PairingResult, error)
}

// PairNodeUseCase handles client-side node pairing
type PairNodeUseCase struct {
	nodeRepo domain.NodeRepository
	client   PairingClient
}

// NewPairNodeUseCase initializes usecase
func NewPairNodeUseCase(repo domain.NodeRepository, client PairingClient) *PairNodeUseCase {
	return &PairNodeUseCase{
		nodeRepo: repo,
		client:   client,
	}
}

// Execute performs the pairing handshake and stores node credentials
func (uc *PairNodeUseCase) Execute(ctx context.Context, nodeName, addr, pin string) (*entity.Node, error) {
	if nodeName == "" {
		nodeName = "node-" + fmt.Sprintf("%d", time.Now().Unix()%1000)
	}

	result, err := uc.client.Pair(ctx, addr, 19800, pin, "kizuna-cli")
	if err != nil {
		return nil, fmt.Errorf("pairing request failed: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("pairing rejected by agent: %s", result.Error)
	}

	node := &entity.Node{
		ID:        result.NodeID,
		Name:      nodeName,
		Addr:      addr,
		AuthToken: result.AuthToken,
		IsOnline:  true,
		LastSeen:  time.Now(),
	}

	if err := uc.nodeRepo.SaveNode(node); err != nil {
		return nil, fmt.Errorf("failed to save paired node: %w", err)
	}

	return node, nil
}
