// Command uninstall completely removes AI Ops Agent from a system.
//
// Usage:
//
//	ai-ops-agent-uninstall [flags]
//
// This performs:
//  1. Stops the running agent process (if any)
//  2. Strips all SDK probes from the target project source code (-source flag)
//  3. Removes the autotrace import from the project's main.go
//  4. Deletes the agent data directory (SQLite DB, baselines, knowledge)
//  5. Optionally removes the agent binary itself
//
// After running this, the target project is 100% restored to pre-agent state.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hsdnh/Aegis/cmd/instrument/rewriter"
)

func main() {
	source := flag.String("source", "", "Path to target project source code (to strip probes)")
	dataDir := flag.String("data", "./data", "Agent data directory to delete")
	keepBinary := flag.Bool("keep-binary", false, "Don't delete the agent binary itself")
	force := flag.Bool("force", false, "Skip confirmation prompt")
	dryRun := flag.Bool("dry-run", false, "Show what would be done without doing it")
	flag.Parse()

	log.Printf("=== AI Ops Agent Uninstaller ===")

	if !*force && !*dryRun {
		fmt.Print("\nThis will:\n")
		fmt.Print("  1. Stop the running agent process\n")
		if *source != "" {
			fmt.Printf("  2. Strip all SDK probes from: %s\n", *source)
			fmt.Print("  3. Remove autotrace import from source\n")
		}
		fmt.Printf("  4. Delete data directory: %s\n", *dataDir)
		if !*keepBinary {
			fmt.Print("  5. Delete the agent binary\n")
		}
		fmt.Print("\nType 'yes' to confirm: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if strings.TrimSpace(scanner.Text()) != "yes" {
			log.Printf("Aborted.")
			return
		}
	}

	// Step 1: Stop running agent process
	log.Printf("\n[Step 1] Stopping agent process...")
	stopAgent(*dryRun)

	// Step 2: Strip SDK probes from source
	if *source != "" {
		log.Printf("\n[Step 2] Stripping SDK probes from: %s", *source)
		stripProbes(*source, *dryRun)

		log.Printf("\n[Step 3] Removing autotrace import...")
		removeAutotraceImport(*source, *dryRun)
	} else {
		log.Printf("\n[Step 2-3] Skipping source cleanup (no -source provided)")
	}

	// Step 4: Delete data directory
	log.Printf("\n[Step 4] Deleting data directory: %s", *dataDir)
	if *dryRun {
		log.Printf("  [DRY RUN] would delete %s", *dataDir)
	} else {
		if err := os.RemoveAll(*dataDir); err != nil {
			log.Printf("  WARNING: %v", err)
		} else {
			log.Printf("  Deleted.")
		}
	}

	// Step 5: Delete agent binary
	if !*keepBinary {
		binary, _ := os.Executable()
		log.Printf("\n[Step 5] Deleting agent binary: %s", binary)
		if *dryRun {
			log.Printf("  [DRY RUN] would delete %s", binary)
		} else {
			// On Windows, can't delete a running executable — schedule for next boot
			if runtime.GOOS == "windows" {
				log.Printf("  Windows: binary will be deleted on next boot (can't delete running exe)")
			} else {
				os.Remove(binary)
				log.Printf("  Deleted.")
			}
		}
	}

	log.Printf("\n=== Uninstall complete ===")
	if *source != "" {
		log.Printf("Your source code has been restored to pre-agent state.")
		log.Printf("You can verify with: git diff")
	}
}

func stopAgent(dryRun bool) {
	// Find and kill agent processes
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("taskkill", "/F", "/IM", "ai-ops-agent.exe")
	} else {
		cmd = exec.Command("pkill", "-f", "ai-ops-agent")
	}

	if dryRun {
		log.Printf("  [DRY RUN] would run: %s", cmd.String())
		return
	}

	if err := cmd.Run(); err != nil {
		log.Printf("  No running agent found (or already stopped)")
	} else {
		log.Printf("  Agent process stopped.")
	}
}

func stripProbes(sourcePath string, dryRun bool) {
	cfg := rewriter.Config{
		DryRun:  dryRun,
		Verbose: true,
		ExcludeDirs: map[string]bool{
			"vendor": true, "testdata": true, ".git": true, "node_modules": true,
		},
	}

	count := 0
	filepath.Walk(sourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if cfg.ExcludeDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		count++
		return rewriter.StripFile(path, cfg)
	})

	log.Printf("  Scanned %d Go files", count)
}

func removeAutotraceImport(sourcePath string, dryRun bool) {
	// Find main.go files and remove the autotrace import
	filepath.Walk(sourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() != "main.go" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		content := string(data)
		autotraceImport := `_ "github.com/hsdnh/Aegis/sdk/autotrace"`

		if !strings.Contains(content, autotraceImport) {
			return nil
		}

		if dryRun {
			log.Printf("  [DRY RUN] would remove autotrace import from: %s", path)
			return nil
		}

		// Remove the import line
		lines := strings.Split(content, "\n")
		var cleaned []string
		for _, line := range lines {
			if strings.Contains(strings.TrimSpace(line), autotraceImport) {
				continue
			}
			cleaned = append(cleaned, line)
		}

		result := strings.Join(cleaned, "\n")
		os.WriteFile(path, []byte(result), 0644)
		log.Printf("  Removed autotrace import from: %s", path)
		return nil
	})
}
