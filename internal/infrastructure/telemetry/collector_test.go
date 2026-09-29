package telemetry

import "testing"

func TestIsVirtualFS(t *testing.T) {
	tests := []struct {
		fstype     string
		mountpoint string
		expected   bool
	}{
		{"ext4", "/", false},
		{"btrfs", "/home", false},
		{"apfs", "/System/Volumes/Data", false},
		{"zfs", "/pool", false},
		{"tmpfs", "/run", true},
		{"proc", "/proc", true},
		{"sysfs", "/sys", true},
		{"cgroup2", "/sys/fs/cgroup", true},
		{"overlay", "/var/lib/docker/overlay2/abc", true},
		{"ext4", "/snap/core/1234", true},
		{"ext4", "/var/lib/docker/volumes", true},
		{"ext4", "/var/lib/containerd/io", true},
	}

	for _, tt := range tests {
		got := isVirtualFS(tt.fstype, tt.mountpoint)
		if got != tt.expected {
			t.Errorf("isVirtualFS(%q, %q) = %v, expected %v", tt.fstype, tt.mountpoint, got, tt.expected)
		}
	}
}
