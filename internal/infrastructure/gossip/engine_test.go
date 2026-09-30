package gossip_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/gossip"
)

// In-memory mock transport for testing gossip communication
type mockTransport struct {
	handlers map[string]func(*entity.GossipMessage) (*entity.GossipMessage, error)
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		handlers: make(map[string]func(*entity.GossipMessage) (*entity.GossipMessage, error)),
	}
}

func (m *mockTransport) Register(addr string, h func(*entity.GossipMessage) (*entity.GossipMessage, error)) {
	m.handlers[addr] = h
}

func (m *mockTransport) SendMessage(ctx context.Context, targetAddr string, port uint16, msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	h, ok := m.handlers[targetAddr]
	if !ok {
		return nil, fmt.Errorf("node at %s not found", targetAddr)
	}
	return h(msg)
}

func TestGossipPingAck(t *testing.T) {
	transport := newMockTransport()

	engineA := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "desktop-a",
		MeshAddr:  "10.0.0.1",
		Transport: transport,
	})
	transport.Register("10.0.0.1", engineA.HandleMessage)

	engineB := gossip.NewEngine(gossip.Config{
		NodeID:    "node_B",
		NodeName:  "server-b",
		MeshAddr:  "10.0.0.2",
		Transport: transport,
	})
	transport.Register("10.0.0.2", engineB.HandleMessage)

	// Node A sends Ping to Node B
	pingMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    "node_A",
		SenderName:  "desktop-a",
		SenderAddr:  "10.0.0.1",
		Incarnation: 1,
	}

	reply, err := engineB.HandleMessage(pingMsg)
	if err != nil {
		t.Fatalf("HandleMessage failed: %v", err)
	}

	if reply.Type != entity.GossipMsgAck {
		t.Fatalf("expected Ack, got %s", reply.Type)
	}

	// Verify Node B now knows Node A as alive member
	memberA, ok := engineB.GetMember("node_A")
	if !ok {
		t.Fatalf("expected Node B to discover Node A")
	}
	if memberA.GossipState != entity.GossipStateAlive {
		t.Fatalf("expected Node A to be Alive, got %s", memberA.GossipState)
	}
}

func TestGossipMultiNodeAutoDiscovery(t *testing.T) {
	// Scenario: Node A knows Node B. Node B knows Node C.
	// Through Anti-Entropy Sync between A and B, Node A automatically discovers Node C!
	transport := newMockTransport()

	engineA := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "desktop-a",
		MeshAddr:  "10.0.0.1",
		Transport: transport,
	})
	transport.Register("10.0.0.1", engineA.HandleMessage)

	engineB := gossip.NewEngine(gossip.Config{
		NodeID:    "node_B",
		NodeName:  "mac-mini-b",
		MeshAddr:  "10.0.0.2",
		Transport: transport,
	})
	transport.Register("10.0.0.2", engineB.HandleMessage)

	// Node B learns about Node C
	nodeC := &entity.Node{
		ID:          "node_C",
		Name:        "cloud-c",
		Addr:        "10.0.0.3",
		Host:        "cloud.lan",
		IP:          "10.0.0.3",
		Tags:        []string{"prod", "web"},
		GossipState: entity.GossipStateAlive,
		Incarnation: 2,
		IsOnline:    true,
	}
	engineB.BroadcastUpdate(nodeC)

	// Node A sends SyncReq to Node B
	syncReq := &entity.GossipMessage{
		Type:        entity.GossipMsgSyncReq,
		SenderID:    "node_A",
		SenderName:  "desktop-a",
		SenderAddr:  "10.0.0.1",
		Incarnation: 1,
		Digest: []*entity.GossipDigest{
			{NodeID: "node_A", NodeName: "desktop-a", Incarnation: 1, State: entity.GossipStateAlive},
		},
	}

	syncResp, err := engineB.HandleMessage(syncReq)
	if err != nil {
		t.Fatalf("HandleMessage sync req failed: %v", err)
	}

	// Node A processes Node B's SyncResp
	_, err = engineA.HandleMessage(syncResp)
	if err != nil {
		t.Fatalf("HandleMessage sync resp failed: %v", err)
	}

	// Verify Node A now automatically knows Node C and its tags/host!
	discoveredC, ok := engineA.GetMember("node_C")
	if !ok {
		t.Fatalf("expected Node A to automatically discover Node C via Node B")
	}
	if discoveredC.Host != "cloud.lan" {
		t.Errorf("expected host 'cloud.lan', got '%s'", discoveredC.Host)
	}
	if len(discoveredC.Tags) != 2 || discoveredC.Tags[0] != "prod" {
		t.Errorf("expected tags [prod web], got %v", discoveredC.Tags)
	}
}

func TestGossipSuspectAndRefutation(t *testing.T) {
	transport := newMockTransport()

	engineA := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "desktop-a",
		MeshAddr:  "10.0.0.1",
		Transport: transport,
	})

	// Rumor arrives claiming Node A is Suspect with Incarnation 1
	suspectMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    "node_B",
		SenderName:  "remote-b",
		SenderAddr:  "10.0.0.2",
		Incarnation: 1,
		Updates: []*entity.GossipUpdate{
			{
				Node:        &entity.Node{ID: "node_A", Name: "desktop-a"},
				State:       entity.GossipStateSuspect,
				Incarnation: 1,
				Timestamp:   time.Now(),
			},
		},
	}

	reply, err := engineA.HandleMessage(suspectMsg)
	if err != nil {
		t.Fatalf("HandleMessage failed: %v", err)
	}

	// Node A must refute by incrementing incarnation and declaring itself Alive in Ack!
	st := engineA.GetStatus()
	if st.State != entity.GossipStateAlive {
		t.Fatalf("expected Node A to remain Alive, got %s", st.State)
	}
	if st.Incarnation <= 1 {
		t.Fatalf("expected Node A incarnation to be incremented for refutation, got %d", st.Incarnation)
	}

	// The Ack must include Node A's refutation update
	var foundRefutation bool
	for _, u := range reply.Updates {
		if u.Node != nil && u.Node.ID == "node_A" && u.State == entity.GossipStateAlive {
			foundRefutation = true
			if u.Incarnation != st.Incarnation {
				t.Errorf("expected refutation incarnation %d, got %d", st.Incarnation, u.Incarnation)
			}
		}
	}
	if !foundRefutation {
		t.Fatalf("expected reply Ack to contain Alive refutation update for node_A")
	}
}

type mockRepo struct {
	saved map[string]*entity.Node
}

func newMockRepo() *mockRepo {
	return &mockRepo{saved: make(map[string]*entity.Node)}
}

func (m *mockRepo) GetNode(name string) (*entity.Node, error) {
	if n, ok := m.saved[name]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockRepo) SaveNode(node *entity.Node) error {
	m.saved[node.Name] = node
	return nil
}

func (m *mockRepo) ListNodes() ([]*entity.Node, error) {
	var list []*entity.Node
	for _, n := range m.saved {
		list = append(list, n)
	}
	return list, nil
}

func (m *mockRepo) DeleteNode(name string) error {
	delete(m.saved, name)
	return nil
}

func TestGossipRepoPersistence(t *testing.T) {
	repo := newMockRepo()
	transport := newMockTransport()

	engine := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "desktop-a",
		MeshAddr:  "10.0.0.1",
		Transport: transport,
		Repo:      repo,
	})

	// When a new node is discovered via Gossip
	updateMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgUpdate,
		SenderID:    "node_B",
		SenderName:  "mac-mini",
		SenderAddr:  "10.0.0.2",
		Incarnation: 1,
		Updates: []*entity.GossipUpdate{
			{
				Node: &entity.Node{
					ID:       "node_B",
					Name:     "mac-mini",
					Addr:     "10.0.0.2",
					Host:     "mini.lan",
					IP:       "10.0.0.2",
					Tags:     []string{"mac", "server"},
					IsOnline: true,
				},
				State:       entity.GossipStateAlive,
				Incarnation: 1,
				Timestamp:   time.Now(),
			},
		},
	}

	_, err := engine.HandleMessage(updateMsg)
	if err != nil {
		t.Fatalf("HandleMessage failed: %v", err)
	}

	// Verify that the discovered node was automatically persisted into repo!
	savedNode, err := repo.GetNode("mac-mini")
	if err != nil {
		t.Fatalf("expected node 'mac-mini' to be persisted in repo: %v", err)
	}
	if savedNode.Host != "mini.lan" {
		t.Errorf("expected host 'mini.lan', got '%s'", savedNode.Host)
	}
	if len(savedNode.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(savedNode.Tags))
	}
}

func TestGossipRemoveMemberAndTombstone(t *testing.T) {
	repo := newMockRepo()
	transport := newMockTransport()

	engine := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "desktop-a",
		MeshAddr:  "10.0.0.1",
		Transport: transport,
		Repo:      repo,
	})

	// Add node_B
	engine.AddOrUpdateMember(&entity.Node{
		ID:          "node_B",
		Name:        "mac-mini",
		Addr:        "10.0.0.2",
		IsOnline:    true,
		GossipState: entity.GossipStateAlive,
	})

	members := engine.GetMembers()
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Remove node_B
	removed := engine.RemoveMember("mac-mini")
	if !removed {
		t.Fatalf("expected RemoveMember to return true")
	}

	membersAfter := engine.GetMembers()
	if len(membersAfter) != 1 {
		t.Fatalf("expected 1 member after removal, got %d", len(membersAfter))
	}
	if _, ok := engine.GetMember("mac-mini"); ok {
		t.Fatalf("expected mac-mini to be gone from GetMember")
	}

	// An incoming message from removed node should be rejected (no resurrection)
	pingMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    "node_B",
		SenderName:  "mac-mini",
		SenderAddr:  "10.0.0.2",
		Incarnation: 5,
	}
	_, _ = engine.HandleMessage(pingMsg)

	if len(engine.GetMembers()) != 1 {
		t.Fatalf("expected tombstone to prevent node_B from resurrecting, got %d members", len(engine.GetMembers()))
	}
}

func TestGossipDERPDissemination(t *testing.T) {
	transport := newMockTransport()

	derpInfo := &entity.DERPNodeInfo{
		RegionID:   901,
		RegionCode: "tokyo",
		RegionName: "Tokyo Relay",
		HostName:   "10.0.0.1",
		Port:       8443,
		STUNPort:   3478,
		CertName:   "sha256-raw:fakefingerprint",
	}

	engineA := gossip.NewEngine(gossip.Config{
		NodeID:    "node_A",
		NodeName:  "relay-node",
		MeshAddr:  "10.0.0.1",
		DERP:      derpInfo,
		Transport: transport,
	})
	transport.Register("10.0.0.1", engineA.HandleMessage)

	engineB := gossip.NewEngine(gossip.Config{
		NodeID:    "node_B",
		NodeName:  "client-node",
		MeshAddr:  "10.0.0.2",
		Transport: transport,
	})
	transport.Register("10.0.0.2", engineB.HandleMessage)

	derpDiscoveredCh := make(chan *entity.DERPNodeInfo, 1)
	engineB.SetOnNodeUpdate(func(n *entity.Node) {
		if n != nil && n.DERP != nil {
			select {
			case derpDiscoveredCh <- n.DERP:
			default:
			}
		}
	})

	// Node A sends Gossip update to Node B containing its DERP capability
	updateMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    "node_A",
		SenderName:  "relay-node",
		SenderAddr:  "10.0.0.1",
		Incarnation: 1,
		Updates: []*entity.GossipUpdate{
			{
				Node: &entity.Node{
					ID:       "node_A",
					Name:     "relay-node",
					Addr:     "10.0.0.1",
					Host:     "10.0.0.1",
					IP:       "10.0.0.1",
					DERP:     derpInfo,
					IsOnline: true,
				},
				State:       entity.GossipStateAlive,
				Incarnation: 1,
				Timestamp:   time.Now(),
			},
		},
	}

	reply, err := engineB.HandleMessage(updateMsg)
	if err != nil {
		t.Fatalf("HandleMessage failed: %v", err)
	}
	if reply.Type != entity.GossipMsgAck {
		t.Fatalf("expected Ack reply, got %s", reply.Type)
	}

	// Verify Node B discovered Node A's DERP relay via callback
	select {
	case discovered := <-derpDiscoveredCh:
		if discovered.RegionID != 901 || discovered.RegionCode != "tokyo" || discovered.Port != 8443 {
			t.Fatalf("unexpected discovered DERP info: %+v", discovered)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for DERP discovery callback")
	}

	// Verify GetMember also returns the DERP metadata
	memberA, ok := engineB.GetMember("node_A")
	if !ok {
		t.Fatalf("expected memberA to be present in engineB")
	}
	if memberA.DERP == nil || memberA.DERP.RegionID != 901 {
		t.Fatalf("expected memberA to have DERP info, got %+v", memberA.DERP)
	}
}



