// Package causal implements bidirectional causal chain tracing.
//
// It builds a graph connecting: code functions ↔ data stores ↔ runtime metrics ↔ observable symptoms.
// Given any anomaly, it can trace backwards to the root cause code line.
// Given any code change, it can trace forward to predict affected metrics.
//
// The graph is built from three sources:
//   - L0 static scan: code → Redis/MySQL/HTTP call relationships
//   - L1.5 trace data: function call trees + timing
//   - L1 metrics: runtime values that indicate health
package causal

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/scanner"
	"github.com/hsdnh/ai-ops-agent/internal/tracecollector"
	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

// NodeType categorizes what a node in the causal graph represents.
type NodeType string

const (
	NodeFunction NodeType = "function"  // a Go function
	NodeRedisKey NodeType = "redis_key" // a Redis key pattern
	NodeSQLTable NodeType = "sql_table" // a MySQL table
	NodeHTTPRoute NodeType = "http_route" // an API endpoint
	NodeMetric   NodeType = "metric"    // a monitored metric
	NodeCronJob  NodeType = "cron_job"  // a scheduled task
)

// Node is a vertex in the causal graph.
type Node struct {
	ID       string   `json:"id"`
	Type     NodeType `json:"type"`
	Name     string   `json:"name"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// EdgeType describes the causal relationship between two nodes.
type EdgeType string

const (
	EdgeCalls    EdgeType = "calls"     // function A calls function B
	EdgeReads    EdgeType = "reads"     // function reads from data store
	EdgeWrites   EdgeType = "writes"    // function writes to data store
	EdgeProduces EdgeType = "produces"  // function produces metric
	EdgeTriggers EdgeType = "triggers"  // cron triggers function
	EdgeAffects  EdgeType = "affects"   // change in A affects B
)

// Edge is a directed connection between two nodes.
type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Type     EdgeType `json:"type"`
	Weight   float64  `json:"weight,omitempty"` // strength of relationship (0-1)
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Graph is the bidirectional causal graph.
type Graph struct {
	mu           sync.RWMutex
	nodes        map[string]*Node
	forward      map[string][]Edge // from → []Edge (forward direction)
	backward     map[string][]Edge // to → []Edge (reverse direction)
	cachedChains []CausalChain     // latest anomaly chains, updated each cycle
}

func NewGraph() *Graph {
	return &Graph{
		nodes:    make(map[string]*Node),
		forward:  make(map[string][]Edge),
		backward: make(map[string][]Edge),
	}
}

func (g *Graph) AddNode(n Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = &n
}

func (g *Graph) AddEdge(e Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forward[e.From] = append(g.forward[e.From], e)
	g.backward[e.To] = append(g.backward[e.To], e)
}

// ForwardFrom returns all nodes reachable from the given node (forward direction).
// "If this function changes, what else is affected?"
func (g *Graph) ForwardFrom(nodeID string, maxDepth int) []CausalPath {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bfs(nodeID, maxDepth, g.forward)
}

// BackwardFrom returns all nodes that lead to the given node (reverse direction).
// "This metric is anomalous — what could have caused it?"
func (g *Graph) BackwardFrom(nodeID string, maxDepth int) []CausalPath {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bfs(nodeID, maxDepth, g.backward)
}

// CausalPath represents a chain of causation from source to target.
type CausalPath struct {
	Steps []CausalStep `json:"steps"`
	Depth int          `json:"depth"`
	Score float64      `json:"score"` // combined edge weights
}

// CausalStep is one link in a causal chain.
type CausalStep struct {
	NodeID   string   `json:"node_id"`
	NodeType NodeType `json:"node_type"`
	NodeName string   `json:"node_name"`
	EdgeType EdgeType `json:"edge_type,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
}

func (g *Graph) bfs(startID string, maxDepth int, adj map[string][]Edge) []CausalPath {
	if maxDepth <= 0 {
		maxDepth = 10
	}

	type state struct {
		nodeID string
		path   []CausalStep
		score  float64
		depth  int
	}

	var results []CausalPath
	visited := make(map[string]bool)
	queue := []state{{nodeID: startID, score: 1.0}}

	startNode := g.nodes[startID]
	if startNode == nil {
		return nil
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if visited[cur.nodeID] {
			continue
		}
		visited[cur.nodeID] = true

		node := g.nodes[cur.nodeID]
		if node == nil {
			continue
		}

		step := CausalStep{
			NodeID:   node.ID,
			NodeType: node.Type,
			NodeName: node.Name,
			File:     node.File,
			Line:     node.Line,
		}
		path := append(cur.path, step)

		// Record this as a result if it's not the start node
		if cur.nodeID != startID {
			results = append(results, CausalPath{
				Steps: copySteps(path),
				Depth: cur.depth,
				Score: cur.score,
			})
		}

		if cur.depth >= maxDepth {
			continue
		}

		for _, edge := range adj[cur.nodeID] {
			target := edge.To
			if _, isForward := adj[edge.From]; !isForward {
				target = edge.From // backward traversal
			}
			if visited[target] {
				continue
			}
			w := edge.Weight
			if w == 0 {
				w = 0.8 // default weight
			}
			queue = append(queue, state{
				nodeID: target,
				path:   path,
				score:  cur.score * w,
				depth:  cur.depth + 1,
			})
		}
	}

	// Sort by score (highest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

func copySteps(steps []CausalStep) []CausalStep {
	out := make([]CausalStep, len(steps))
	copy(out, steps)
	return out
}

// --- Graph Builder: populate from L0 scan + L1 metrics + L1.5 traces ---

// BuildFromScan populates the graph from L0 static analysis results.
func (g *Graph) BuildFromScan(scan *scanner.ScanResult) {
	// Add HTTP route nodes
	for _, rt := range scan.Routes {
		g.AddNode(Node{
			ID:   fmt.Sprintf("route:%s:%s", rt.Method, rt.Path),
			Type: NodeHTTPRoute,
			Name: fmt.Sprintf("%s %s", rt.Method, rt.Path),
			File: rt.File, Line: rt.Line,
			Metadata: map[string]string{"handler": rt.Handler},
		})
		// Route → Handler function
		if rt.Handler != "" {
			handlerID := fmt.Sprintf("func:%s", rt.Handler)
			g.AddNode(Node{ID: handlerID, Type: NodeFunction, Name: rt.Handler, File: rt.File, Line: rt.Line})
			g.AddEdge(Edge{From: "route:" + rt.Method + ":" + rt.Path, To: handlerID, Type: EdgeCalls, Weight: 1.0})
		}
	}

	// Add Redis key nodes and function→key edges
	for _, rk := range scan.RedisKeys {
		keyID := fmt.Sprintf("redis:%s", rk.KeyPattern)
		g.AddNode(Node{ID: keyID, Type: NodeRedisKey, Name: rk.KeyPattern})

		funcID := fmt.Sprintf("func:%s:%d", rk.File, rk.Line)
		g.AddNode(Node{ID: funcID, Type: NodeFunction, Name: rk.File, File: rk.File, Line: rk.Line})

		edgeType := EdgeReads
		switch rk.Operation {
		case "LPush", "RPush", "Set", "SetNX", "HSet", "SAdd", "ZAdd", "Incr":
			edgeType = EdgeWrites
		}
		g.AddEdge(Edge{From: funcID, To: keyID, Type: edgeType, Weight: 0.9})
	}

	// Add SQL table nodes
	for _, sq := range scan.SQLTables {
		tableID := fmt.Sprintf("sql:%s", sq.Table)
		g.AddNode(Node{ID: tableID, Type: NodeSQLTable, Name: sq.Table})

		funcID := fmt.Sprintf("func:%s:%d", sq.File, sq.Line)
		g.AddNode(Node{ID: funcID, Type: NodeFunction, Name: sq.File, File: sq.File, Line: sq.Line})

		edgeType := EdgeReads
		if sq.Operation == "INSERT" || sq.Operation == "UPDATE" || sq.Operation == "DELETE" {
			edgeType = EdgeWrites
		}
		g.AddEdge(Edge{From: funcID, To: tableID, Type: edgeType, Weight: 0.9})
	}

	// Add cron job nodes
	for _, cj := range scan.CronJobs {
		cronID := fmt.Sprintf("cron:%s", cj.Schedule)
		g.AddNode(Node{ID: cronID, Type: NodeCronJob, Name: cj.Schedule})
		if cj.Handler != "" {
			handlerID := fmt.Sprintf("func:%s", cj.Handler)
			g.AddNode(Node{ID: handlerID, Type: NodeFunction, Name: cj.Handler, File: cj.File, Line: cj.Line})
			g.AddEdge(Edge{From: cronID, To: handlerID, Type: EdgeTriggers, Weight: 1.0})
		}
	}

	// Link Redis keys to their monitoring metrics
	for _, rk := range scan.RedisKeys {
		keyID := fmt.Sprintf("redis:%s", rk.KeyPattern)
		metricName := fmt.Sprintf("redis.keys.%s.total_length",
			strings.NewReplacer("*", "all", ":", "_", ".", "_").Replace(rk.KeyPattern))
		metricID := fmt.Sprintf("metric:%s", metricName)
		g.AddNode(Node{ID: metricID, Type: NodeMetric, Name: metricName})
		g.AddEdge(Edge{From: keyID, To: metricID, Type: EdgeProduces, Weight: 1.0})
	}

	// Link SQL tables to their monitoring metrics
	for _, sq := range scan.SQLTables {
		tableID := fmt.Sprintf("sql:%s", sq.Table)
		metricID := fmt.Sprintf("metric:mysql.check.%s", sq.Table)
		g.AddNode(Node{ID: metricID, Type: NodeMetric, Name: "mysql.check." + sq.Table})
		g.AddEdge(Edge{From: tableID, To: metricID, Type: EdgeProduces, Weight: 0.8})
	}
}

// EnrichFromTrace adds runtime call relationships from trace data.
func (g *Graph) EnrichFromTrace(trace *tracecollector.AnalyzedWindow) {
	if trace == nil {
		return
	}
	for _, hs := range trace.HotSpots {
		funcID := fmt.Sprintf("func:%s", hs.FuncName)
		g.AddNode(Node{
			ID: funcID, Type: NodeFunction, Name: hs.FuncName,
			Metadata: map[string]string{
				"avg_duration": hs.AvgTime.String(),
				"call_count":   fmt.Sprintf("%d", hs.CallCount),
				"pct_of_total": fmt.Sprintf("%.1f%%", hs.PctOfTotal),
			},
		})
	}
}

// EnrichFromMetrics links anomalous metrics into the graph.
func (g *Graph) EnrichFromMetrics(metrics []types.Metric, ruleResults []types.RuleResult) {
	for _, m := range metrics {
		metricID := fmt.Sprintf("metric:%s", m.Name)
		node := Node{ID: metricID, Type: NodeMetric, Name: m.Name,
			Metadata: map[string]string{"value": fmt.Sprintf("%.2f", m.Value)}}
		g.AddNode(node)
	}

	// Mark triggered rules with higher weight edges
	for _, rr := range ruleResults {
		if rr.Triggered {
			metricID := fmt.Sprintf("metric:%s", rr.MetricName)
			if n, ok := g.nodes[metricID]; ok {
				if n.Metadata == nil {
					n.Metadata = make(map[string]string)
				}
				n.Metadata["triggered"] = "true"
				n.Metadata["severity"] = rr.Severity.String()
				n.Metadata["threshold"] = fmt.Sprintf("%.2f", rr.Threshold)
			}
		}
	}
}

// --- High-level analysis ---

// CausalChain represents a complete backward trace from symptom to root cause.
type CausalChain struct {
	Symptom    string       `json:"symptom"`     // what was observed
	RootCause  string       `json:"root_cause"`  // suspected code location
	Chain      []ChainLink  `json:"chain"`       // step by step
	Confidence float64      `json:"confidence"`
	Timestamp  time.Time    `json:"timestamp"`
}

// ChainLink is one step in a human-readable causal chain.
type ChainLink struct {
	Description string `json:"description"`
	NodeID      string `json:"node_id"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Evidence    string `json:"evidence,omitempty"` // metric value, timing data, etc.
}

// TraceAnomalyToCode finds the code-level root cause of an anomalous metric.
// This is the "reverse trace" — from symptom back to the code line.
func (g *Graph) TraceAnomalyToCode(metricName string, metricValue float64) *CausalChain {
	metricID := fmt.Sprintf("metric:%s", metricName)

	paths := g.BackwardFrom(metricID, 8)
	if len(paths) == 0 {
		return nil
	}

	// Find the deepest path that ends at a function node
	var bestPath *CausalPath
	for i, p := range paths {
		lastStep := p.Steps[len(p.Steps)-1]
		if lastStep.NodeType == NodeFunction {
			bestPath = &paths[i]
			break
		}
	}
	if bestPath == nil && len(paths) > 0 {
		bestPath = &paths[0]
	}
	if bestPath == nil {
		return nil
	}

	// Build human-readable chain
	chain := &CausalChain{
		Symptom:    fmt.Sprintf("%s = %.2f", metricName, metricValue),
		Confidence: bestPath.Score,
		Timestamp:  time.Now(),
	}

	for i, step := range bestPath.Steps {
		link := ChainLink{
			NodeID: step.NodeID,
			File:   step.File,
			Line:   step.Line,
		}

		switch step.NodeType {
		case NodeMetric:
			if node, ok := g.nodes[step.NodeID]; ok && node.Metadata != nil {
				link.Description = fmt.Sprintf("Anomaly detected: %s = %s", step.NodeName, node.Metadata["value"])
				link.Evidence = fmt.Sprintf("threshold: %s, severity: %s", node.Metadata["threshold"], node.Metadata["severity"])
			} else {
				link.Description = fmt.Sprintf("Anomaly detected: %s", step.NodeName)
			}
		case NodeRedisKey:
			link.Description = fmt.Sprintf("Linked to Redis key: %s", step.NodeName)
		case NodeSQLTable:
			link.Description = fmt.Sprintf("Linked to MySQL table: %s", step.NodeName)
		case NodeFunction:
			desc := fmt.Sprintf("Code location: %s", step.NodeName)
			if node, ok := g.nodes[step.NodeID]; ok && node.Metadata != nil {
				if pct := node.Metadata["pct_of_total"]; pct != "" {
					desc += fmt.Sprintf(" (占总耗时 %s)", pct)
				}
				if dur := node.Metadata["avg_duration"]; dur != "" {
					desc += fmt.Sprintf(" (avg %s)", dur)
				}
			}
			link.Description = desc
		case NodeHTTPRoute:
			link.Description = fmt.Sprintf("Exposed via API: %s", step.NodeName)
		case NodeCronJob:
			link.Description = fmt.Sprintf("Triggered by cron: %s", step.NodeName)
		}

		chain.Chain = append(chain.Chain, link)

		// Last function node is the root cause
		if i == len(bestPath.Steps)-1 && step.NodeType == NodeFunction {
			chain.RootCause = fmt.Sprintf("%s (%s:%d)", step.NodeName, step.File, step.Line)
		}
	}

	if chain.RootCause == "" && len(bestPath.Steps) > 0 {
		last := bestPath.Steps[len(bestPath.Steps)-1]
		chain.RootCause = last.NodeName
	}

	return chain
}

// TraceAllAnomalies finds causal chains for all triggered rules in a snapshot.
// Results are cached for the dashboard API.
func (g *Graph) TraceAllAnomalies(snapshot *types.Snapshot) []CausalChain {
	var chains []CausalChain
	for _, rr := range snapshot.RuleResults {
		if !rr.Triggered {
			continue
		}
		chain := g.TraceAnomalyToCode(rr.MetricName, rr.MetricValue)
		if chain != nil {
			chains = append(chains, *chain)
		}
	}
	g.mu.Lock()
	g.cachedChains = chains
	g.mu.Unlock()
	return chains
}

// NodeCount returns the number of nodes in the graph.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// EdgeCount returns the number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	count := 0
	for _, edges := range g.forward {
		count += len(edges)
	}
	return count
}

// AllNodes returns all nodes (for dashboard visualization).
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var nodes []*Node
	for _, n := range g.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

// AllEdges returns all edges (for dashboard visualization).
func (g *Graph) AllEdges() []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var edges []Edge
	for _, ee := range g.forward {
		edges = append(edges, ee...)
	}
	return edges
}
