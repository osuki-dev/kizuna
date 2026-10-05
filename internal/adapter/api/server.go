package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
)

// GossipEngine defines the interface required by API Server to interact with the Gossip subsystem
type GossipEngine interface {
	HandleMessage(msg *entity.GossipMessage) (*entity.GossipMessage, error)
	GetMembers() []*entity.Node
	GetStatus() entity.GossipEngineStatus
	UpdateLocalMeta(meta *entity.NodeMetaUpdate)
	RemoveMember(nameOrID string) bool
	AddOrUpdateMember(node *entity.Node)
}

// Server handles incoming RPC requests to the Agent over mesh or local TCP
type Server struct {
	auth           domain.AuthManager
	workload       domain.WorkloadRunner
	ingress        domain.IngressManager
	backup         domain.BackupManager
	gossip         GossipEngine
	nodeID         string
	nodeName       string
	mu             sync.RWMutex
	metaPath       string
	meta           entity.NodeMetaUpdate
	handlerOnce    sync.Once
	cachedHandler  http.Handler
	maxUploadBytes int64
	gossipToken    string
}

// NewServer initializes the Agent API HTTP Server
func NewServer(
	auth domain.AuthManager,
	workload domain.WorkloadRunner,
	ingress domain.IngressManager,
	backup domain.BackupManager,
	nodeID, nodeName string,
) *Server {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".kizuna")
	_ = os.MkdirAll(configDir, 0700)
	metaPath := filepath.Join(configDir, "node_meta.json")

	s := &Server{
		auth:           auth,
		workload:       workload,
		ingress:        ingress,
		backup:         backup,
		nodeID:         nodeID,
		nodeName:       nodeName,
		metaPath:       metaPath,
		maxUploadBytes: 500 << 20, // 500MB default
	}
	s.loadMeta()
	return s
}

// SetMaxUploadBytes sets maximum allowable multipart upload size for deployment artifacts
func (s *Server) SetMaxUploadBytes(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes > 0 {
		s.maxUploadBytes = bytes
	}
}

// SetGossipToken configures a dedicated shared secret for node synchronization.
// The secret does not authorize application or membership management endpoints.
func (s *Server) SetGossipToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gossipToken = token
}

// SetGossipEngine attaches an active Gossip engine to the API Server
func (s *Server) SetGossipEngine(g GossipEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gossip = g
}

func (s *Server) loadMeta() {
	if data, err := os.ReadFile(s.metaPath); err == nil {
		_ = json.Unmarshal(data, &s.meta)
	}
}

func (s *Server) saveMetaLocked() error {
	data, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath, data, 0600)
}

// Handler returns the HTTP handler with all registered routes and auth middleware.
// The handler is built once and cached for all subsequent calls.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		mux := http.NewServeMux()

		// Public endpoint for pairing
		mux.HandleFunc("/api/v1/pair", s.handlePair)

		// Protected endpoints
		mux.HandleFunc("/api/v1/deploy", s.withAuth(s.handleDeploy))
		mux.HandleFunc("/api/v1/ingress", s.withAuth(s.handleIngress))
		mux.HandleFunc("/api/v1/status", s.withAuth(s.handleStatus))
		mux.HandleFunc("/api/v1/logs", s.withAuth(s.handleLogs))
		mux.HandleFunc("/api/v1/backup", s.withAuth(s.handleBackup))
		mux.HandleFunc("/api/v1/node/meta", s.withAuth(s.handleNodeMeta))
		mux.HandleFunc("/api/v1/auth/revoke", s.withAuth(s.handleRevoke))

		// Internal node mesh synchronization & member discovery
		mux.HandleFunc("/api/v1/node/sync", s.withSyncAuth(s.handleNodeSync))
		mux.HandleFunc("/api/v1/node/members", s.withAuth(s.handleNodeMembers))
		mux.HandleFunc("/api/v1/node/remove", s.withAuth(s.handleNodeRemove))
		mux.HandleFunc("/api/v1/node/add", s.withAuth(s.handleNodeAdd))

		s.cachedHandler = mux
	})
	return s.cachedHandler
}

// ServeConn dispatches a single raw net.Conn (from tailcat) to http.Server
func (s *Server) ServeConn(conn net.Conn) {
	srv := &http.Server{
		Handler: s.Handler(),
	}
	l := newSingleConnListener(conn)
	defer func() { _ = l.Close() }()
	_ = srv.Serve(l)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok || token == "" || s.auth == nil || !s.auth.ValidateTokenFromAddr(token, r.RemoteAddr) {
			http.Error(w, "unauthorized: invalid or missing bearer token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req entity.PairingPayload
	if err := decodeRequest(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	token, err := s.auth.VerifyPIN(req.PIN, req.ClientName)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(entity.PairingResult{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entity.PairingResult{
		Success:   true,
		NodeID:    s.nodeID,
		NodeName:  s.nodeName,
		AuthToken: token,
	})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	limit := s.maxUploadBytes
	s.mu.RUnlock()
	if limit <= 0 {
		limit = 500 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	// This is a memory threshold, not the request size limit.
	err := r.ParseMultipartForm(8 << 20)
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "deployment upload exceeds size limit", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
		}
		return
	}

	manifestJSON := r.FormValue("manifest")
	var svc entity.Service
	if err := decodeManifest(manifestJSON, &svc); err != nil {
		http.Error(w, fmt.Sprintf("invalid service manifest: %v", err), http.StatusBadRequest)
		return
	}

	if err := validateService(&svc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var artifactReader io.Reader
	file, _, err := r.FormFile("artifact")
	if err == nil {
		defer func() { _ = file.Close() }()
		artifactReader = file
	} else if !errors.Is(err, http.ErrMissingFile) {
		http.Error(w, "invalid artifact", http.StatusBadRequest)
		return
	}

	if err := s.workload.Deploy(r.Context(), &svc, artifactReader); err != nil {
		http.Error(w, fmt.Sprintf("deployment failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "deployed", "service": svc.Name, "image": svc.Image})
}

func (s *Server) handleIngress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg entity.IngressConfig
	if err := decodeRequest(w, r, &cfg); err != nil {
		http.Error(w, "invalid ingress config", http.StatusBadRequest)
		return
	}

	if err := s.ingress.ConfigureRoute(r.Context(), &cfg); err != nil {
		http.Error(w, fmt.Sprintf("ingress error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	services, err := s.workload.ListServices(r.Context())
	if err != nil {
		http.Error(w, "failed to list services", http.StatusInternalServerError)
		return
	}
	safeServices := make([]*entity.Service, 0, len(services))
	for _, svc := range services {
		safeServices = append(safeServices, PublicService(svc))
	}

	col := telemetry.NewCollector()
	metrics, _ := col.Collect(r.Context())

	node := entity.Node{
		ID:       s.nodeID,
		Name:     s.nodeName,
		IsOnline: true,
		LastSeen: time.Now(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}

	if metrics != nil {
		if metrics.OS != "" {
			node.OS = metrics.OS
		}
		if metrics.Arch != "" {
			node.Arch = metrics.Arch
		}
		node.CPUUsage = metrics.CPUUsage
		node.MemoryUsage = metrics.MemoryUsage
		node.DiskUsage = metrics.DiskUsage
		node.TotalMemory = metrics.TotalMemory
		node.UsedMemory = metrics.UsedMemory
		node.TotalDisk = metrics.TotalDisk
		node.UsedDisk = metrics.UsedDisk
		node.CPUCores = metrics.CPUCores
		node.Uptime = metrics.Uptime
		node.Load1 = metrics.Load1
	}

	s.mu.RLock()
	node.Tags = s.meta.Tags
	node.Host = s.meta.Host
	node.IP = s.meta.IP
	if node.Host == "" {
		node.Host = s.nodeName
	}
	if s.gossip != nil {
		node.GossipState = entity.GossipStateAlive
		st := s.gossip.GetStatus()
		node.Incarnation = st.Incarnation
	} else {
		node.GossipState = entity.GossipStateAlive
		node.Incarnation = 1
	}
	s.mu.RUnlock()

	resp := map[string]any{
		"node":     node,
		"services": safeServices,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svcName := r.URL.Query().Get("service")
	if entity.ValidateServiceName(svcName) != nil {
		http.Error(w, "invalid service query param", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	if err := s.workload.StreamLogs(r.Context(), svcName, w); err != nil {
		http.Error(w, "failed to stream service logs", http.StatusInternalServerError)
	}
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Service string               `json:"service"`
		Config  *entity.BackupConfig `json:"config"`
	}
	if err := decodeRequest(w, r, &req); err != nil {
		http.Error(w, "invalid backup payload", http.StatusBadRequest)
		return
	}

	if entity.ValidateServiceName(req.Service) != nil || req.Config == nil {
		http.Error(w, "invalid service or backup config", http.StatusBadRequest)
		return
	}
	record, err := s.backup.CreateBackup(r.Context(), req.Service, req.Config)
	if err != nil {
		http.Error(w, fmt.Sprintf("backup failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(record)
}

func (s *Server) handleNodeMeta(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id":   s.nodeID,
			"node_name": s.nodeName,
			"tags":      s.meta.Tags,
			"host":      s.meta.Host,
			"ip":        s.meta.IP,
		})
	case http.MethodPost:
		var update entity.NodeMetaUpdate
		if err := decodeRequest(w, r, &update); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		previousMeta := s.meta
		if update.Tags != nil {
			s.meta.Tags = update.Tags
		}
		if update.Host != "" {
			s.meta.Host = update.Host
			if s.meta.IP == "" {
				s.meta.IP = update.Host
			}
		}
		if update.IP != "" {
			s.meta.IP = update.IP
			if s.meta.Host == "" {
				s.meta.Host = update.IP
			}
		}
		if err := s.saveMetaLocked(); err != nil {
			s.meta = previousMeta
			s.mu.Unlock()
			http.Error(w, "failed to persist node metadata", http.StatusInternalServerError)
			return
		}
		currentMeta := s.meta
		ge := s.gossip
		s.mu.Unlock()

		if ge != nil {
			ge.UpdateLocalMeta(&currentMeta)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"meta":    currentMeta,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		NameOrID string `json:"name_or_id"`
		Token    string `json:"token"`
	}
	if err := decodeRequest(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var err error
	if req.Token != "" {
		err = s.auth.RevokeToken(req.Token)
	} else if req.NameOrID != "" {
		err = s.auth.RevokeClient(req.NameOrID)
	} else {
		http.Error(w, "name_or_id or token is required", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("revoke failed: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

// trackingConn calls onClose when the connection is closed
type trackingConn struct {
	net.Conn
	closeOnce sync.Once
	onClose   func()
}

func (c *trackingConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
		err = c.Conn.Close()
	})
	return err
}

// singleConnListener allows serving an http.Server over a single net.Conn
type singleConnListener struct {
	conn      net.Conn
	done      bool
	closeChan chan struct{}
	closeOnce sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	l := &singleConnListener{
		closeChan: make(chan struct{}),
	}
	l.conn = &trackingConn{
		Conn: conn,
		onClose: func() {
			_ = l.Close()
		},
	}
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if !l.done {
		l.done = true
		return l.conn, nil
	}
	<-l.closeChan
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closeChan)
	})
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}

func (s *Server) handleNodeSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	ge := s.gossip
	s.mu.RUnlock()

	if ge == nil {
		http.Error(w, "mesh engine not active", http.StatusServiceUnavailable)
		return
	}

	var msg entity.GossipMessage
	if err := decodeRequest(w, r, &msg); err != nil {
		http.Error(w, "invalid sync message payload", http.StatusBadRequest)
		return
	}

	for _, update := range msg.Updates {
		if update != nil {
			update.Node = PublicNode(update.Node)
		}
	}
	reply, err := ge.HandleMessage(&msg)
	if err != nil {
		http.Error(w, fmt.Sprintf("mesh sync error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if reply != nil {
		safeReply := *reply
		safeReply.Updates = make([]*entity.GossipUpdate, 0, len(reply.Updates))
		for _, update := range reply.Updates {
			if update != nil {
				copy := *update
				copy.Node = PublicNode(update.Node)
				safeReply.Updates = append(safeReply.Updates, &copy)
			}
		}
		reply = &safeReply
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (s *Server) handleNodeMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	ge := s.gossip
	s.mu.RUnlock()

	if ge == nil {
		http.Error(w, "mesh engine not active", http.StatusServiceUnavailable)
		return
	}

	members := ge.GetMembers()
	safeMembers := make([]*entity.Node, 0, len(members))
	for _, member := range members {
		safeMembers = append(safeMembers, PublicNode(member))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(safeMembers)
}

func (s *Server) handleNodeRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := decodeRequest(w, r, &req); err != nil {
		http.Error(w, "invalid json request", http.StatusBadRequest)
		return
	}

	target := req.Name
	if target == "" {
		target = req.ID
	}
	if target == "" {
		http.Error(w, "name or id is required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	ge := s.gossip
	s.mu.RUnlock()

	var removed bool
	if ge != nil {
		removed = ge.RemoveMember(target)
	}

	if s.auth != nil {
		if req.Name != "" {
			_ = s.auth.RevokeClient(req.Name)
		}
		if req.ID != "" {
			_ = s.auth.RevokeClient(req.ID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"removed": removed,
		"target":  target,
	})
}

func (s *Server) handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var node entity.Node
	if err := decodeRequest(w, r, &node); err != nil {
		http.Error(w, "invalid json request", http.StatusBadRequest)
		return
	}

	if node.ID == "" && node.Name == "" {
		http.Error(w, "node id or name is required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	ge := s.gossip
	s.mu.RUnlock()

	if ge != nil {
		ge.AddOrUpdateMember(&node)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"node":    node.Name,
	})
}

func (s *Server) withSyncAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.RLock()
		shared := s.gossipToken
		s.mu.RUnlock()
		sharedValid := shared != "" && subtle.ConstantTimeCompare([]byte(token), []byte(shared)) == 1
		pairedValid := s.auth != nil && s.auth.ValidateTokenFromAddr(token, r.RemoteAddr)
		if !ok || token == "" || (!sharedValid && !pairedValid) {
			http.Error(w, "unauthorized: sync requires a paired or shared gossip token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func validateService(svc *entity.Service) error {
	if err := entity.ValidateServiceName(svc.Name); err != nil {
		return err
	}
	switch svc.Type {
	case entity.TypeDocker, entity.TypeCompose, entity.TypeBun, entity.TypeNode, entity.TypeProcess:
	default:
		return fmt.Errorf("invalid service type")
	}
	if svc.Replicas < 0 || svc.Replicas > 1000 {
		return fmt.Errorf("replicas must be between 0 and 1000")
	}
	for _, port := range svc.Ports {
		if err := entity.ValidatePortMapping(port); err != nil {
			return err
		}
	}
	return nil
}

// Status is an observation API, not a configuration or credential export.
func PublicService(svc *entity.Service) *entity.Service {
	if svc == nil {
		return nil
	}
	return &entity.Service{
		Name: svc.Name, Type: svc.Type, Image: svc.Image, Ports: append([]string(nil), svc.Ports...),
		Replicas: svc.Replicas, State: svc.State, UpdatedAt: svc.UpdatedAt,
	}
}

// PublicNode removes client credentials from an observed node without mutating it.
func PublicNode(node *entity.Node) *entity.Node {
	if node == nil {
		return nil
	}
	copy := *node
	copy.AuthToken = ""
	return &copy
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

func decodeManifest(manifest string, dst *entity.Service) error {
	decoder := json.NewDecoder(strings.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("manifest must contain one JSON value")
	}
	return nil
}
