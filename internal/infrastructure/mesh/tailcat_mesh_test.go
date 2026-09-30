package mesh

import (
	"strings"
	"testing"

	"github.com/osuki-dev/kizuna/internal/domain/entity"
)

func boolPtr(b bool) *bool {
	return &b
}

func TestCheckSSHPermission_NilPolicy(t *testing.T) {
	caller := &entity.Node{
		ID:   "node-1",
		Name: "worker-1",
		Tags: []string{"web", "prod"},
	}
	err := CheckSSHPermission(nil, caller, "nodekey:1234567890abcdef")
	if err != nil {
		t.Fatalf("expected nil policy to allow SSH, got error: %v", err)
	}
}

func TestCheckSSHPermission_Disabled(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled: boolPtr(false),
	}
	caller := &entity.Node{
		ID:   "node-1",
		Name: "worker-1",
		Tags: []string{"admin"},
	}
	err := CheckSSHPermission(policy, caller, "nodekey:1234567890abcdef")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error, got: %v", err)
	}
}

func TestCheckSSHPermission_NoRules(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled: boolPtr(true),
	}
	caller := &entity.Node{
		ID:   "node-1",
		Name: "worker-1",
	}
	err := CheckSSHPermission(policy, caller, "nodekey:1234567890abcdef")
	if err != nil {
		t.Fatalf("expected empty rules to allow SSH, got: %v", err)
	}
}

func TestCheckSSHPermission_AllowTags(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled:   boolPtr(true),
		AllowTags: []string{"admin", "ops"},
	}

	// Case 1: Matching tag (admin)
	caller1 := &entity.Node{
		ID:   "node-1",
		Name: "laptop",
		Tags: []string{"laptop", "admin"},
	}
	if err := CheckSSHPermission(policy, caller1, "nodekey:1111"); err != nil {
		t.Errorf("expected caller1 with 'admin' tag to be allowed, got: %v", err)
	}

	// Case 2: Matching tag case-insensitive (OPS)
	caller2 := &entity.Node{
		ID:   "node-2",
		Name: "bastion",
		Tags: []string{"OPS"},
	}
	if err := CheckSSHPermission(policy, caller2, "nodekey:2222"); err != nil {
		t.Errorf("expected caller2 with 'OPS' tag to be allowed, got: %v", err)
	}

	// Case 3: Non-matching tag (guest)
	caller3 := &entity.Node{
		ID:   "node-3",
		Name: "guest-pc",
		Tags: []string{"guest", "worker"},
	}
	if err := CheckSSHPermission(policy, caller3, "nodekey:3333"); err == nil {
		t.Errorf("expected caller3 with tags [guest worker] to be rejected")
	}

	// Case 4: No tags on caller
	caller4 := &entity.Node{
		ID:   "node-4",
		Name: "empty-tags",
	}
	if err := CheckSSHPermission(policy, caller4, "nodekey:4444"); err == nil {
		t.Errorf("expected caller4 with no tags to be rejected")
	}
}

func TestCheckSSHPermission_DenyTags(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled:   boolPtr(true),
		AllowTags: []string{"admin"},
		DenyTags:  []string{"untrusted", "guest"},
	}

	// Case 1: Matching allow and deny -> Deny must take precedence!
	caller1 := &entity.Node{
		ID:   "node-1",
		Name: "compromised-admin",
		Tags: []string{"admin", "untrusted"},
	}
	if err := CheckSSHPermission(policy, caller1, "nodekey:1111"); err == nil {
		t.Errorf("expected caller with deny tag 'untrusted' to be rejected despite having 'admin'")
	}

	// Case 2: Clean admin node
	caller2 := &entity.Node{
		ID:   "node-2",
		Name: "clean-admin",
		Tags: []string{"admin"},
	}
	if err := CheckSSHPermission(policy, caller2, "nodekey:2222"); err != nil {
		t.Errorf("expected clean admin to be allowed, got: %v", err)
	}
}

func TestCheckSSHPermission_AllowAndDenyNodes(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled:    boolPtr(true),
		AllowNodes: []string{"special-node", "node_special_id"},
		DenyNodes:  []string{"banned-node"},
	}

	// Case 1: Matching allowed name
	caller1 := &entity.Node{
		ID:   "id-1",
		Name: "special-node",
	}
	if err := CheckSSHPermission(policy, caller1, "nodekey:1111"); err != nil {
		t.Errorf("expected special-node to be allowed, got: %v", err)
	}

	// Case 2: Matching allowed ID
	caller2 := &entity.Node{
		ID:   "node_special_id",
		Name: "other-name",
	}
	if err := CheckSSHPermission(policy, caller2, "nodekey:2222"); err != nil {
		t.Errorf("expected caller with allowed ID to be allowed, got: %v", err)
	}

	// Case 3: Explicitly denied node
	caller3 := &entity.Node{
		ID:   "id-3",
		Name: "banned-node",
	}
	if err := CheckSSHPermission(policy, caller3, "nodekey:3333"); err == nil {
		t.Errorf("expected banned-node to be rejected")
	}

	// Case 4: Node not in allow list
	caller4 := &entity.Node{
		ID:   "id-4",
		Name: "random-node",
	}
	if err := CheckSSHPermission(policy, caller4, "nodekey:4444"); err == nil {
		t.Errorf("expected random-node not in allow list to be rejected")
	}
}

func TestCheckSSHPermission_UnrecognizedCaller(t *testing.T) {
	policy := &entity.SSHConfig{
		Enabled:   boolPtr(true),
		AllowTags: []string{"admin"},
	}

	err := CheckSSHPermission(policy, nil, "nodekey:unknown123456789")
	if err == nil || !strings.Contains(err.Error(), "not recognized") {
		t.Fatalf("expected unrecognized error for nil caller with ACL active, got: %v", err)
	}
}

func TestExtractNodePublicKey(t *testing.T) {
	// 1. Invalid address
	if pub := ExtractNodePublicKey("127.0.0.1:22"); pub != "" {
		t.Errorf("expected empty string for non-tailcat address, got: %s", pub)
	}

	// 2. Real persistent key generated address
	nodeKey, psk := loadOrCreateMeshKeys()
	meshGw := &TailcatMesh{}
	meshGw.nodeKey = nodeKey
	meshGw.psk = psk
	meshGw.keysLoaded = true

	// Verify that loadOrCreateMeshKeys works
	expectedPub := nodeKey.Public().String()
	if !strings.HasPrefix(expectedPub, "nodekey:") {
		t.Errorf("expected nodekey prefix, got: %s", expectedPub)
	}
}
