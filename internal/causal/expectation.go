// Package causal — expectation model for detecting logic bugs.
// Learns "what normal looks like" from historical traces,
// then flags traces that deviate even though they "succeeded".
package causal

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hsdnh/Aegis/sdk/probe"
)

// ExpectationModel learns normal request patterns and detects logic anomalies.
// It catches bugs that monitoring and tracing miss — the "ran fine but wrong result" bugs.
type ExpectationModel struct {
	mu       sync.RWMutex
	patterns map[string]*TracePattern // entryFunc → learned pattern
	minSamples int
}

// TracePattern is the learned "normal" for a specific entry function.
type TracePattern struct {
	EntryFunc     string           `json:"entry_func"`
	SampleCount   int              `json:"sample_count"`
	AvgStepCount  float64          `json:"avg_step_count"`
	AvgDurationNs float64          `json:"avg_duration_ns"`
	StdDurationNs float64          `json:"std_duration_ns"`
	ExpectedSteps []ExpectedStep   `json:"expected_steps"`
	StepSequences map[string]int   `json:"step_sequences"` // "A→B→C" → count
	UpdatedAt     time.Time        `json:"updated_at"`
}

// ExpectedStep is what we expect to see at each position in a trace.
type ExpectedStep struct {
	Position    int     `json:"position"`
	FuncName    string  `json:"func_name"`
	Frequency   float64 `json:"frequency"`    // 0-1: how often this step appears here
	AvgDuration float64 `json:"avg_duration_ns"`
	OpType      string  `json:"op_type,omitempty"`
}

// TraceDeviation is a detected anomaly in a trace compared to the expected pattern.
type TraceDeviation struct {
	TraceID     string  `json:"trace_id"`
	Type        string  `json:"type"` // "missing_step", "unexpected_step", "wrong_order", "unusual_result", "duration_anomaly"
	Severity    string  `json:"severity"` // "info", "warning", "critical"
	Description string  `json:"description"`
	Expected    string  `json:"expected"`
	Actual      string  `json:"actual"`
	StepIdx     int     `json:"step_idx,omitempty"`
	Confidence  float64 `json:"confidence"`
}

func NewExpectationModel(minSamples int) *ExpectationModel {
	if minSamples == 0 {
		minSamples = 50
	}
	return &ExpectationModel{
		patterns:   make(map[string]*TracePattern),
		minSamples: minSamples,
	}
}

// Learn ingests a completed trace to build/update the pattern model.
func (em *ExpectationModel) Learn(trace probe.RequestTrace) {
	if trace.Status == "error" || len(trace.Steps) == 0 {
		return // only learn from successful traces
	}

	em.mu.Lock()
	defer em.mu.Unlock()

	pattern, ok := em.patterns[trace.EntryFunc]
	if !ok {
		pattern = &TracePattern{
			EntryFunc:     trace.EntryFunc,
			StepSequences: make(map[string]int),
		}
		em.patterns[trace.EntryFunc] = pattern
	}

	n := float64(pattern.SampleCount)
	pattern.SampleCount++

	// Running average of step count
	pattern.AvgStepCount = (pattern.AvgStepCount*n + float64(len(trace.Steps))) / (n + 1)

	// Running average + std of duration
	dur := float64(trace.Duration)
	oldAvg := pattern.AvgDurationNs
	pattern.AvgDurationNs = (oldAvg*n + dur) / (n + 1)
	if n > 0 {
		pattern.StdDurationNs = math.Sqrt((pattern.StdDurationNs*pattern.StdDurationNs*n + (dur-oldAvg)*(dur-pattern.AvgDurationNs)) / (n + 1))
	}

	// Learn step sequence
	seq := ""
	for i, step := range trace.Steps {
		if i > 0 {
			seq += "→"
		}
		seq += step.FuncName
	}
	pattern.StepSequences[seq]++

	// Learn expected steps at each position
	for i, step := range trace.Steps {
		if i >= len(pattern.ExpectedSteps) {
			pattern.ExpectedSteps = append(pattern.ExpectedSteps, ExpectedStep{
				Position: i,
				FuncName: step.FuncName,
				OpType:   step.OpType,
			})
		}
		es := &pattern.ExpectedSteps[i]
		es.Frequency = (es.Frequency*n + 1) / (n + 1)
		es.AvgDuration = (es.AvgDuration*n + float64(step.DurationNs)) / (n + 1)
	}

	pattern.UpdatedAt = time.Now()
}

// IsReady returns true if we have enough data to make predictions.
func (em *ExpectationModel) IsReady(entryFunc string) bool {
	em.mu.RLock()
	defer em.mu.RUnlock()
	p, ok := em.patterns[entryFunc]
	return ok && p.SampleCount >= em.minSamples
}

// Check compares a trace against the learned pattern and returns deviations.
func (em *ExpectationModel) Check(trace probe.RequestTrace) []TraceDeviation {
	em.mu.RLock()
	defer em.mu.RUnlock()

	pattern, ok := em.patterns[trace.EntryFunc]
	if !ok || pattern.SampleCount < em.minSamples {
		return nil // not enough data to judge
	}

	var deviations []TraceDeviation

	// 1. Duration anomaly (> 3 sigma)
	if pattern.StdDurationNs > 0 {
		zScore := (float64(trace.Duration) - pattern.AvgDurationNs) / pattern.StdDurationNs
		if zScore > 3 {
			deviations = append(deviations, TraceDeviation{
				TraceID:     trace.TraceID,
				Type:        "duration_anomaly",
				Severity:    severityFromZ(zScore),
				Description: fmt.Sprintf("%s took %.0fms, normal is %.0f±%.0fms (%.1fσ)",
					trace.EntryFunc, float64(trace.Duration)/1e6,
					pattern.AvgDurationNs/1e6, pattern.StdDurationNs/1e6, zScore),
				Expected:   fmt.Sprintf("%.0fms", pattern.AvgDurationNs/1e6),
				Actual:     fmt.Sprintf("%.0fms", float64(trace.Duration)/1e6),
				Confidence: math.Min(zScore/10, 1.0),
			})
		}
	}

	// 2. Step count anomaly
	stepDiff := math.Abs(float64(len(trace.Steps)) - pattern.AvgStepCount)
	if stepDiff > 3 && pattern.AvgStepCount > 0 {
		deviations = append(deviations, TraceDeviation{
			TraceID:     trace.TraceID,
			Type:        "step_count_anomaly",
			Severity:    "warning",
			Description: fmt.Sprintf("%s had %d steps, normal is %.0f",
				trace.EntryFunc, len(trace.Steps), pattern.AvgStepCount),
			Expected: fmt.Sprintf("%.0f steps", pattern.AvgStepCount),
			Actual:   fmt.Sprintf("%d steps", len(trace.Steps)),
			Confidence: math.Min(stepDiff/pattern.AvgStepCount, 1.0),
		})
	}

	// 3. Missing expected steps
	for i, expected := range pattern.ExpectedSteps {
		if expected.Frequency < 0.8 {
			continue // step isn't always present, skip
		}
		found := false
		for _, step := range trace.Steps {
			if step.FuncName == expected.FuncName {
				found = true
				break
			}
		}
		if !found {
			deviations = append(deviations, TraceDeviation{
				TraceID:     trace.TraceID,
				Type:        "missing_step",
				Severity:    "critical",
				Description: fmt.Sprintf("%s is usually step #%d (present %.0f%% of the time) but was skipped",
					expected.FuncName, i+1, expected.Frequency*100),
				Expected: expected.FuncName,
				Actual:   "not present",
				StepIdx:  i,
				Confidence: expected.Frequency,
			})
		}
	}

	// 4. Unexpected new steps (never seen before)
	for _, step := range trace.Steps {
		seen := false
		for _, expected := range pattern.ExpectedSteps {
			if step.FuncName == expected.FuncName {
				seen = true
				break
			}
		}
		if !seen {
			deviations = append(deviations, TraceDeviation{
				TraceID:     trace.TraceID,
				Type:        "unexpected_step",
				Severity:    "warning",
				Description: fmt.Sprintf("%s was never seen in previous %d traces of %s",
					step.FuncName, pattern.SampleCount, trace.EntryFunc),
				Expected: "not present",
				Actual:   step.FuncName,
				Confidence: 0.7,
			})
		}
	}

	// 5. Unusual step sequence
	seq := ""
	for i, step := range trace.Steps {
		if i > 0 { seq += "→" }
		seq += step.FuncName
	}
	if _, seen := pattern.StepSequences[seq]; !seen && len(pattern.StepSequences) > 5 {
		deviations = append(deviations, TraceDeviation{
			TraceID:     trace.TraceID,
			Type:        "wrong_order",
			Severity:    "warning",
			Description: fmt.Sprintf("Step sequence never seen before in %d traces", pattern.SampleCount),
			Expected: mostCommonSequence(pattern.StepSequences),
			Actual:   seq,
			Confidence: 0.6,
		})
	}

	// 6. Individual step duration anomaly
	for i, step := range trace.Steps {
		if i >= len(pattern.ExpectedSteps) {
			break
		}
		expected := pattern.ExpectedSteps[i]
		if expected.AvgDuration > 0 && step.FuncName == expected.FuncName {
			ratio := float64(step.DurationNs) / expected.AvgDuration
			if ratio > 5 { // 5x slower than normal
				deviations = append(deviations, TraceDeviation{
					TraceID:     trace.TraceID,
					Type:        "duration_anomaly",
					Severity:    "warning",
					Description: fmt.Sprintf("Step %s took %.0fms, normally %.0fms (%.0fx slower)",
						step.FuncName, float64(step.DurationNs)/1e6, expected.AvgDuration/1e6, ratio),
					Expected: fmt.Sprintf("%.0fms", expected.AvgDuration/1e6),
					Actual:   fmt.Sprintf("%.0fms", float64(step.DurationNs)/1e6),
					StepIdx:  i,
					Confidence: math.Min(ratio/20, 1.0),
				})
			}
		}
	}

	return deviations
}

// GetPattern returns the learned pattern for an entry function.
func (em *ExpectationModel) GetPattern(entryFunc string) *TracePattern {
	em.mu.RLock()
	defer em.mu.RUnlock()
	if p, ok := em.patterns[entryFunc]; ok {
		return p
	}
	return nil
}

// AllPatterns returns all learned patterns.
func (em *ExpectationModel) AllPatterns() []*TracePattern {
	em.mu.RLock()
	defer em.mu.RUnlock()
	var result []*TracePattern
	for _, p := range em.patterns {
		result = append(result, p)
	}
	return result
}

func severityFromZ(z float64) string {
	if z > 5 { return "critical" }
	if z > 3 { return "warning" }
	return "info"
}

func mostCommonSequence(seqs map[string]int) string {
	best := ""
	bestCount := 0
	for seq, count := range seqs {
		if count > bestCount {
			best = seq
			bestCount = count
		}
	}
	return best
}
