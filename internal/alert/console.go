package alert

import (
	"context"
	"fmt"
	"os"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// ConsoleAlerter prints alerts to stdout. Useful for testing and debugging.
type ConsoleAlerter struct{}

func NewConsoleAlerter() *ConsoleAlerter {
	return &ConsoleAlerter{}
}

func (c *ConsoleAlerter) Name() string { return "console" }

func (c *ConsoleAlerter) Send(_ context.Context, alert types.Alert) error {
	icon := "ℹ️"
	switch alert.Severity {
	case types.SeverityWarning:
		icon = "⚠️"
	case types.SeverityCritical:
		icon = "🔴"
	case types.SeverityFatal:
		icon = "💀"
	}
	fmt.Fprintf(os.Stdout, "%s [%s] %s\n   %s\n   Source: %s | Time: %s\n\n",
		icon, alert.Severity, alert.Title,
		alert.Body, alert.Source,
		alert.CreatedAt.Format("2006-01-02 15:04:05"))
	return nil
}
