package rewriter

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"strings"
)

const sentinel = "// __aiops_instrumented"
const probeImport = `"github.com/hsdnh/ai-ops-agent/sdk/probe"`

// Config controls the instrumentation behavior.
type Config struct {
	DryRun      bool
	Verbose     bool
	ExcludeDirs map[string]bool
	MinLines    int
}

// ProcessFile parses a Go file, injects trace probes, and writes it back.
func ProcessFile(path string, cfg Config) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	// Skip already instrumented files
	if isInstrumented(file) {
		if cfg.Verbose {
			log.Printf("SKIP (already instrumented): %s", path)
		}
		return nil
	}

	// Skip main-less test helpers, generated files, etc.
	if isGenerated(file) {
		return nil
	}

	// Collect functions to instrument
	var targets []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if shouldSkipFunc(fn, cfg) {
			continue
		}
		targets = append(targets, fn)
	}

	if len(targets) == 0 {
		if cfg.Verbose {
			log.Printf("SKIP (no eligible functions): %s", path)
		}
		return nil
	}

	// Read original source (we'll do text-based injection for reliability)
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Build injection plan
	type injection struct {
		offset int    // byte offset in source where to insert
		code   string // code to insert
		funcName string
	}

	var injections []injection
	pkgName := file.Name.Name

	for _, fn := range targets {
		funcName := qualifiedName(fn, pkgName)
		lineNum := fset.Position(fn.Pos()).Line
		bodyStart := fset.Position(fn.Body.Lbrace).Offset + 1 // after '{'

		probeCode := fmt.Sprintf(
			"\n%s\nif __aiops_p.Active(){__aiops_t:=__aiops_p.Enter(__aiops_id_%s);defer __aiops_p.Exit(__aiops_t)}\n",
			sentinel, sanitizeID(fn.Name.Name),
		)

		injections = append(injections, injection{
			offset:   bodyStart,
			code:     probeCode,
			funcName: funcName,
		})

		if cfg.Verbose {
			log.Printf("INJECT: %s:%d %s", path, lineNum, funcName)
		}
	}

	if cfg.DryRun {
		for _, inj := range injections {
			fmt.Printf("[DRY RUN] %s: would inject probe at %s\n", path, inj.funcName)
		}
		return nil
	}

	// IMPORTANT: Add import and var declarations FIRST (uses original AST offsets),
	// THEN inject function probes (which shift offsets).
	// addImportAndVars uses fset positions from the original parse — must run before body edits.
	header := buildHeader(targets, pkgName, file)
	result := addImportAndVars(src, header, fset, file)

	// Re-parse to get new offsets after import/header injection
	fset2 := token.NewFileSet()
	file2, err := parser.ParseFile(fset2, path, result, parser.ParseComments)
	if err != nil {
		// Fallback: just write with imports, skip probe injection
		log.Printf("WARNING: re-parse failed for %s after import injection: %v", path, err)
	} else {
		// Rebuild injection offsets from re-parsed AST
		injections = injections[:0]
		idx := 0
		for _, decl := range file2.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if shouldSkipFunc(fn, cfg) {
				continue
			}
			if idx >= len(targets) {
				break
			}
			bodyStart := fset2.Position(fn.Body.Lbrace).Offset + 1
			probeCode := fmt.Sprintf(
				"\n%s\nif __aiops_p.Active(){__aiops_t:=__aiops_p.Enter(__aiops_id_%s);defer __aiops_p.Exit(__aiops_t)}\n",
				sentinel, sanitizeID(fn.Name.Name),
			)
			injections = append(injections, injection{offset: bodyStart, code: probeCode, funcName: qualifiedName(fn, pkgName)})
			idx++
		}

		// Apply injections (reverse order to preserve offsets)
		for i := len(injections) - 1; i >= 0; i-- {
			inj := injections[i]
			result = insertAt(result, inj.offset, []byte(inj.code))
		}
	}

	// Format the result
	formatted, err := format.Source(result)
	if err != nil {
		// If formatting fails, write unformatted (better than losing work)
		log.Printf("WARNING: gofmt failed for %s, writing unformatted: %v", path, err)
		formatted = result
	}

	if err := os.WriteFile(path, formatted, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	log.Printf("INSTRUMENTED: %s (%d functions)", path, len(injections))
	return nil
}

func isInstrumented(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, "__aiops_instrumented") {
				return true
			}
		}
	}
	return false
}

func isGenerated(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, "Code generated") || strings.Contains(c.Text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}

func shouldSkipFunc(fn *ast.FuncDecl, cfg Config) bool {
	name := fn.Name.Name

	// Skip unexported tiny helpers
	if !ast.IsExported(name) && bodyLineCount(fn) < cfg.MinLines {
		return true
	}

	// Skip init() — we inject our own init
	if name == "init" {
		return true
	}

	// Skip functions that are clearly trivial
	trivialPrefixes := []string{"String", "Error", "Len", "Less", "Swap", "MarshalJSON", "UnmarshalJSON"}
	for _, p := range trivialPrefixes {
		if name == p {
			return true
		}
	}

	return false
}

func bodyLineCount(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 0
	}
	return len(fn.Body.List)
}

func qualifiedName(fn *ast.FuncDecl, pkg string) string {
	name := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		// Method: extract receiver type
		recv := fn.Recv.List[0].Type
		var typeName string
		switch t := recv.(type) {
		case *ast.StarExpr:
			if ident, ok := t.X.(*ast.Ident); ok {
				typeName = ident.Name
			}
		case *ast.Ident:
			typeName = t.Name
		}
		if typeName != "" {
			return fmt.Sprintf("%s.(%s).%s", pkg, typeName, name)
		}
	}
	return fmt.Sprintf("%s.%s", pkg, name)
}

func sanitizeID(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}

func buildHeader(targets []*ast.FuncDecl, pkgName string, file *ast.File) string {
	var buf bytes.Buffer

	buf.WriteString("\n// --- AI Ops Agent instrumentation (auto-generated) ---\n")
	buf.WriteString(sentinel + "\n")
	buf.WriteString("var __aiops_p = probe.Global\n")

	for _, fn := range targets {
		safeName := sanitizeID(fn.Name.Name)
		qualName := qualifiedName(fn, pkgName)
		buf.WriteString(fmt.Sprintf(
			"var __aiops_id_%s = __aiops_p.RegisterFunc(%q, %q, 0, %q, 2)\n",
			safeName, qualName, "", pkgName,
		))
	}
	buf.WriteString("// --- end AI Ops Agent instrumentation ---\n\n")

	return buf.String()
}

func addImportAndVars(src []byte, header string, fset *token.FileSet, file *ast.File) []byte {
	// Find the end of the import block to insert our import
	var importEnd int
	if len(file.Imports) > 0 {
		last := file.Imports[len(file.Imports)-1]
		importEnd = fset.Position(last.End()).Offset
	} else {
		// No imports — insert after package clause
		importEnd = fset.Position(file.Name.End()).Offset
	}

	// Check if probe import already exists
	hasProbeImport := false
	for _, imp := range file.Imports {
		if imp.Path.Value == probeImport {
			hasProbeImport = true
			break
		}
	}

	var result []byte
	result = append(result, src[:importEnd]...)
	if !hasProbeImport {
		if len(file.Imports) > 0 {
			result = append(result, []byte("\n\t"+probeImport)...)
		} else {
			result = append(result, []byte("\n\nimport (\n\t"+probeImport+"\n)")...)
		}
	}
	result = append(result, src[importEnd:]...)

	// Find end of imports/package to insert var declarations
	// Insert header after the import block
	insertPoint := findInsertPoint(result)
	final := make([]byte, 0, len(result)+len(header))
	final = append(final, result[:insertPoint]...)
	final = append(final, []byte(header)...)
	final = append(final, result[insertPoint:]...)

	return final
}

func findInsertPoint(src []byte) int {
	// Find the closing paren of the last import block, or end of package line
	s := string(src)

	// Look for closing paren of import
	if idx := strings.LastIndex(s[:min(len(s), 2000)], ")"); idx != -1 {
		return idx + 1
	}

	// Fallback: after first newline (after package line)
	if idx := strings.Index(s, "\n"); idx != -1 {
		return idx + 1
	}

	return len(src)
}

func insertAt(src []byte, offset int, insertion []byte) []byte {
	result := make([]byte, len(src)+len(insertion))
	copy(result, src[:offset])
	copy(result[offset:], insertion)
	copy(result[offset+len(insertion):], src[offset:])
	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
