package alert

import (
	"context"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// Alerter is the plugin interface for sending alerts.
type Alerter interface {
	Name() string
	Send(ctx context.Context, alert types.Alert) error
}

// Manager holds all alerters and dispatches alerts.
type Manager struct {
	alerters []Alerter
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Register(a Alerter) {
	m.alerters = append(m.alerters, a)
}

// Dispatch sends an alert to all registered alerters.
// Returns the first error encountered but continues sending to all.
func (m *Manager) Dispatch(ctx context.Context, alert types.Alert) error {
	var firstErr error
	for _, a := range m.alerters {
		if err := a.Send(ctx, alert); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DispatchAll sends multiple alerts.
func (m *Manager) DispatchAll(ctx context.Context, alerts []types.Alert) []error {
	var errs []error
	for _, alert := range alerts {
		if err := m.Dispatch(ctx, alert); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
