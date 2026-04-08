package rewriter

import (
	"fmt"
	"go/format"
	"log"
	"os"
	"strings"
)

// StripFile removes all AI Ops Agent instrumentation from a Go source file.
// This is the "uninstall" operation — restores the file to its original state.
// Idempotent: running on a non-instrumented file is a no-op.
func StripFile(path string, cfg Config) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	original := string(src)

	// Check if file has any instrumentation
	if !strings.Contains(original, "__aiops") {
		if cfg.Verbose {
			log.Printf("SKIP (not instrumented): %s", path)
		}
		return nil
	}

	lines := strings.Split(original, "\n")
	var cleaned []string
	inHeaderBlock := false
	removedCount := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip the auto-generated header block
		if trimmed == "// --- AI Ops Agent instrumentation (auto-generated) ---" {
			inHeaderBlock = true
			removedCount++
			continue
		}
		if inHeaderBlock {
			if trimmed == "// --- end AI Ops Agent instrumentation ---" {
				inHeaderBlock = false
				removedCount++
				continue
			}
			removedCount++
			continue
		}

		// Skip sentinel comments inside functions
		if trimmed == sentinel {
			removedCount++
			continue
		}

		// Skip the probe check line injected into function bodies
		if strings.Contains(trimmed, "__aiops_p.Active()") &&
			strings.Contains(trimmed, "__aiops_p.Enter(") {
			removedCount++
			continue
		}

		// Skip var declarations for probe IDs
		if strings.HasPrefix(trimmed, "var __aiops_") {
			removedCount++
			continue
		}

		// Skip the probe import line
		if trimmed == `"github.com/hsdnh/Aegis/sdk/probe"` {
			removedCount++
			continue
		}

		cleaned = append(cleaned, line)
	}

	if removedCount == 0 {
		if cfg.Verbose {
			log.Printf("SKIP (no probe lines found): %s", path)
		}
		return nil
	}

	// Remove empty import blocks left after stripping
	result := strings.Join(cleaned, "\n")
	result = cleanEmptyImports(result)

	if cfg.DryRun {
		fmt.Printf("[DRY RUN] %s: would remove %d instrumented lines\n", path, removedCount)
		return nil
	}

	// Format the cleaned code
	formatted, err := format.Source([]byte(result))
	if err != nil {
		// Write unformatted if gofmt fails
		log.Printf("WARNING: gofmt failed for %s after stripping, writing unformatted: %v", path, err)
		formatted = []byte(result)
	}

	if err := os.WriteFile(path, formatted, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	log.Printf("STRIPPED: %s (%d lines removed)", path, removedCount)
	return nil
}

// cleanEmptyImports removes import blocks that became empty after stripping.
// e.g., import (\n\n) → removed entirely
func cleanEmptyImports(src string) string {
	// Remove empty import groups: import (\n) or import (\n\n)
	for {
		// Find import ( ... ) with only whitespace inside
		idx := strings.Index(src, "import (")
		if idx == -1 {
			break
		}
		end := strings.Index(src[idx:], ")")
		if end == -1 {
			break
		}
		end += idx

		// Check if the import block is empty (only whitespace/newlines between parens)
		inside := strings.TrimSpace(src[idx+len("import (") : end])
		if inside == "" {
			// Remove the entire empty import block
			src = src[:idx] + src[end+1:]
			continue
		}
		break // non-empty import, stop
	}
	return src
}
