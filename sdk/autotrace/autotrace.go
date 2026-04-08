// Package autotrace automatically initializes the probe when imported.
//
// Usage in target project (ONE line):
//
//	import _ "github.com/hsdnh/Aegis/sdk/autotrace"
//
// This starts the probe with default settings:
//   - Agent address: 127.0.0.1:19876 (env: AIOPS_AGENT_ADDR)
//   - Scheduled window: every 5 minutes, 10 seconds each
//   - Control listener: :19877 (agent can trigger on-demand windows)
//
// The probe adds ~1ns overhead per instrumented function when dormant.
// During a 10-second window (every 5 minutes), overhead is 50-100ns per call.
// Average overhead across all time: ~0.03%.
package autotrace

import (
	"os"
	"strconv"
	"time"

	"github.com/hsdnh/Aegis/sdk/probe"
)

func init() {
	agentAddr := envOr("AIOPS_AGENT_ADDR", "127.0.0.1:19876")
	controlAddr := envOr("AIOPS_CONTROL_ADDR", ":19877")
	intervalMin := envIntOr("AIOPS_TRACE_INTERVAL_MIN", 5)
	windowSec := envIntOr("AIOPS_TRACE_WINDOW_SEC", 10)

	// Initialize the global probe
	probe.Global.Init(agentAddr)

	// Start periodic scheduled windows
	probe.Global.StartScheduler(
		time.Duration(intervalMin)*time.Minute,
		time.Duration(windowSec)*time.Second,
	)

	// Listen for remote commands from Agent (anomaly-triggered windows)
	probe.Global.ListenForCommands(controlAddr)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
