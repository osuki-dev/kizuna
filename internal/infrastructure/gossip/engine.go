package gossip

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
)

// Config holds initialization options for the Gossip Engine
type Config struct {
	NodeID         string
	NodeName       string
	MeshAddr       string
	Host           string
	IP             string
	Tags           []string
	DERP           *entity.DERPNodeInfo
	Interval       time.Duration // Periodic gossip ping interval (default: 2s)
	SuspectTimeout time.Duration // Time in suspect state before marking dead (default: 5s)
	SyncInterval   time.Duration // Anti-entropy full state sync interval (default: 20s)
	Transport      Transport
	Repo           domain.NodeRepository
	Seeds          []*entity.Node
}

// Engine implements a decentralized, SWIM-based Gossip membership and state synchronization engine
type Engine struct {
	mu            sync.RWMutex
	self          *entity.Node
	members       map[string]*entity.Node    // Keyed by Node.ID
	nameToID      map[string]string          // Maps Node.Name to Node.ID
	suspectTimers map[string]*time.Timer     // Keyed by Node.ID
	recentUpdates []*entity.GossipUpdate     // Piggybacked update queue
	tombstones    map[string]time.Time       // Deleted / kicked node IDs and Names
	failCount     map[string]int             // Node ID -> consecutive failed probe count
	onNodeUpdate  func(*entity.Node)
	transport     Transport
	repo          domain.NodeRepository
	interval      time.Duration
	suspectPeriod time.Duration
	syncInterval  time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	running       bool
	stats         struct {
		Sent   uint64
		Recv   uint64
		Rounds uint64
	}
}

var cgnatNet = &net.IPNet{
	IP:   net.ParseIP("100.64.0.0"),
	Mask: net.CIDRMask(10, 32),
}

func isVirtualOrVPN(ifName string) bool {
	lower := strings.ToLower(ifName)
	prefixes := []string{
		"tailscale", "docker", "br-", "veth", "tun", "tap", "utun", "wg", "virbr", "vmnet", "vbox",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// DetectOutboundIP determines the preferred local physical outbound IP for gossip/metadata,
// prioritizing real physical LAN interfaces (eno, eth, en, wlan) and ignoring VPN/Tailscale/Docker virtual networks.
func DetectOutboundIP() string {
	// 1. Try finding a physical LAN IPv4 interface first
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			if isVirtualOrVPN(iface.Name) {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
					continue
				}
				ipv4 := ipNet.IP.To4()
				if ipv4 == nil {
					continue
				}
				if cgnatNet.Contains(ipv4) || ipv4.IsLinkLocalUnicast() {
					continue
				}
				return ipv4.String()
			}
		}
	}

	// 2. Fallback to outbound dial if no physical LAN interface matched
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer func() { _ = conn.Close() }()
		if localAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok && localAddr.IP != nil {
			ipv4 := localAddr.IP.To4()
			if ipv4 != nil && !cgnatNet.Contains(ipv4) {
				return ipv4.String()
			}
		}
	}

	return ""
}

// NewEngine creates and initializes a new Gossip Engine
func NewEngine(cfg Config) *Engine {
	if cfg.Interval <= 0 {
		cfg.Interval = 3 * time.Second
	}
	if cfg.SuspectTimeout <= 0 {
		cfg.SuspectTimeout = 15 * time.Second
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 20 * time.Second
	}
	if cfg.NodeName == "" {
		cfg.NodeName, _ = os.Hostname()
		if cfg.NodeName == "" {
			cfg.NodeName = "kizuna-node"
		}
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "node_" + cfg.NodeName
	}
	if cfg.Host == "" {
		cfg.Host = cfg.NodeName
	}
	if cfg.IP == "" {
		cfg.IP = DetectOutboundIP()
	}

	self := &entity.Node{
		ID:          cfg.NodeID,
		Name:        cfg.NodeName,
		Addr:        cfg.MeshAddr,
		Host:        cfg.Host,
		IP:          cfg.IP,
		Tags:        cfg.Tags,
		DERP:        cfg.DERP,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		IsOnline:    true,
		GossipState: entity.GossipStateAlive,
		Incarnation: 1,
		LastSeen:    time.Now(),
	}

	e := &Engine{
		self:          self,
		members:       make(map[string]*entity.Node),
		nameToID:      make(map[string]string),
		suspectTimers: make(map[string]*time.Timer),
		recentUpdates: make([]*entity.GossipUpdate, 0, 50),
		tombstones:    make(map[string]time.Time),
		failCount:     make(map[string]int),
		transport:     cfg.Transport,
		repo:          cfg.Repo,
		interval:      cfg.Interval,
		suspectPeriod: cfg.SuspectTimeout,
		syncInterval:  cfg.SyncInterval,
	}

	// Register self
	e.members[self.ID] = self
	e.nameToID[self.Name] = self.ID

	// Load seeds from config or repository
	for _, seed := range cfg.Seeds {
		if seed != nil && seed.ID != self.ID && seed.Name != self.Name {
			sCopy := *seed
			if sCopy.GossipState == "" {
				sCopy.GossipState = entity.GossipStateAlive
			}
			e.members[sCopy.ID] = &sCopy
			e.nameToID[sCopy.Name] = sCopy.ID
		}
	}

	if cfg.Repo != nil {
		if nodes, err := cfg.Repo.ListNodes(); err == nil {
			for _, n := range nodes {
				if n != nil && n.ID != self.ID && n.Name != self.Name {
					nCopy := *n
					if nCopy.GossipState == "" {
						nCopy.GossipState = entity.GossipStateAlive
					}
					e.members[nCopy.ID] = &nCopy
					e.nameToID[nCopy.Name] = nCopy.ID
				}
			}
		}
	}

	return e
}

// Start launches background gossip workers
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return nil
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.running = true
	e.mu.Unlock()

	// Initial metrics refresh
	e.refreshTelemetry()

	// Worker 1: Periodic Gossip Round (Ping / Failure Detection)
	go e.runGossipLoop()

	// Worker 2: Anti-Entropy Full State Sync
	go e.runAntiEntropyLoop()

	// Worker 3: Periodic Telemetry Metrics Update
	go e.runTelemetryLoop()

	return nil
}

// Stop gracefully shuts down the Gossip Engine
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	if e.cancel != nil {
		e.cancel()
	}

	for _, timer := range e.suspectTimers {
		timer.Stop()
	}
	e.suspectTimers = make(map[string]*time.Timer)
	e.self.GossipState = entity.GossipStateLeft
	e.self.IsOnline = false
	e.mu.Unlock()

	return nil
}

// UpdateLocalMeta updates self tags, host, IP, increments incarnation and queues broadcast
func (e *Engine) UpdateLocalMeta(meta *entity.NodeMetaUpdate) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if meta == nil {
		return
	}

	e.self.Incarnation++
	e.self.LastSeen = time.Now()
	if len(meta.Tags) > 0 {
		e.self.Tags = meta.Tags
	}
	if meta.Host != "" {
		e.self.Host = meta.Host
	}
	if meta.IP != "" {
		e.self.IP = meta.IP
	}

	update := &entity.GossipUpdate{
		Node:        e.cloneNode(e.self),
		State:       entity.GossipStateAlive,
		Incarnation: e.self.Incarnation,
		Timestamp:   time.Now(),
	}
	e.queueUpdateLocked(update)
}

// UpdateLocalDERP updates self DERP relay information, increments incarnation and queues broadcast
func (e *Engine) UpdateLocalDERP(derp *entity.DERPNodeInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.self.DERP = derp
	e.self.Incarnation++
	e.self.LastSeen = time.Now()

	update := &entity.GossipUpdate{
		Node:        e.cloneNode(e.self),
		State:       entity.GossipStateAlive,
		Incarnation: e.self.Incarnation,
		Timestamp:   time.Now(),
	}
	e.queueUpdateLocked(update)
}

// SetOnNodeUpdate registers a callback invoked whenever a node's metadata or DERP status changes
func (e *Engine) SetOnNodeUpdate(fn func(*entity.Node)) {
	e.mu.Lock()
	e.onNodeUpdate = fn
	var currentMembers []*entity.Node
	for _, m := range e.members {
		if m != nil && m.ID != e.self.ID {
			currentMembers = append(currentMembers, e.cloneNode(m))
		}
	}
	e.mu.Unlock()

	if fn != nil {
		for _, m := range currentMembers {
			fn(m)
		}
	}
}

func (e *Engine) notifyNodeUpdateLocked(n *entity.Node) {
	if n == nil || e.onNodeUpdate == nil {
		return
	}
	cp := e.cloneNode(n)
	fn := e.onNodeUpdate
	go fn(cp)
}

// BroadcastUpdate queues an update for dissemination and applies locally
func (e *Engine) BroadcastUpdate(node *entity.Node) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if node == nil {
		return
	}
	update := &entity.GossipUpdate{
		Node:        e.cloneNode(node),
		State:       node.GossipState,
		Incarnation: node.Incarnation,
		Timestamp:   time.Now(),
	}
	e.applyUpdateLocked(update)
}

// GetMembers returns all known active members in the cluster
func (e *Engine) GetMembers() []*entity.Node {
	e.mu.RLock()
	defer e.mu.RUnlock()

	list := make([]*entity.Node, 0, len(e.members))
	for _, n := range e.members {
		if _, dead := e.tombstones[n.ID]; dead {
			continue
		}
		if _, dead := e.tombstones[n.Name]; dead {
			continue
		}
		list = append(list, e.cloneNode(n))
	}
	return list
}

// RemoveMember removes a member from the live mesh and registers a tombstone to prevent resurrection
func (e *Engine) RemoveMember(nameOrID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	id, ok := e.nameToID[nameOrID]
	if !ok {
		id = nameOrID
	}
	node, exists := e.members[id]
	if !exists {
		for mid, m := range e.members {
			if m.Name == nameOrID {
				id = mid
				node = m
				exists = true
				break
			}
		}
	}

	// Always tombstone by both ID and Name to reject incoming gossip
	e.tombstones[nameOrID] = time.Now()
	e.tombstones[id] = time.Now()
	delete(e.failCount, id)
	delete(e.failCount, nameOrID)

	if !exists || node == nil {
		if e.repo != nil {
			_ = e.repo.DeleteNode(nameOrID)
		}
		return false
	}

	if node.Name != "" {
		e.tombstones[node.Name] = time.Now()
	}

	if timer, ok := e.suspectTimers[id]; ok {
		timer.Stop()
		delete(e.suspectTimers, id)
	}

	delete(e.members, id)
	delete(e.nameToID, node.Name)

	// Broadcast dead update to mesh peers
	deadUpdate := &entity.GossipUpdate{
		Node:        e.cloneNode(node),
		State:       entity.GossipStateDead,
		Incarnation: node.Incarnation + 1,
		Timestamp:   time.Now(),
	}
	e.queueUpdateLocked(deadUpdate)

	if e.repo != nil {
		_ = e.repo.DeleteNode(node.Name)
		_ = e.repo.DeleteNode(node.ID)
	}

	return true
}

// AddOrUpdateMember adds or updates a member and clears any tombstone
func (e *Engine) AddOrUpdateMember(node *entity.Node) {
	if node == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if node.ID == "" && node.Name != "" {
		if id, ok := e.nameToID[node.Name]; ok {
			node.ID = id
		} else {
			node.ID = "node_" + node.Name
		}
	}
	if node.ID == "" {
		return
	}

	delete(e.tombstones, node.ID)
	if node.Name != "" {
		delete(e.tombstones, node.Name)
	}
	delete(e.failCount, node.ID)

	nCopy := e.cloneNode(node)
	if nCopy.GossipState == "" {
		nCopy.GossipState = entity.GossipStateAlive
	}
	nCopy.LastSeen = time.Now()
	e.members[nCopy.ID] = nCopy
	if nCopy.Name != "" {
		e.nameToID[nCopy.Name] = nCopy.ID
	}

	if e.repo != nil {
		_ = e.repo.SaveNode(e.cloneNode(nCopy))
	}
}

// ClearTombstone removes a node from the tombstones map
func (e *Engine) ClearTombstone(nameOrID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.tombstones, nameOrID)
	if id, ok := e.nameToID[nameOrID]; ok {
		delete(e.tombstones, id)
	}
}

// GetMember retrieves a single member by ID or Name
func (e *Engine) GetMember(nameOrID string) (*entity.Node, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if _, dead := e.tombstones[nameOrID]; dead {
		return nil, false
	}

	if n, ok := e.members[nameOrID]; ok {
		return e.cloneNode(n), true
	}
	if id, ok := e.nameToID[nameOrID]; ok {
		if _, dead := e.tombstones[id]; dead {
			return nil, false
		}
		if n, ok := e.members[id]; ok {
			return e.cloneNode(n), true
		}
	}
	return nil, false
}

// GetStatus returns the diagnostic health and statistics of the Gossip engine
func (e *Engine) GetStatus() entity.GossipEngineStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var alive, suspect, dead int
	members := make([]*entity.Node, 0, len(e.members))

	for _, n := range e.members {
		members = append(members, e.cloneNode(n))
		switch n.GossipState {
		case entity.GossipStateAlive:
			alive++
		case entity.GossipStateSuspect:
			suspect++
		case entity.GossipStateDead, entity.GossipStateLeft:
			dead++
		}
	}

	return entity.GossipEngineStatus{
		NodeID:       e.self.ID,
		NodeName:     e.self.Name,
		MeshAddr:     e.self.Addr,
		State:        e.self.GossipState,
		Incarnation:  e.self.Incarnation,
		Protocol:     "SWIM+AntiEntropy/v1",
		TotalMembers: len(e.members),
		AliveCount:   alive,
		SuspectCount: suspect,
		DeadCount:    dead,
		IntervalMs:   e.interval.Milliseconds(),
		Members:      members,
	}
}

// HandleMessage handles an incoming GossipMessage and returns the response
func (e *Engine) HandleMessage(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil gossip message")
	}

	e.mu.Lock()
	e.stats.Recv++
	e.mu.Unlock()

	switch msg.Type {
	case entity.GossipMsgPing:
		return e.handlePing(msg)
	case entity.GossipMsgAck:
		return e.handleAck(msg)
	case entity.GossipMsgIndirectPing:
		return e.handleIndirectPing(msg)
	case entity.GossipMsgSyncReq:
		return e.handleSyncReq(msg)
	case entity.GossipMsgSyncResp:
		return e.handleSyncResp(msg)
	case entity.GossipMsgUpdate:
		return e.handleUpdate(msg)
	default:
		return nil, fmt.Errorf("unknown gossip message type: %s", msg.Type)
	}
}

func (e *Engine) handlePing(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Update sender as alive
	e.registerOrTouchSenderLocked(msg)

	// Process piggybacked updates
	for _, u := range msg.Updates {
		e.applyUpdateLocked(u)
	}

	// Prepare Ack with piggybacked updates known to us
	updatesToSend := e.getPiggybackedUpdatesLocked(8)

	reply := &entity.GossipMessage{
		Type:        entity.GossipMsgAck,
		SenderID:    e.self.ID,
		SenderName:  e.self.Name,
		SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
		Incarnation: e.self.Incarnation,
		Updates:     updatesToSend,
	}
	return reply, nil
}

func (e *Engine) handleAck(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.registerOrTouchSenderLocked(msg)
	for _, u := range msg.Updates {
		e.applyUpdateLocked(u)
	}

	return &entity.GossipMessage{
		Type:       entity.GossipMsgAck,
		SenderID:   e.self.ID,
		SenderName: e.self.Name,
	}, nil
}

func (e *Engine) handleIndirectPing(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	targetID := msg.TargetID
	if targetID == "" {
		return nil, fmt.Errorf("missing target_id for indirect ping")
	}

	e.mu.RLock()
	target, exists := e.members[targetID]
	if !exists {
		if id, ok := e.nameToID[targetID]; ok {
			target, exists = e.members[id]
		}
	}
	var targetCopy *entity.Node
	if exists {
		targetCopy = e.cloneNode(target)
	}
	e.mu.RUnlock()

	if !exists || targetCopy == nil || targetCopy.Addr == "" {
		return &entity.GossipMessage{
			Type:       entity.GossipMsgAck,
			SenderID:   e.self.ID,
			SenderName: e.self.Name,
			Updates: []*entity.GossipUpdate{
				{
					Node:        targetCopy,
					State:       entity.GossipStateSuspect,
					Incarnation: 0,
					Timestamp:   time.Now(),
				},
			},
		}, nil
	}

	// Ping target on behalf of sender
	ctx, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
	defer cancel()

	pingMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    e.self.ID,
		SenderName:  e.self.Name,
		SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
		Incarnation: e.self.Incarnation,
	}

	reply, err := e.transport.SendMessage(ctx, targetCopy.Addr, 19800, pingMsg)
	if err == nil && reply != nil && reply.Type == entity.GossipMsgAck {
		return &entity.GossipMessage{
			Type:       entity.GossipMsgAck,
			SenderID:   e.self.ID,
			SenderName: e.self.Name,
			Updates: []*entity.GossipUpdate{
				{
					Node:        targetCopy,
					State:       entity.GossipStateAlive,
					Incarnation: targetCopy.Incarnation,
					Timestamp:   time.Now(),
				},
			},
		}, nil
	}

	return &entity.GossipMessage{
		Type:       entity.GossipMsgAck,
		SenderID:   e.self.ID,
		SenderName: e.self.Name,
		Updates: []*entity.GossipUpdate{
			{
				Node:        targetCopy,
				State:       entity.GossipStateSuspect,
				Incarnation: targetCopy.Incarnation,
				Timestamp:   time.Now(),
			},
		},
	}, nil
}

func (e *Engine) handleSyncReq(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.registerOrTouchSenderLocked(msg)

	digestMap := make(map[string]*entity.GossipDigest)
	for _, d := range msg.Digest {
		digestMap[d.NodeID] = d
	}

	var updatesToSend []*entity.GossipUpdate

	// 1. Check local members against requester digest
	for id, member := range e.members {
		remoteDigest, inRemote := digestMap[id]
		if !inRemote || member.Incarnation > remoteDigest.Incarnation ||
			(member.Incarnation == remoteDigest.Incarnation && member.GossipState != remoteDigest.State) {
			updatesToSend = append(updatesToSend, &entity.GossipUpdate{
				Node:        e.cloneNode(member),
				State:       member.GossipState,
				Incarnation: member.Incarnation,
				Timestamp:   time.Now(),
			})
		}
	}

	// 2. Prepare our own digest for requester to inspect
	localDigest := make([]*entity.GossipDigest, 0, len(e.members))
	for _, member := range e.members {
		localDigest = append(localDigest, &entity.GossipDigest{
			NodeID:      member.ID,
			NodeName:    member.Name,
			Incarnation: member.Incarnation,
			State:       member.GossipState,
		})
	}

	return &entity.GossipMessage{
		Type:        entity.GossipMsgSyncResp,
		SenderID:    e.self.ID,
		SenderName:  e.self.Name,
		SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
		Incarnation: e.self.Incarnation,
		Updates:     updatesToSend,
		Digest:      localDigest,
	}, nil
}

func (e *Engine) handleSyncResp(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.registerOrTouchSenderLocked(msg)
	for _, u := range msg.Updates {
		e.applyUpdateLocked(u)
	}

	return &entity.GossipMessage{
		Type:       entity.GossipMsgAck,
		SenderID:   e.self.ID,
		SenderName: e.self.Name,
	}, nil
}

func (e *Engine) handleUpdate(msg *entity.GossipMessage) (*entity.GossipMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.registerOrTouchSenderLocked(msg)
	for _, u := range msg.Updates {
		e.applyUpdateLocked(u)
	}

	return &entity.GossipMessage{
		Type:       entity.GossipMsgAck,
		SenderID:   e.self.ID,
		SenderName: e.self.Name,
	}, nil
}

func (e *Engine) registerOrTouchSenderLocked(msg *entity.GossipMessage) {
	if msg == nil || msg.SenderID == "" || msg.SenderID == e.self.ID {
		return
	}

	if _, dead := e.tombstones[msg.SenderID]; dead {
		return
	}
	if msg.SenderName != "" {
		if _, dead := e.tombstones[msg.SenderName]; dead {
			return
		}
	}
	delete(e.failCount, msg.SenderID)

	sender, exists := e.members[msg.SenderID]
	if !exists {
		if oldID, ok := e.nameToID[msg.SenderName]; ok && oldID != msg.SenderID {
			delete(e.members, oldID)
			delete(e.failCount, oldID)
			if t, ok := e.suspectTimers[oldID]; ok {
				t.Stop()
				delete(e.suspectTimers, oldID)
			}
		}
		sender = &entity.Node{
			ID:          msg.SenderID,
			Name:        msg.SenderName,
			Addr:        msg.SenderAddr,
			IsOnline:    true,
			GossipState: entity.GossipStateAlive,
			Incarnation: msg.Incarnation,
			LastSeen:    time.Now(),
		}
		e.members[msg.SenderID] = sender
		e.nameToID[msg.SenderName] = msg.SenderID

		if msg.SenderDERP != nil {
			derpCopy := *msg.SenderDERP
			sender.DERP = &derpCopy
		}
		if e.repo != nil {
			_ = e.repo.SaveNode(e.cloneNode(sender))
		}
		e.notifyNodeUpdateLocked(sender)
	} else {
		sender.IsOnline = true
		sender.GossipState = entity.GossipStateAlive
		sender.LastSeen = time.Now()
		if msg.SenderAddr != "" {
			sender.Addr = msg.SenderAddr
		}
		if msg.SenderDERP != nil {
			derpCopy := *msg.SenderDERP
			sender.DERP = &derpCopy
			e.notifyNodeUpdateLocked(sender)
		}
		if msg.Incarnation > sender.Incarnation {
			sender.Incarnation = msg.Incarnation
		}
		if timer, ok := e.suspectTimers[msg.SenderID]; ok {
			timer.Stop()
			delete(e.suspectTimers, msg.SenderID)
		}
	}
}

func (e *Engine) applyUpdateLocked(u *entity.GossipUpdate) {
	if u == nil || u.Node == nil || u.Node.ID == "" {
		return
	}

	if _, dead := e.tombstones[u.Node.ID]; dead {
		return
	}
	if u.Node.Name != "" {
		if _, dead := e.tombstones[u.Node.Name]; dead {
			return
		}
	}

	// 1. If update is about self
	if u.Node.ID == e.self.ID {
		if u.State == entity.GossipStateSuspect || u.State == entity.GossipStateDead {
			// We are alive! Refute suspect rumor by incrementing incarnation
			if u.Incarnation >= e.self.Incarnation {
				e.self.Incarnation = u.Incarnation + 1
			} else {
				e.self.Incarnation++
			}
			e.self.GossipState = entity.GossipStateAlive
			e.self.IsOnline = true
			e.self.LastSeen = time.Now()

			refute := &entity.GossipUpdate{
				Node:        e.cloneNode(e.self),
				State:       entity.GossipStateAlive,
				Incarnation: e.self.Incarnation,
				Timestamp:   time.Now(),
			}
			e.queueUpdateLocked(refute)
		}
		return
	}

	// 2. If update is about a peer
	existing, exists := e.members[u.Node.ID]
	if !exists {
		// Newly discovered peer via gossip!
		if oldID, ok := e.nameToID[u.Node.Name]; ok && oldID != u.Node.ID {
			delete(e.members, oldID)
			delete(e.failCount, oldID)
			if t, ok := e.suspectTimers[oldID]; ok {
				t.Stop()
				delete(e.suspectTimers, oldID)
			}
		}
		newNode := e.cloneNode(u.Node)
		newNode.GossipState = u.State
		newNode.Incarnation = u.Incarnation
		newNode.LastSeen = time.Now()
		if u.State == entity.GossipStateAlive {
			newNode.IsOnline = true
		} else {
			newNode.IsOnline = false
		}

		e.members[newNode.ID] = newNode
		e.nameToID[newNode.Name] = newNode.ID

		if e.repo != nil {
			_ = e.repo.SaveNode(e.cloneNode(newNode))
		}
		e.notifyNodeUpdateLocked(newNode)
		e.queueUpdateLocked(u)
		return
	}

	// Conflict resolution via SWIM rules:
	// A higher incarnation always supersedes a lower incarnation.
	if u.Incarnation > existing.Incarnation {
		existing.Incarnation = u.Incarnation
		existing.GossipState = u.State
		existing.LastSeen = time.Now()
		switch u.State {
		case entity.GossipStateAlive:
			existing.IsOnline = true
			if t, ok := e.suspectTimers[existing.ID]; ok {
				t.Stop()
				delete(e.suspectTimers, existing.ID)
			}
		case entity.GossipStateSuspect:
			existing.IsOnline = false
			e.startSuspectTimerLocked(existing.ID)
		case entity.GossipStateDead:
			existing.IsOnline = false
		}

		// Merge metadata & metrics
		e.mergeNodeMetadata(existing, u.Node)
		if e.repo != nil {
			_ = e.repo.SaveNode(e.cloneNode(existing))
		}
		e.notifyNodeUpdateLocked(existing)
		e.queueUpdateLocked(u)
	} else if u.Incarnation == existing.Incarnation {
		// Priority for equal incarnation: Dead > Suspect > Alive
		if existing.GossipState == entity.GossipStateAlive && u.State == entity.GossipStateSuspect {
			existing.GossipState = entity.GossipStateSuspect
			existing.IsOnline = false
			e.startSuspectTimerLocked(existing.ID)
			e.queueUpdateLocked(u)
		} else if existing.GossipState == entity.GossipStateSuspect && u.State == entity.GossipStateDead {
			existing.GossipState = entity.GossipStateDead
			existing.IsOnline = false
			if t, ok := e.suspectTimers[existing.ID]; ok {
				t.Stop()
				delete(e.suspectTimers, existing.ID)
			}
			e.queueUpdateLocked(u)
		}
		e.mergeNodeMetadata(existing, u.Node)
		if e.repo != nil {
			_ = e.repo.SaveNode(e.cloneNode(existing))
		}
		e.notifyNodeUpdateLocked(existing)
	}
}

func (e *Engine) mergeNodeMetadata(dest, src *entity.Node) {
	if src == nil || dest == nil {
		return
	}
	if len(src.Tags) > 0 {
		dest.Tags = src.Tags
	}
	if src.Host != "" {
		dest.Host = src.Host
	}
	if src.IP != "" {
		dest.IP = src.IP
	}
	if src.OS != "" {
		dest.OS = src.OS
	}
	if src.Arch != "" {
		dest.Arch = src.Arch
	}
	if src.Addr != "" {
		dest.Addr = src.Addr
	}
	if src.DERP != nil {
		derpCopy := *src.DERP
		dest.DERP = &derpCopy
	}
	if src.CPUUsage > 0 {
		dest.CPUUsage = src.CPUUsage
		dest.MemoryUsage = src.MemoryUsage
		dest.DiskUsage = src.DiskUsage
		dest.Load1 = src.Load1
		dest.Uptime = src.Uptime
		dest.CPUCores = src.CPUCores
	}
}

func (e *Engine) startSuspectTimerLocked(nodeID string) {
	if timer, ok := e.suspectTimers[nodeID]; ok {
		timer.Stop()
	}

	e.suspectTimers[nodeID] = time.AfterFunc(e.suspectPeriod, func() {
		e.mu.Lock()
		defer e.mu.Unlock()

		delete(e.suspectTimers, nodeID)
		if node, ok := e.members[nodeID]; ok && node.GossipState == entity.GossipStateSuspect {
			node.GossipState = entity.GossipStateDead
			node.IsOnline = false

			deadUpdate := &entity.GossipUpdate{
				Node:        e.cloneNode(node),
				State:       entity.GossipStateDead,
				Incarnation: node.Incarnation,
				Timestamp:   time.Now(),
			}
			e.queueUpdateLocked(deadUpdate)
		}
	})
}

func (e *Engine) queueUpdateLocked(u *entity.GossipUpdate) {
	if u == nil {
		return
	}
	if len(e.recentUpdates) >= 50 {
		e.recentUpdates = e.recentUpdates[1:]
	}
	e.recentUpdates = append(e.recentUpdates, u)
}

func (e *Engine) getPiggybackedUpdatesLocked(max int) []*entity.GossipUpdate {
	if len(e.recentUpdates) == 0 {
		return nil
	}
	if len(e.recentUpdates) <= max {
		cp := make([]*entity.GossipUpdate, len(e.recentUpdates))
		copy(cp, e.recentUpdates)
		return cp
	}
	start := len(e.recentUpdates) - max
	cp := make([]*entity.GossipUpdate, max)
	copy(cp, e.recentUpdates[start:])
	return cp
}

func (e *Engine) runGossipLoop() {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.performGossipRound()
		}
	}
}

func (e *Engine) performGossipRound() {
	if e.transport == nil {
		return
	}

	peer := e.selectRandomPeer()
	if peer == nil {
		return
	}

	e.mu.Lock()
	e.stats.Rounds++
	e.stats.Sent++
	updates := e.getPiggybackedUpdatesLocked(8)
	pingMsg := &entity.GossipMessage{
		Type:        entity.GossipMsgPing,
		SenderID:    e.self.ID,
		SenderName:  e.self.Name,
		SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
		Incarnation: e.self.Incarnation,
		Updates:     updates,
	}
	e.mu.Unlock()

	start := time.Now()
	ctx, cancel := context.WithTimeout(e.ctx, 12*time.Second)
	reply, err := e.transport.SendMessage(ctx, peer.Addr, 19800, pingMsg)
	cancel()

	latency := time.Since(start).Milliseconds()

	if err == nil && reply != nil && reply.Type == entity.GossipMsgAck {
		// Ping succeeded
		e.mu.Lock()
		delete(e.failCount, peer.ID)
		if n, ok := e.members[peer.ID]; ok {
			n.IsOnline = true
			n.GossipState = entity.GossipStateAlive
			n.LastSeen = time.Now()
			n.LatencyMs = latency
			if timer, exists := e.suspectTimers[peer.ID]; exists {
				timer.Stop()
				delete(e.suspectTimers, peer.ID)
			}
		}
		for _, u := range reply.Updates {
			e.applyUpdateLocked(u)
		}
		e.mu.Unlock()
		return
	}

	// Direct ping failed -> Try indirect ping via another peer
	indirectHelper := e.selectRandomPeerExcluding(peer.ID)
	if indirectHelper != nil {
		indCtx, indCancel := context.WithTimeout(e.ctx, 5000*time.Millisecond)
		indMsg := &entity.GossipMessage{
			Type:        entity.GossipMsgIndirectPing,
			SenderID:    e.self.ID,
			SenderName:  e.self.Name,
			SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
			TargetID:    peer.ID,
			Incarnation: e.self.Incarnation,
		}
		indReply, indErr := e.transport.SendMessage(indCtx, indirectHelper.Addr, 19800, indMsg)
		indCancel()

		if indErr == nil && indReply != nil {
			for _, u := range indReply.Updates {
				if u.Node != nil && u.Node.ID == peer.ID && u.State == entity.GossipStateAlive {
					// Indirect ping succeeded! Peer is alive through helper
					e.mu.Lock()
					delete(e.failCount, peer.ID)
					if n, ok := e.members[peer.ID]; ok {
						n.IsOnline = true
						n.GossipState = entity.GossipStateAlive
						n.LastSeen = time.Now()
					}
					e.mu.Unlock()
					return
				}
			}
		}
	}

	// Both direct and indirect ping failed: track consecutive failure count
	e.mu.Lock()
	e.failCount[peer.ID]++
	// Require at least 2 consecutive failed rounds before marking Suspect
	// to prevent transient WAN / DERP relay latency spikes from causing state flapping
	if e.failCount[peer.ID] >= 2 {
		if n, ok := e.members[peer.ID]; ok && n.GossipState == entity.GossipStateAlive {
			n.GossipState = entity.GossipStateSuspect
			n.IsOnline = false
			e.startSuspectTimerLocked(peer.ID)

			suspectUpdate := &entity.GossipUpdate{
				Node:        e.cloneNode(n),
				State:       entity.GossipStateSuspect,
				Incarnation: n.Incarnation,
				Timestamp:   time.Now(),
			}
			e.queueUpdateLocked(suspectUpdate)
		}
	}
	e.mu.Unlock()
}

func (e *Engine) runAntiEntropyLoop() {
	ticker := time.NewTicker(e.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.cleanupTombstones()
			e.reloadFromRepo()
			e.performAntiEntropySync()
		}
	}
}

func (e *Engine) cleanupTombstones() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for k, t := range e.tombstones {
		if now.Sub(t) > 24*time.Hour {
			delete(e.tombstones, k)
		}
	}
}

func (e *Engine) reloadFromRepo() {
	if e.repo == nil {
		return
	}
	nodes, err := e.repo.ListNodes()
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	validRepo := make(map[string]bool)
	for _, n := range nodes {
		if n != nil {
			validRepo[n.ID] = true
			if n.Name != "" {
				validRepo[n.Name] = true
			}
		}
	}

	// Purge any members from memory that were deleted from disk repository
	for id, m := range e.members {
		if id == e.self.ID || (m != nil && m.Name == e.self.Name) {
			continue
		}
		if !validRepo[id] && (m == nil || !validRepo[m.Name]) {
			delete(e.members, id)
			if m != nil {
				delete(e.nameToID, m.Name)
			}
			delete(e.failCount, id)
			if t, ok := e.suspectTimers[id]; ok {
				t.Stop()
				delete(e.suspectTimers, id)
			}
		}
	}

	for _, n := range nodes {
		if n == nil || n.ID == e.self.ID || n.Name == e.self.Name {
			continue
		}
		if _, dead := e.tombstones[n.ID]; dead {
			continue
		}
		if _, dead := e.tombstones[n.Name]; dead {
			continue
		}
		if existing, ok := e.members[n.ID]; ok {
			if n.Addr != "" && existing.Addr != n.Addr {
				existing.Addr = n.Addr
			}
			if len(n.Tags) > 0 {
				existing.Tags = n.Tags
			}
			if n.DERP != nil {
				derpCopy := *n.DERP
				existing.DERP = &derpCopy
			}
			e.notifyNodeUpdateLocked(existing)
		} else {
			nCopy := *n
			if nCopy.GossipState == "" {
				nCopy.GossipState = entity.GossipStateAlive
			}
			if n.DERP != nil {
				derpCopy := *n.DERP
				nCopy.DERP = &derpCopy
			}
			e.members[nCopy.ID] = &nCopy
			e.nameToID[nCopy.Name] = nCopy.ID
			e.notifyNodeUpdateLocked(&nCopy)
		}
	}
}

func (e *Engine) performAntiEntropySync() {
	if e.transport == nil {
		return
	}

	peer := e.selectRandomPeer()
	if peer == nil {
		return
	}

	e.mu.RLock()
	localDigest := make([]*entity.GossipDigest, 0, len(e.members))
	for _, m := range e.members {
		localDigest = append(localDigest, &entity.GossipDigest{
			NodeID:      m.ID,
			NodeName:    m.Name,
			Incarnation: m.Incarnation,
			State:       m.GossipState,
		})
	}
	req := &entity.GossipMessage{
		Type:        entity.GossipMsgSyncReq,
		SenderID:    e.self.ID,
		SenderName:  e.self.Name,
		SenderAddr:  e.self.Addr,
		SenderDERP:  e.self.DERP,
		Incarnation: e.self.Incarnation,
		Digest:      localDigest,
	}
	e.mu.RUnlock()

	ctx, cancel := context.WithTimeout(e.ctx, 5000*time.Millisecond)
	resp, err := e.transport.SendMessage(ctx, peer.Addr, 19800, req)
	cancel()

	if err == nil && resp != nil && resp.Type == entity.GossipMsgSyncResp {
		e.mu.Lock()
		for _, u := range resp.Updates {
			e.applyUpdateLocked(u)
		}
		e.mu.Unlock()
	}
}

func (e *Engine) runTelemetryLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.refreshTelemetry()
		}
	}
}

func (e *Engine) refreshTelemetry() {
	col := telemetry.NewCollector()
	metrics, err := col.Collect(context.Background())
	if err != nil || metrics == nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.self.CPUUsage = metrics.CPUUsage
	e.self.MemoryUsage = metrics.MemoryUsage
	e.self.DiskUsage = metrics.DiskUsage
	e.self.TotalMemory = metrics.TotalMemory
	e.self.UsedMemory = metrics.UsedMemory
	e.self.TotalDisk = metrics.TotalDisk
	e.self.UsedDisk = metrics.UsedDisk
	e.self.CPUCores = metrics.CPUCores
	e.self.Uptime = metrics.Uptime
	e.self.Load1 = metrics.Load1
	e.self.LastSeen = time.Now()
}

func (e *Engine) selectRandomPeer() *entity.Node {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var candidates []*entity.Node
	for _, m := range e.members {
		if m.ID != e.self.ID && (e.self.Name == "" || m.Name != e.self.Name) && m.Addr != "" && m.GossipState != entity.GossipStateDead {
			if _, dead := e.tombstones[m.ID]; dead {
				continue
			}
			if _, dead := e.tombstones[m.Name]; dead {
				continue
			}
			candidates = append(candidates, m)
		}
	}

	// If no alive peers available, probe dead peers to enable automatic node recovery
	if len(candidates) == 0 {
		for _, m := range e.members {
			if m.ID != e.self.ID && (e.self.Name == "" || m.Name != e.self.Name) && m.Addr != "" {
				if _, dead := e.tombstones[m.ID]; dead {
					continue
				}
				if _, dead := e.tombstones[m.Name]; dead {
					continue
				}
				candidates = append(candidates, m)
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	idx := rand.Intn(len(candidates))
	return e.cloneNode(candidates[idx])
}

func (e *Engine) selectRandomPeerExcluding(excludeID string) *entity.Node {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var candidates []*entity.Node
	for _, m := range e.members {
		if m.ID != e.self.ID && (e.self.Name == "" || m.Name != e.self.Name) && m.ID != excludeID && m.Addr != "" && m.GossipState == entity.GossipStateAlive {
			if _, dead := e.tombstones[m.ID]; dead {
				continue
			}
			if _, dead := e.tombstones[m.Name]; dead {
				continue
			}
			candidates = append(candidates, m)
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	idx := rand.Intn(len(candidates))
	return e.cloneNode(candidates[idx])
}

func (e *Engine) cloneNode(n *entity.Node) *entity.Node {
	if n == nil {
		return nil
	}
	cp := *n
	if len(n.Tags) > 0 {
		cp.Tags = make([]string, len(n.Tags))
		copy(cp.Tags, n.Tags)
	}
	if n.DERP != nil {
		derpCopy := *n.DERP
		cp.DERP = &derpCopy
	}
	return &cp
}
