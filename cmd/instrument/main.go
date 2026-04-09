// Command instrument manages AI Ops Agent trace probes in Go source files.
//
// Usage:
//
//	# Install probes (add tracing to all eligible functions)
//	ai-ops-agent-instrument [flags] ./path/to/project/...
//
//	# Uninstall probes (remove all tracing, restore original code)
//	ai-ops-agent-instrument -strip [flags] ./path/to/project/...
//
// Install is idempotent — running it twice produces the same result.
// Strip removes every line injected by install, leaving clean source code.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hsdnh/Aegis/cmd/instrument/rewriter"
)

func main() {
	strip := flag.Bool("strip", false, "Remove all instrumentation (uninstall)")
	dryRun := flag.Bool("dry-run", false, "Print changes without writing files")
	verbose := flag.Bool("v", false, "Verbose output")
	exclude := flag.String("exclude", "vendor,testdata,.git", "Comma-separated dirs to skip")
	minLines := flag.Int("min-lines", 3, "Only instrument functions with at least N lines")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  Install:   ai-ops-agent-instrument ./path/to/project/...")
		fmt.Fprintln(os.Stderr, "  Uninstall: ai-ops-agent-instrument -strip ./path/to/project/...")
		os.Exit(1)
	}

	excludeDirs := make(map[string]bool)
	for _, d := range strings.Split(*exclude, ",") {
		excludeDirs[strings.TrimSpace(d)] = true
	}

	cfg := rewriter.Config{
		DryRun:      *dryRun,
		Verbose:     *verbose,
		ExcludeDirs: excludeDirs,
		MinLines:    *minLines,
	}

	mode := "INSTRUMENT"
	if *strip {
		mode = "STRIP"
	}
	log.Printf("=== %s mode ===", mode)

	total := 0
	for _, pattern := range flag.Args() {
		root := strings.TrimSuffix(pattern, "/...")
		if root == "" {
			root = "."
		}

		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if info.IsDir() {
				if excludeDirs[info.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			total++
			if *strip {
				return rewriter.StripFile(path, cfg)
			}
			return rewriter.ProcessFile(path, cfg)
		})

		if err != nil {
			log.Fatalf("Error processing %s: %v", pattern, err)
		}
	}

	log.Printf("=== Done. Scanned %d files ===", total)

	// After instrumenting, try to add SDK dependency to go.mod
	if !*strip && total > 0 {
		root := strings.TrimSuffix(flag.Args()[0], "/...")
		if root == "" {
			root = "."
		}
		goModPath := findGoMod(root)
		if goModPath != "" {
			log.Printf("Adding SDK dependency to %s...", goModPath)
			cmd := exec.Command("go", "get", "github.com/hsdnh/Aegis/sdk/probe@latest")
			cmd.Dir = filepath.Dir(goModPath)
			cmd.Env = append(os.Environ(),
				"GOPRIVATE=github.com/hsdnh/Aegis",
				"GONOSUMCHECK=github.com/hsdnh/Aegis",
				"GONOSUMDB=github.com/hsdnh/Aegis",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Printf("WARNING: could not add SDK dep: %v\n%s", err, string(out))
				log.Printf("Run manually: cd %s && go get github.com/hsdnh/Aegis/sdk/probe", filepath.Dir(goModPath))
			} else {
				log.Printf("SDK dependency added successfully")
			}
		}
	}
}

func findGoMod(dir string) string {
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		p := filepath.Join(d, "go.mod")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
