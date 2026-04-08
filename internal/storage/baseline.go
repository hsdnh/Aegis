package storage

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hsdnh/Aegis/internal/ai"
	"github.com/hsdnh/Aegis/pkg/types"
)

func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

// BaselineLearner accumulates metric samples and computes statistical baselines.
// During the learning period (first 24-48h), it collects data without alerting.
// After enough samples, it computes mean/stddev for each metric.
type BaselineLearner struct {
	db           *DB
	minSamples   int           // minimum samples before baseline is valid (default 48 = 24h at 30min)
	learningDone bool
}

func NewBaselineLearner(db *DB, minSamples int) *BaselineLearner {
	if minSamples == 0 {
		minSamples = 48 // 24 hours at 30-min intervals
	}
	return &BaselineLearner{db: db, minSamples: minSamples}
}

// Ingest processes a batch of metrics from one monitoring cycle.
// Saves to DB and updates running baselines.
func (bl *BaselineLearner) Ingest(metrics []types.Metric) error {
	if err := bl.db.SaveMetrics(metrics); err != nil {
		return err
	}

	// Recompute baselines for each metric (keyed by name+labels)
	seen := make(map[string]bool)
	for _, m := range metrics {
		key := metricKey(m.Name, m.Labels)
		if seen[key] {
			continue
		}
		seen[key] = true

		series, err := bl.db.QueryMetricSeries(key, time.Now().Add(-48*time.Hour))
		if err != nil || len(series) < 3 {
			continue
		}

		// Compute statistics
		var sum, sumSq, minV, maxV float64
		minV = math.MaxFloat64
		maxV = -math.MaxFloat64
		for _, pt := range series {
			sum += pt.Value
			sumSq += pt.Value * pt.Value
			if pt.Value < minV {
				minV = pt.Value
			}
			if pt.Value > maxV {
				maxV = pt.Value
			}
		}
		n := float64(len(series))
		mean := sum / n
		variance := (sumSq / n) - (mean * mean)
		if variance < 0 {
			variance = 0
		}
		stdDev := math.Sqrt(variance)

		bl.db.SaveBaseline(key, Baseline{
			Mean:        mean,
			StdDev:      stdDev,
			Min:         minV,
			Max:         maxV,
			SampleCount: len(series),
			UpdatedAt:   time.Now(),
		})
	}

	return nil
}

// IsReady returns true if we have enough samples for meaningful baselines.
func (bl *BaselineLearner) IsReady() bool {
	if bl.learningDone {
		return true
	}
	baselines, err := bl.db.LoadBaselines()
	if err != nil {
		return false
	}
	// Consider ready if at least 3 metrics have enough samples
	readyCount := 0
	for _, b := range baselines {
		if b.SampleCount >= bl.minSamples {
			readyCount++
		}
	}
	if readyCount >= 3 {
		bl.learningDone = true
		return true
	}
	return false
}

// GetAIBaseline converts stored baselines to the format the AI analyst expects.
func (bl *BaselineLearner) GetAIBaseline() *ai.BaselineData {
	baselines, err := bl.db.LoadBaselines()
	if err != nil || len(baselines) == 0 {
		return nil
	}

	result := &ai.BaselineData{
		MetricBaselines: make(map[string]ai.MetricBaseline),
		CollectedAt:     time.Now(),
	}
	for name, b := range baselines {
		result.MetricBaselines[name] = ai.MetricBaseline{
			Mean:   b.Mean,
			StdDev: b.StdDev,
			Min:    b.Min,
			Max:    b.Max,
		}
	}
	return result
}
