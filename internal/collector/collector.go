package collector

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/hsdnh/Aegis/pkg/types"
)

// Collector is the plugin interface for data collection.
// Each collector gathers metrics and logs from one source.
type Collector interface {
	// Name returns the unique identifier for this collector.
	Name() string
	// Collect gathers metrics and logs. Called every cycle.
	Collect(ctx context.Context) (*types.CollectResult, error)
}

// Registry holds all registered collectors.
type Registry struct {
	collectors []Collector
	timeout    time.Duration // per-collector timeout
}

func NewRegistry() *Registry {
	return &Registry{
		timeout: 30 * time.Second, // default per-collector timeout
	}
}

func (r *Registry) SetTimeout(d time.Duration) {
	r.timeout = d
}

func (r *Registry) Register(c Collector) {
	r.collectors = append(r.collectors, c)
}

func (r *Registry) All() []Collector {
	return r.collectors
}

// CollectAll runs all collectors in parallel with per-collector timeouts.
// Returns results and a health summary.
func (r *Registry) CollectAll(ctx context.Context) ([]types.CollectResult, types.SnapshotHealth) {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []types.CollectResult
		health  = types.SnapshotHealth{
			TotalCollectors: len(r.collectors),
		}
	)

	for _, c := range r.collectors {
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()

			// Per-collector timeout to prevent one slow collector blocking everything
			collectorCtx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()

			start := time.Now()
			result, err := c.Collect(collectorCtx)
			elapsed := time.Since(start)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				log.Printf("  [%s] FAILED in %v: %v", c.Name(), elapsed, err)
				results = append(results, types.CollectResult{
					CollectorName: c.Name(),
					Errors:        []string{err.Error()},
					CollectedAt:   time.Now(),
				})
				health.FailedCollectors++
			} else {
				result.CollectedAt = time.Now()
				results = append(results, *result)
				health.SuccessCollectors++
			}
		}(c)
	}

	wg.Wait()

	// Calculate health scores
	if health.TotalCollectors > 0 {
		health.Completeness = float64(health.SuccessCollectors) / float64(health.TotalCollectors)
	}
	health.Freshness = 1.0 // all data from this cycle is fresh
	health.Trustworthy = health.Completeness >= 0.5 // at least half must succeed

	return results, health
}
