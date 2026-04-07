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

func checkNetwork(ctx context.Context) bool {
	// Try to resolve a well-known external host
	// We don't make HTTP requests — just DNS + TCP to minimize overhead
	resolver := &net.Resolver{}
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	_, err := resolver.LookupHost(resolveCtx, "dns.google")
	if err == nil {
		return true // DNS resolved successfully
	}
	// Fallback: try TCP connect to a public DNS
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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
