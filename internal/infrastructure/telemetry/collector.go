package telemetry

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// PartitionInfo represents an individual storage filesystem/disk
type PartitionInfo struct {
	Device      string  `json:"device"`
	Mountpoint  string  `json:"mountpoint"`
	Fstype      string  `json:"fstype"`
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Free        uint64  `json:"free"`
	UsedPercent float64 `json:"used_percent"`
}

// ProcessInfo represents a top resource-consuming process
type ProcessInfo struct {
	PID           int32   `json:"pid"`
	Name          string  `json:"name"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float32 `json:"memory_percent"`
	MemoryBytes   uint64  `json:"memory_bytes"`
}

// Metrics holds comprehensive real-time system resource manager metrics
type Metrics struct {
	Hostname        string          `json:"hostname"`
	OS              string          `json:"os"`
	Platform        string          `json:"platform"`
	PlatformVer     string          `json:"platform_version"`
	KernelVer       string          `json:"kernel_version"`
	Arch            string          `json:"arch"`
	CPUCores        int             `json:"cpu_cores"`
	CPUModel        string          `json:"cpu_model,omitempty"`
	CPUUsage        float64         `json:"cpu_usage"`
	CoreUsages      []float64       `json:"core_usages,omitempty"`
	TotalMemory     uint64          `json:"total_memory"`
	UsedMemory      uint64          `json:"used_memory"`
	FreeMemory      uint64          `json:"free_memory"`
	AvailableMemory uint64          `json:"available_memory"`
	CachedMemory    uint64          `json:"cached_memory"`
	MemoryUsage     float64         `json:"memory_usage"`
	TotalSwap       uint64          `json:"total_swap"`
	UsedSwap        uint64          `json:"used_swap"`
	FreeSwap        uint64          `json:"free_swap"`
	SwapUsage       float64         `json:"swap_usage"`
	TotalDisk       uint64          `json:"total_disk"`
	UsedDisk        uint64          `json:"used_disk"`
	FreeDisk        uint64          `json:"free_disk"`
	DiskUsage       float64         `json:"disk_usage"`
	Partitions      []PartitionInfo `json:"partitions,omitempty"`
	DiskReadBytes   uint64          `json:"disk_read_bytes"`
	DiskWriteBytes  uint64          `json:"disk_write_bytes"`
	Uptime          uint64          `json:"uptime"`
	Load1           float64         `json:"load1"`
	Load5           float64         `json:"load5"`
	Load15          float64         `json:"load15"`
	BytesRecv       uint64          `json:"bytes_recv"`
	BytesSent       uint64          `json:"bytes_sent"`
	RxRate          uint64          `json:"rx_rate"` // bytes/sec
	TxRate          uint64          `json:"tx_rate"` // bytes/sec
	TopProcesses    []ProcessInfo   `json:"top_processes,omitempty"`
	CollectedAt     time.Time       `json:"collected_at"`
}

// Collector provides real-time OS telemetry
type Collector struct {
	mu            sync.Mutex
	lastTime      time.Time
	lastBytesRecv uint64
	lastBytesSent uint64
}

// NewCollector creates a system metrics collector
func NewCollector() *Collector {
	return &Collector{}
}

// Collect returns a complete, lightweight snapshot of current system metrics
func (c *Collector) Collect(ctx context.Context) (*Metrics, error) {
	now := time.Now()

	m := &Metrics{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		CPUCores:    runtime.NumCPU(),
		CollectedAt: now,
	}

	// Hostname fallback
	if hn, err := os.Hostname(); err == nil {
		m.Hostname = hn
	}

	// Host Info
	if hInfo, err := host.InfoWithContext(ctx); err == nil && hInfo != nil {
		if hInfo.Hostname != "" {
			m.Hostname = hInfo.Hostname
		}
		m.Platform = hInfo.Platform
		m.PlatformVer = hInfo.PlatformVersion
		m.KernelVer = hInfo.KernelVersion
		m.Uptime = hInfo.Uptime
	}

	// CPU Info & Overall
	if cpuInfos, err := cpu.InfoWithContext(ctx); err == nil && len(cpuInfos) > 0 {
		m.CPUModel = cpuInfos[0].ModelName
	}
	if percents, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(percents) > 0 {
		m.CPUUsage = percents[0]
	}
	// Per-core usages
	if corePercents, err := cpu.PercentWithContext(ctx, 0, true); err == nil && len(corePercents) > 0 {
		m.CoreUsages = corePercents
	}

	// Virtual Memory & Swap
	if vMem, err := mem.VirtualMemoryWithContext(ctx); err == nil && vMem != nil {
		m.TotalMemory = vMem.Total
		m.UsedMemory = vMem.Used
		m.FreeMemory = vMem.Free
		m.AvailableMemory = vMem.Available
		m.CachedMemory = vMem.Cached + vMem.Buffers
		m.MemoryUsage = vMem.UsedPercent
	}
	if sMem, err := mem.SwapMemoryWithContext(ctx); err == nil && sMem != nil {
		m.TotalSwap = sMem.Total
		m.UsedSwap = sMem.Used
		m.FreeSwap = sMem.Free
		m.SwapUsage = sMem.UsedPercent
	}

	// Partitions and Disks
	c.collectDisks(ctx, m)

	// Load Average (Unix/Linux/macOS)
	if lAvg, err := load.AvgWithContext(ctx); err == nil && lAvg != nil {
		m.Load1 = lAvg.Load1
		m.Load5 = lAvg.Load5
		m.Load15 = lAvg.Load15
	}

	// Network I/O and Throughput Rate
	c.collectNetwork(ctx, m, now)

	// Top Processes (limited to 5 for high performance)
	c.collectProcesses(ctx, m)

	return m, nil
}

func (c *Collector) collectDisks(ctx context.Context, m *Metrics) {
	partitions, err := disk.PartitionsWithContext(ctx, false)
	if err == nil && len(partitions) > 0 {
		seenMounts := make(map[string]bool)
		for _, p := range partitions {
			// Skip virtual / pseudo filesystems
			if isVirtualFS(p.Fstype, p.Mountpoint) {
				continue
			}
			if seenMounts[p.Mountpoint] {
				continue
			}
			seenMounts[p.Mountpoint] = true

			u, err := disk.UsageWithContext(ctx, p.Mountpoint)
			if err != nil || u == nil || u.Total == 0 {
				continue
			}

			m.Partitions = append(m.Partitions, PartitionInfo{
				Device:      p.Device,
				Mountpoint:  p.Mountpoint,
				Fstype:      p.Fstype,
				Total:       u.Total,
				Used:        u.Used,
				Free:        u.Free,
				UsedPercent: u.UsedPercent,
			})
		}
	}

	// Overall root or aggregate disk
	if len(m.Partitions) > 0 {
		rootPart := m.Partitions[0]
		for _, p := range m.Partitions {
			if p.Mountpoint == "/" || p.Mountpoint == "C:\\" {
				rootPart = p
				break
			}
		}
		m.TotalDisk = rootPart.Total
		m.UsedDisk = rootPart.Used
		m.FreeDisk = rootPart.Free
		m.DiskUsage = rootPart.UsedPercent
	} else {
		diskRoot := "/"
		if runtime.GOOS == "windows" {
			if sysDrive := os.Getenv("SystemDrive"); sysDrive != "" {
				diskRoot = sysDrive + "\\"
			} else {
				diskRoot = "C:\\"
			}
		}
		if dUsage, err := disk.UsageWithContext(ctx, diskRoot); err == nil && dUsage != nil {
			m.TotalDisk = dUsage.Total
			m.UsedDisk = dUsage.Used
			m.FreeDisk = dUsage.Free
			m.DiskUsage = dUsage.UsedPercent
		}
	}

	// Disk IO
	if ioCounters, err := disk.IOCountersWithContext(ctx); err == nil {
		for _, io := range ioCounters {
			m.DiskReadBytes += io.ReadBytes
			m.DiskWriteBytes += io.WriteBytes
		}
	}
}

func (c *Collector) collectNetwork(ctx context.Context, m *Metrics, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ioCounters, err := net.IOCountersWithContext(ctx, false); err == nil && len(ioCounters) > 0 {
		m.BytesRecv = ioCounters[0].BytesRecv
		m.BytesSent = ioCounters[0].BytesSent

		if !c.lastTime.IsZero() && now.After(c.lastTime) {
			elapsed := now.Sub(c.lastTime).Seconds()
			if elapsed > 0 {
				if m.BytesRecv >= c.lastBytesRecv {
					m.RxRate = uint64(float64(m.BytesRecv-c.lastBytesRecv) / elapsed)
				}
				if m.BytesSent >= c.lastBytesSent {
					m.TxRate = uint64(float64(m.BytesSent-c.lastBytesSent) / elapsed)
				}
			}
		}
		c.lastTime = now
		c.lastBytesRecv = m.BytesRecv
		c.lastBytesSent = m.BytesSent
	}
}

func (c *Collector) collectProcesses(ctx context.Context, m *Metrics) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil || len(procs) == 0 {
		return
	}

	var sampled []ProcessInfo
	for _, p := range procs {
		memInfo, err := p.MemoryInfoWithContext(ctx)
		if err != nil || memInfo == nil || memInfo.RSS < 30*1024*1024 {
			continue
		}
		name, _ := p.NameWithContext(ctx)
		if name == "" {
			continue
		}
		cpuP, _ := p.CPUPercentWithContext(ctx)
		memP, _ := p.MemoryPercentWithContext(ctx)

		sampled = append(sampled, ProcessInfo{
			PID:           p.Pid,
			Name:          name,
			CPUPercent:    cpuP,
			MemoryPercent: memP,
			MemoryBytes:   memInfo.RSS,
		})
	}

	sort.Slice(sampled, func(i, j int) bool {
		return sampled[i].MemoryBytes > sampled[j].MemoryBytes
	})

	if len(sampled) > 5 {
		sampled = sampled[:5]
	}
	m.TopProcesses = sampled
}

func isVirtualFS(fstype, mountpoint string) bool {
	switch fstype {
	case "tmpfs", "devtmpfs", "squashfs", "overlay", "proc", "sysfs", "cgroup", "cgroup2",
		"debugfs", "tracefs", "securityfs", "pstore", "bpf", "configfs", "fusectl", "mqueue":
		return true
	}
	if strings.HasPrefix(mountpoint, "/snap") ||
		strings.HasPrefix(mountpoint, "/var/lib/docker") ||
		strings.HasPrefix(mountpoint, "/var/lib/containerd") {
		return true
	}
	return false
}
