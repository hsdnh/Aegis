package causal

import (
	"encoding/json"
	"net/http"
)

// GraphAPI implements the dashboard CausalGraphRegistrar interface.
type GraphAPI struct {
	graph *Graph
}

func NewGraphAPI(g *Graph) *GraphAPI {
	return &GraphAPI{graph: g}
}

// RegisterToMux adds causal chain endpoints to an HTTP mux.
func (a *GraphAPI) RegisterToMux(mux *http.ServeMux, cors func(http.HandlerFunc) http.HandlerFunc) {
	// Full graph topology (for visualization)
	mux.HandleFunc("/api/causal/graph", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"nodes":      a.graph.AllNodes(),
			"edges":      a.graph.AllEdges(),
			"node_count": a.graph.NodeCount(),
			"edge_count": a.graph.EdgeCount(),
		})
	}))

	// Reverse trace: metric anomaly → root cause code
	mux.HandleFunc("/api/causal/trace/reverse", cors(func(w http.ResponseWriter, r *http.Request) {
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "metric parameter required"})
			return
		}
		chain := a.graph.TraceAnomalyToCode(metric, 0)
		w.Header().Set("Content-Type", "application/json")
		if chain == nil {
			json.NewEncoder(w).Encode(map[string]string{"status": "no_path_found", "metric": metric})
			return
		}
		json.NewEncoder(w).Encode(chain)
	}))

	// Forward trace: code node → affected metrics
	mux.HandleFunc("/api/causal/trace/forward", cors(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.URL.Query().Get("node")
		if nodeID == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "node parameter required"})
			return
		}
		paths := a.graph.ForwardFrom(nodeID, 6)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(paths)
	}))

	// All current causal chains (from latest cycle anomalies)
	mux.HandleFunc("/api/causal/chains", cors(func(w http.ResponseWriter, r *http.Request) {
		a.graph.mu.RLock()
		chains := a.graph.cachedChains
		a.graph.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if chains == nil {
			chains = []CausalChain{}
		}
		json.NewEncoder(w).Encode(chains)
	}))
}
