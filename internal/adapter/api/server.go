package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"github.com/osuki-dev/kizuna/internal/infrastructure/telemetry"
)

// Server handles incoming RPC requests to the Agent over mesh or local TCP
type Server struct {
	auth     domain.AuthManager
	workload domain.WorkloadRunner
	ingress  domain.IngressManager
	backup   domain.BackupManager
	nodeID   string
	nodeName string
	mu       sync.RWMutex
	metaPath string
	meta     entity.NodeMetaUpdate
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
		auth:     auth,
		workload: workload,
		ingress:  ingress,
		backup:   backup,
		nodeID:   nodeID,
		nodeName: nodeName,
		metaPath: metaPath,
	}
	s.loadMeta()
	return s
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

// Handler returns the HTTP handler with all registered routes and auth middleware
func (s *Server) Handler() http.Handler {
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

	return mux
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
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || !s.auth.ValidateToken(token) {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Deploy request parses multipart: json "manifest" + optional binary "artifact"
	err := r.ParseMultipartForm(500 << 20) // 500MB max
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to parse multipart: %v", err), http.StatusBadRequest)
		return
	}

	manifestJSON := r.FormValue("manifest")
	var svc entity.Service
	if err := json.Unmarshal([]byte(manifestJSON), &svc); err != nil {
		http.Error(w, fmt.Sprintf("invalid service manifest: %v", err), http.StatusBadRequest)
		return
	}

	var artifactReader io.Reader
	file, _, err := r.FormFile("artifact")
	if err == nil {
		defer func() { _ = file.Close() }()
		artifactReader = file
	}

	if err := s.workload.Deploy(r.Context(), &svc, artifactReader); err != nil {
		http.Error(w, fmt.Sprintf("deployment failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "deployed", "service": svc.Name})
}

func (s *Server) handleIngress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cfg entity.IngressConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
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
	services, _ := s.workload.ListServices(r.Context())

	col := telemetry.NewCollector()
	metrics, _ := col.Collect(r.Context())

	node := entity.Node{
		ID:          s.nodeID,
		Name:        s.nodeName,
		IsOnline:    true,
		LastSeen:    time.Now(),
		OS:          "linux",
		Arch:        "amd64",
	}

	if metrics != nil {
		node.OS = metrics.OS
		node.Arch = metrics.Arch
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
	s.mu.RUnlock()

	resp := map[string]any{
		"node":     node,
		"services": services,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	svcName := r.URL.Query().Get("service")
	if svcName == "" {
		http.Error(w, "missing service query param", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	_ = s.workload.StreamLogs(r.Context(), svcName, w)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string               `json:"service"`
		Config  *entity.BackupConfig `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid backup payload", http.StatusBadRequest)
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
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
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
		_ = s.saveMetaLocked()
		currentMeta := s.meta
		s.mu.Unlock()

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

// singleConnListener allows serving an http.Server over a single net.Conn
type singleConnListener struct {
	conn      net.Conn
	done      bool
	closeChan chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{
		conn:      conn,
		closeChan: make(chan struct{}),
	}
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
	select {
	case <-l.closeChan:
	default:
		close(l.closeChan)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}
