package health

import (
	"context"
	"net"
	"os"
	"time"


	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

var startedAt = time.Now()

// CheckSelf verifies the agent's own operational health.
// Must pass before sending CRITICAL/FATAL alerts to avoid false positives
// from network partitions or agent-side failures.
func CheckSelf(ctx context.Context) types.AgentHealth {
	h := types.AgentHealth{
		StartedAt:     startedAt,
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
	}

	// Network connectivity — can we reach an external gateway?
	// This prevents false "everything is down" alerts during network partitions.
	h.NetworkOK = checkNetwork(ctx)

	// Disk space — can we still write logs/data?
	h.DiskOK = checkDisk()

	// Last cycle assumed OK (caller tracks this)
	h.LastCycleOK = true

	return h
}

// ShouldSuppressCritical returns true if agent health issues suggest
// that critical alerts might be false positives.
func ShouldSuppressCritical(h types.AgentHealth) bool {
	// If our own network is down, all "service unreachable" alerts are suspect
	return !h.NetworkOK
}

// HealthCheckTarget can be set via env AIOPS_HEALTH_TARGET to override default.
// Default tries multiple targets for China/global/intranet compatibility.
var HealthCheckTarget = os.Getenv("AIOPS_HEALTH_TARGET")

func checkNetwork(ctx context.Context) bool {
	// If user specified a custom target, only check that
	if HealthCheckTarget != "" {
		conn, err := net.DialTimeout("tcp", HealthCheckTarget, 3*time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}

	// Try multiple targets for global compatibility:
	// China: 114.114.114.114 (DNS), 223.5.5.5 (Alibaba DNS)
	// Global: 8.8.8.8 (Google), 1.1.1.1 (Cloudflare)
	// Any one succeeding = network is OK
	targets := []string{
		"114.114.114.114:53", // China DNS
		"223.5.5.5:53",       // Alibaba DNS
		"8.8.8.8:53",         // Google DNS
		"1.1.1.1:53",         // Cloudflare DNS
	}

	for _, target := range targets {
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

func checkDisk() bool {
	// Simple check: can we create a temp file?
	f, err := os.CreateTemp("", "aiops-health-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}
