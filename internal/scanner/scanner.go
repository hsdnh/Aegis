// Package scanner performs static analysis on Go projects to discover
// all monitorable endpoints, data stores, scheduled tasks, and error patterns.
// This is the L0 "project analysis" phase.
package scanner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ScanResult holds everything discovered about a project.
type ScanResult struct {
	ProjectPath string         `json:"project_path"`
	Routes      []Route        `json:"routes"`
	RedisKeys   []RedisKeyUsage `json:"redis_keys"`
	SQLTables   []SQLUsage     `json:"sql_tables"`
	CronJobs    []CronJob      `json:"cron_jobs"`
	LogPatterns []LogPattern   `json:"log_patterns"`
	ConfigRefs  []ConfigRef    `json:"config_refs"`
	GoFiles     int            `json:"go_files_scanned"`
}

// Route is a discovered HTTP endpoint.
type Route struct {
	Method  string `json:"method"`  // GET, POST, PUT, DELETE
	Path    string `json:"path"`    // "/v1/overview"
	Handler string `json:"handler"` // "handler.Overview"
	File    string `json:"file"`
	Line    int    `json:"line"`
}

// RedisKeyUsage is a discovered Redis key pattern.
type RedisKeyUsage struct {
	Operation string `json:"operation"` // Set, Get, LPush, BRPop, etc.
	KeyPattern string `json:"key_pattern"` // "mh:queue:search:pending" or "mh:push:dedup:{var}"
	File       string `json:"file"`
	Line       int    `json:"line"`
}

// SQLUsage is a discovered SQL table/query.
type SQLUsage struct {
	Operation string `json:"operation"` // SELECT, INSERT, UPDATE, DELETE
	Table     string `json:"table"`
	RawSQL    string `json:"raw_sql,omitempty"` // truncated
	File      string `json:"file"`
	Line      int    `json:"line"`
}

// CronJob is a discovered scheduled task.
type CronJob struct {
	Schedule string `json:"schedule"` // "*/5 * * * *"
	Handler  string `json:"handler"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// LogPattern is a discovered error log pattern.
type LogPattern struct {
	Level   string `json:"level"` // Error, Warn, Fatal, Panic
	Pattern string `json:"pattern"`
	File    string `json:"file"`
	Line    int    `json:"line"`
}

// ConfigRef is a discovered connection or config reference.
type ConfigRef struct {
	Type  string `json:"type"` // "redis_addr", "mysql_dsn", "http_listen"
	Value string `json:"value"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

// ScanProject walks a Go project and discovers all monitorable components.
func ScanProject(projectPath string) (*ScanResult, error) {
	result := &ScanResult{ProjectPath: projectPath}

	err := filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == ".git" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		result.GoFiles++
		return scanFile(path, projectPath, result)
	})

	return result, err
}

func scanFile(path, projectRoot string, result *ScanResult) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil // skip unparseable files
	}

	relPath, _ := filepath.Rel(projectRoot, path)

	// Also read raw source for regex-based patterns
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	srcStr := string(src)

	// AST-based scanning
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		line := fset.Position(call.Pos()).Line

		// Scan for HTTP routes
		if route := extractRoute(call, relPath, line); route != nil {
			result.Routes = append(result.Routes, *route)
		}

		// Scan for Redis operations
		if redis := extractRedisOp(call, relPath, line); redis != nil {
			result.RedisKeys = append(result.RedisKeys, *redis)
		}

		// Scan for cron registrations
		if cron := extractCron(call, relPath, line); cron != nil {
			result.CronJobs = append(result.CronJobs, *cron)
		}

		// Scan for log patterns
		if logp := extractLogPattern(call, relPath, line); logp != nil {
			result.LogPatterns = append(result.LogPatterns, *logp)
		}

		return true
	})

	// Regex-based scanning (for things AST can't easily catch)
	result.SQLTables = append(result.SQLTables, extractSQL(srcStr, relPath)...)
	result.ConfigRefs = append(result.ConfigRefs, extractConfigRefs(srcStr, relPath)...)

	return nil
}

// --- Route Extraction ---
// Matches: r.GET("/path", handler), router.POST("/path", handler), etc.
// Also: gin.Group, echo.Group patterns

func extractRoute(call *ast.CallExpr, file string, line int) *Route {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	method := sel.Sel.Name
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS",
		"Handle", "HandleFunc", "Any":
	default:
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringLit(call.Args[0])
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[len(call.Args)-1])
	}

	httpMethod := method
	if method == "Handle" || method == "HandleFunc" || method == "Any" {
		httpMethod = "ANY"
	}

	return &Route{
		Method:  httpMethod,
		Path:    path,
		Handler: handler,
		File:    file,
		Line:    line,
	}
}

// --- Redis Extraction ---
// Matches: rdb.Set(...), client.LPush(...), rdb.Get(...), etc.

var redisOps = map[string]bool{
	"Set": true, "Get": true, "Del": true, "Exists": true,
	"SetNX": true, "SetEX": true, "Incr": true, "Decr": true,
	"LPush": true, "RPush": true, "LPop": true, "RPop": true, "BRPop": true, "BLPop": true,
	"LLen": true, "LRange": true,
	"SAdd": true, "SMembers": true, "SCard": true, "SIsMember": true,
	"HSet": true, "HGet": true, "HGetAll": true, "HDel": true, "HLen": true,
	"ZAdd": true, "ZRange": true, "ZRangeByScore": true, "ZCard": true, "ZRem": true,
	"Expire": true, "TTL": true, "Keys": true, "Scan": true,
	"Publish": true, "Subscribe": true,
	"Pipeline": true, "TxPipeline": true,
}

func extractRedisOp(call *ast.CallExpr, file string, line int) *RedisKeyUsage {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	op := sel.Sel.Name
	if !redisOps[op] {
		return nil
	}

	// First argument is usually the context, second is the key
	keyIdx := 0
	if len(call.Args) >= 2 {
		// Check if first arg looks like context
		if argStr := exprToString(call.Args[0]); argStr == "ctx" || strings.Contains(argStr, "context") {
			keyIdx = 1
		}
	}

	if keyIdx >= len(call.Args) {
		return &RedisKeyUsage{Operation: op, KeyPattern: "(dynamic)", File: file, Line: line}
	}

	key := extractKeyPattern(call.Args[keyIdx])
	if key == "" {
		key = "(dynamic)"
	}

	return &RedisKeyUsage{
		Operation:  op,
		KeyPattern: key,
		File:       file,
		Line:       line,
	}
}

// extractKeyPattern tries to reconstruct a Redis key from an expression.
// Handles: "literal", "prefix:" + var, fmt.Sprintf("pattern:%s", var)
func extractKeyPattern(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.Trim(e.Value, "\"'`")
		}
	case *ast.BinaryExpr:
		// "prefix:" + variable → "prefix:{var}"
		if e.Op == token.ADD {
			left := extractKeyPattern(e.X)
			right := extractKeyPattern(e.Y)
			if left != "" && right == "" {
				return left + "{dynamic}"
			}
			return left + right
		}
	case *ast.CallExpr:
		// fmt.Sprintf("pattern:%s", ...) → extract format string
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if sel.Sel.Name == "Sprintf" && len(e.Args) > 0 {
				if fmtStr := extractStringLit(e.Args[0]); fmtStr != "" {
					// Replace %s/%d/%v with {var}
					cleaned := regexp.MustCompile(`%[sdvfx]`).ReplaceAllString(fmtStr, "{var}")
					return cleaned
				}
			}
		}
	case *ast.Ident:
		return "" // variable reference — can't resolve statically
	}
	return ""
}

// --- SQL Extraction (regex-based) ---

var sqlPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(SELECT)\s+.*?\s+FROM\s+(\w+)`),
	regexp.MustCompile(`(?i)(INSERT)\s+INTO\s+(\w+)`),
	regexp.MustCompile(`(?i)(UPDATE)\s+(\w+)\s+SET`),
	regexp.MustCompile(`(?i)(DELETE)\s+FROM\s+(\w+)`),
	regexp.MustCompile(`(?i)(ALTER)\s+TABLE\s+(\w+)`),
	regexp.MustCompile(`(?i)(CREATE)\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`),
}

func extractSQL(src, file string) []SQLUsage {
	var results []SQLUsage
	seen := make(map[string]bool)
	lines := strings.Split(src, "\n")

	for lineNum, line := range lines {
		for _, pattern := range sqlPatterns {
			matches := pattern.FindStringSubmatch(line)
			if len(matches) >= 3 {
				op := strings.ToUpper(matches[1])
				table := matches[2]
				key := op + ":" + table
				if seen[key] {
					continue
				}
				seen[key] = true
				results = append(results, SQLUsage{
					Operation: op,
					Table:     table,
					RawSQL:    truncateStr(strings.TrimSpace(line), 200),
					File:      file,
					Line:      lineNum + 1,
				})
			}
		}
	}
	return results
}

// --- Cron Extraction ---

func extractCron(call *ast.CallExpr, file string, line int) *CronJob {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "AddFunc" && sel.Sel.Name != "AddJob" {
		return nil
	}

	if len(call.Args) < 2 {
		return nil
	}

	schedule := extractStringLit(call.Args[0])
	if schedule == "" || !looksLikeCron(schedule) {
		return nil
	}

	handler := exprToString(call.Args[1])

	return &CronJob{
		Schedule: schedule,
		Handler:  handler,
		File:     file,
		Line:     line,
	}
}

// --- Log Pattern Extraction ---

var logMethods = map[string]string{
	"Error": "ERROR", "Errorf": "ERROR", "Errorln": "ERROR",
	"Warn": "WARN", "Warnf": "WARN", "Warning": "WARN", "Warningf": "WARN",
	"Fatal": "FATAL", "Fatalf": "FATAL", "Fatalln": "FATAL",
	"Panic": "PANIC", "Panicf": "PANIC", "Panicln": "PANIC",
}

func extractLogPattern(call *ast.CallExpr, file string, line int) *LogPattern {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	level, ok := logMethods[sel.Sel.Name]
	if !ok {
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	pattern := extractStringLit(call.Args[0])
	if pattern == "" {
		pattern = exprToString(call.Args[0])
	}
	pattern = truncateStr(pattern, 100)

	return &LogPattern{
		Level:   level,
		Pattern: pattern,
		File:    file,
		Line:    line,
	}
}

// --- Config Reference Extraction (regex) ---

var configPatterns = []struct {
	re   *regexp.Regexp
	kind string
}{
	{regexp.MustCompile(`(?i)redis.*(?:addr|address|host).*["']([^"']+:\d+)["']`), "redis_addr"},
	{regexp.MustCompile(`(?i)(?:dsn|mysql|database).*["']([^"']*@tcp\([^)]+\)[^"']*)["']`), "mysql_dsn"},
	{regexp.MustCompile(`(?i)(?:listen|addr|bind).*["'](:\d{4,5})["']`), "http_listen"},
	{regexp.MustCompile(`(?i)(?:listen|addr|bind).*["'](0\.0\.0\.0:\d+|localhost:\d+)["']`), "http_listen"},
}

func extractConfigRefs(src, file string) []ConfigRef {
	var results []ConfigRef
	lines := strings.Split(src, "\n")

	for lineNum, line := range lines {
		for _, cp := range configPatterns {
			matches := cp.re.FindStringSubmatch(line)
			if len(matches) >= 2 {
				results = append(results, ConfigRef{
					Type:  cp.kind,
					Value: matches[1],
					File:  file,
					Line:  lineNum + 1,
				})
			}
		}
	}
	return results
}

// --- Helpers ---

func extractStringLit(expr ast.Expr) string {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		return strings.Trim(lit.Value, "\"'`")
	}
	return ""
}

func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.BasicLit:
		return strings.Trim(e.Value, "\"'`")
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}

func looksLikeCron(s string) bool {
	parts := strings.Fields(s)
	return len(parts) >= 5 && len(parts) <= 7
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// FormatForAI returns a human-readable summary suitable for AI prompts.
func (r *ScanResult) FormatForAI() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("项目扫描结果 (%s, %d Go文件)\n\n", r.ProjectPath, r.GoFiles))

	if len(r.Routes) > 0 {
		sb.WriteString(fmt.Sprintf("### API 路由 (%d)\n", len(r.Routes)))
		for _, rt := range r.Routes {
			sb.WriteString(fmt.Sprintf("  %s %s → %s (%s:%d)\n", rt.Method, rt.Path, rt.Handler, rt.File, rt.Line))
		}
		sb.WriteString("\n")
	}

	if len(r.RedisKeys) > 0 {
		sb.WriteString(fmt.Sprintf("### Redis Key 模式 (%d)\n", len(r.RedisKeys)))
		seen := make(map[string]bool)
		for _, rk := range r.RedisKeys {
			key := rk.Operation + ":" + rk.KeyPattern
			if seen[key] {
				continue
			}
			seen[key] = true
			sb.WriteString(fmt.Sprintf("  %s %s (%s:%d)\n", rk.Operation, rk.KeyPattern, rk.File, rk.Line))
		}
		sb.WriteString("\n")
	}

	if len(r.SQLTables) > 0 {
		sb.WriteString(fmt.Sprintf("### SQL 表操作 (%d)\n", len(r.SQLTables)))
		for _, sq := range r.SQLTables {
			sb.WriteString(fmt.Sprintf("  %s %s (%s:%d)\n", sq.Operation, sq.Table, sq.File, sq.Line))
		}
		sb.WriteString("\n")
	}

	if len(r.CronJobs) > 0 {
		sb.WriteString(fmt.Sprintf("### 定时任务 (%d)\n", len(r.CronJobs)))
		for _, cj := range r.CronJobs {
			sb.WriteString(fmt.Sprintf("  %s → %s (%s:%d)\n", cj.Schedule, cj.Handler, cj.File, cj.Line))
		}
		sb.WriteString("\n")
	}

	if len(r.LogPatterns) > 0 {
		sb.WriteString(fmt.Sprintf("### 错误日志模式 (%d)\n", len(r.LogPatterns)))
		seen := make(map[string]bool)
		for _, lp := range r.LogPatterns {
			if seen[lp.Pattern] {
				continue
			}
			seen[lp.Pattern] = true
			sb.WriteString(fmt.Sprintf("  [%s] %s (%s:%d)\n", lp.Level, lp.Pattern, lp.File, lp.Line))
		}
		sb.WriteString("\n")
	}

	if len(r.ConfigRefs) > 0 {
		sb.WriteString(fmt.Sprintf("### 配置引用 (%d)\n", len(r.ConfigRefs)))
		for _, cr := range r.ConfigRefs {
			sb.WriteString(fmt.Sprintf("  [%s] %s (%s:%d)\n", cr.Type, cr.Value, cr.File, cr.Line))
		}
	}

	return sb.String()
}
