package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/osuki-dev/kizuna/internal/domain"
	"github.com/osuki-dev/kizuna/internal/domain/entity"
	"gopkg.in/yaml.v3"
)

// FileNodeRepository implements domain.NodeRepository using ~/.kizuna/nodes.yaml
type FileNodeRepository struct {
	mu       sync.RWMutex
	filePath string
	nodes    map[string]*entity.Node
}

// NewNodeRepository initializes node repository
func NewNodeRepository(baseDir string) (domain.NodeRepository, error) {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".kizuna")
	}
	_ = os.MkdirAll(baseDir, 0700)

	repo := &FileNodeRepository{
		filePath: filepath.Join(baseDir, "nodes.yaml"),
		nodes:    make(map[string]*entity.Node),
	}

	if err := repo.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return repo, nil
}

func (r *FileNodeRepository) GetNode(name string) (*entity.Node, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if node, ok := r.nodes[name]; ok {
		return node, nil
	}
	for _, n := range r.nodes {
		if n.ID == name || strings.EqualFold(n.Name, name) || n.Host == name {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node '%s' not found", name)
}

func (r *FileNodeRepository) SaveNode(node *entity.Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nodes[node.Name] = node
	return r.saveLocked()
}

func (r *FileNodeRepository) ListNodes() ([]*entity.Node, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var list []*entity.Node
	for _, n := range r.nodes {
		list = append(list, n)
	}
	return list, nil
}

func (r *FileNodeRepository) DeleteNode(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.nodes, name)
	for k, n := range r.nodes {
		if n.ID == name || strings.EqualFold(n.Name, name) {
			delete(r.nodes, k)
		}
	}
	return r.saveLocked()
}

func (r *FileNodeRepository) load() error {
	data, err := os.ReadFile(r.filePath)
	if err != nil {
		return err
	}
	var list []*entity.Node
	if err := yaml.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, n := range list {
		r.nodes[n.Name] = n
	}
	return nil
}

func (r *FileNodeRepository) saveLocked() error {
	var list []*entity.Node
	for _, n := range r.nodes {
		list = append(list, n)
	}
	data, err := yaml.Marshal(list)
	if err != nil {
		return err
	}
	return os.WriteFile(r.filePath, data, 0600)
}
