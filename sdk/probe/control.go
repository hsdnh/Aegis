package probe

import (
	"encoding/json"
	"net"
	"sync/atomic"
	"time"
)

// ControlCommand is sent from Agent to SDK to trigger trace windows.
type ControlCommand struct {
	Action   string        `json:"action"`   // "start_window", "start_deep", "stop"
	Duration time.Duration `json:"duration"` // window duration
	Trigger  string        `json:"trigger"`  // "anomaly", "manual", "deploy"
	FocusIDs []uint32      `json:"focus_ids,omitempty"` // only trace these funcIDs (selective activation)
}

// ListenForCommands starts a UDP listener for remote control from Agent.
// The Agent can trigger trace windows when it detects anomalies.
//
// Example flow:
//   Agent detects queue buildup → sends {action: "start_window", duration: 30s, trigger: "anomaly"}
//   → SDK activates tracing for 30s → captures function-level data → sends back to Agent
func (p *Probe) ListenForCommands(listenAddr string) {
	if listenAddr == "" {
		listenAddr = ":19877"
	}

	go func() {
		defer func() { recover() }()

		conn, err := net.ListenPacket("udp", listenAddr)
		if err != nil {
			return // can't listen — silently give up
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		for {
			select {
			case <-p.stopCh:
				return
			default:
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				continue
			}

			var cmd ControlCommand
			if err := json.Unmarshal(buf[:n], &cmd); err != nil {
				continue
			}

			p.handleCommand(cmd)
		}
	}()
}

func (p *Probe) handleCommand(cmd ControlCommand) {
	defer func() { recover() }()

	switch cmd.Action {
	case "start_window":
		dur := cmd.Duration
		if dur == 0 {
			dur = 10 * time.Second
		}
		if dur > 60*time.Second {
			dur = 60 * time.Second // safety cap
		}
		p.StartWindow(dur, cmd.Trigger)

	case "start_deep":
		dur := cmd.Duration
		if dur == 0 {
			dur = 10 * time.Second
		}
		if dur > 30*time.Second {
			dur = 30 * time.Second // deep mode capped at 30s
		}
		p.StartDeepWindow(dur)

	case "stop":
		atomic.StoreUint32(&p.active, 0)
	}
}
