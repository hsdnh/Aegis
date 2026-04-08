package rule

import (
	"github.com/hsdnh/Aegis/pkg/types"
)

// DependencyMap defines which components depend on which.
// If a root component is FATAL, downstream alerts are suppressed.
//
// Example: MySQL down → suppress order_pending, failed_orders, API checks
type DependencyMap struct {
	// key: root metric name, value: list of dependent metric names
	deps map[string][]string
}

func NewDependencyMap() *DependencyMap {
	return &DependencyMap{
		deps: make(map[string][]string),
	}
}

// AddDependency registers that downstream metrics depend on root.
// When root triggers FATAL, downstream alerts are suppressed.
func (d *DependencyMap) AddDependency(rootMetric string, dependentMetrics []string) {
	d.deps[rootMetric] = append(d.deps[rootMetric], dependentMetrics...)
}

// Suppress removes downstream alerts when their root cause is already triggered.
// Returns filtered results + list of suppressed rules with reason.
func (d *DependencyMap) Suppress(results []types.RuleResult) (filtered []types.RuleResult, suppressed []types.RuleResult) {
	// Find which root components have triggered FATAL/CRITICAL
	failedRoots := make(map[string]string) // metric → rule name
	for _, rr := range results {
		if rr.Triggered && (rr.Severity == types.SeverityFatal || rr.Severity == types.SeverityCritical) {
			if _, isRoot := d.deps[rr.MetricName]; isRoot {
				failedRoots[rr.MetricName] = rr.RuleName
			}
		}
	}

	if len(failedRoots) == 0 {
		return results, nil
	}

	// Build suppression set
	suppressSet := make(map[string]string) // dependent metric → root rule name
	for rootMetric, rootRule := range failedRoots {
		for _, dep := range d.deps[rootMetric] {
			suppressSet[dep] = rootRule
		}
	}

	// Filter
	for _, rr := range results {
		if rootRule, shouldSuppress := suppressSet[rr.MetricName]; shouldSuppress && rr.Triggered {
			rr.Message = rr.Message + " [SUPPRESSED: caused by " + rootRule + "]"
			rr.Triggered = false // suppress the alert
			suppressed = append(suppressed, rr)
		} else {
			filtered = append(filtered, rr)
		}
	}

	return
}

// DefaultMercariHunterDeps returns dependency map for mercari-hunter.
func DefaultMercariHunterDeps() *DependencyMap {
	d := NewDependencyMap()

	// MySQL down → all MySQL-dependent checks are meaningless
	d.AddDependency("mysql.connection.alive", []string{
		"mysql.check.pending_orders",
		"mysql.check.failed_orders",
		"mysql.threads.connected",
		"mysql.threads.running",
		"mysql.slow_queries",
	})

	// Redis down → all Redis-dependent checks are meaningless
	d.AddDependency("redis.connection.alive", []string{
		"redis.keys.mh_queue_all_pending.total_length",
		"redis.memory.used_bytes",
		"redis.clients.connected",
	})

	// API down → all API JSON path checks are meaningless
	d.AddDependency("http.response.alive", []string{
		"http.json.api_queue_pending",
		"http.json.workers_online",
		"http.response.latency_ms",
	})

	return d
}
