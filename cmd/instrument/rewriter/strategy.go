package rewriter

import (
	"go/ast"
	"strings"
)

// Priority levels for function instrumentation.
const (
	PrioritySkip      byte = 0 // Don't instrument (too trivial or too frequent)
	PriorityTimingOnly byte = 1 // Only record entry/exit timing
	PriorityFullTrace  byte = 2 // Record timing + mark type flags
)

// ClassifyFunc determines how a function should be instrumented
// based on its name, receiver, parameters, and body content.
// This is the "polymorphic self-adaptation" — the tool automatically
// figures out what's worth tracing.
func ClassifyFunc(fn *ast.FuncDecl, srcSnippet string) byte {
	name := fn.Name.Name
	lower := strings.ToLower(name)

	// --- ALWAYS SKIP ---
	// Getters, setters, simple accessors — way too frequent, no diagnostic value
	if isSimpleAccessor(fn) {
		return PrioritySkip
	}

	// String/Error interface methods — called constantly
	if name == "String" || name == "Error" || name == "GoString" {
		return PrioritySkip
	}

	// Sort interface
	if name == "Len" || name == "Less" || name == "Swap" {
		return PrioritySkip
	}

	// Serialization
	if strings.HasPrefix(name, "Marshal") || strings.HasPrefix(name, "Unmarshal") {
		return PrioritySkip
	}

	// --- FULL TRACE (highest value) ---
	// HTTP handlers — every request matters
	if hasHTTPHandlerSignature(fn) {
		return PriorityFullTrace
	}

	// Functions that touch database
	if containsAny(srcSnippet, []string{
		"db.Exec", "db.Query", "db.QueryRow",
		"tx.Exec", "tx.Query", "tx.Commit",
		"sql.Open", ".Prepare(",
	}) {
		return PriorityFullTrace
	}

	// Functions that touch Redis
	if containsAny(srcSnippet, []string{
		"rdb.", "redis.", ".Set(", ".Get(", ".LPush(", ".BRPop(",
		".HSet(", ".HGet(", ".SAdd(", ".ZAdd(",
		".SetNX(", ".Incr(", ".Decr(",
	}) {
		return PriorityFullTrace
	}

	// Functions that make external HTTP calls
	if containsAny(srcSnippet, []string{
		"http.Get(", "http.Post(", "http.Do(",
		"client.Do(", "client.Get(",
	}) {
		return PriorityFullTrace
	}

	// Functions with "Order", "Push", "Search" in name — business critical
	if containsAny(lower, []string{
		"order", "push", "search", "dispatch",
		"notify", "purchase", "checkout", "payment",
	}) {
		return PriorityFullTrace
	}

	// Functions that create goroutines — potential leak source
	if strings.Contains(srcSnippet, "go ") || strings.Contains(srcSnippet, "go\t") {
		return PriorityFullTrace
	}

	// --- TIMING ONLY (medium value) ---
	// Exported functions — part of the API surface
	if ast.IsExported(name) {
		return PriorityTimingOnly
	}

	// Functions with context parameter — usually important
	if hasContextParam(fn) {
		return PriorityTimingOnly
	}

	// Functions with error return — failure points
	if hasErrorReturn(fn) {
		return PriorityTimingOnly
	}

	// --- DEFAULT: SKIP for small unexported functions ---
	return PrioritySkip
}

func isSimpleAccessor(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return true
	}
	// Single return statement = likely a getter
	if len(fn.Body.List) <= 1 {
		return true
	}
	return false
}

func hasHTTPHandlerSignature(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		if typeStr := typeToString(param.Type); typeStr != "" {
			if strings.Contains(typeStr, "ResponseWriter") ||
				strings.Contains(typeStr, "http.Request") ||
				strings.Contains(typeStr, "gin.Context") ||
				strings.Contains(typeStr, "echo.Context") ||
				strings.Contains(typeStr, "fiber.Ctx") {
				return true
			}
		}
	}
	return false
}

func hasContextParam(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		if typeStr := typeToString(param.Type); strings.Contains(typeStr, "context.Context") || typeStr == "Context" {
			return true
		}
	}
	return false
}

func hasErrorReturn(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, result := range fn.Type.Results.List {
		if typeStr := typeToString(result.Type); typeStr == "error" {
			return true
		}
	}
	return false
}

func typeToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
	case *ast.StarExpr:
		return "*" + typeToString(t.X)
	case *ast.ArrayType:
		return "[]" + typeToString(t.Elt)
	}
	return ""
}

func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
