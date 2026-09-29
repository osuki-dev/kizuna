package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestCollector_Collect(t *testing.T) {
	c := NewCollector()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m, err := c.Collect(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if m.OS == "" {
		t.Errorf("expected OS to be set")
	}
	if m.CPUCores <= 0 {
		t.Errorf("expected CPUCores > 0, got %d", m.CPUCores)
	}
	if m.TotalMemory == 0 {
		t.Errorf("expected TotalMemory > 0")
	}
}
