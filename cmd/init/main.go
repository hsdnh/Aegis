// Command init scans a project and generates a monitoring configuration.
//
// Usage:
//
//	ai-ops-agent init \
//	  --project mercari-hunter \
//	  --source /path/to/mercari-hunter \
//	  --redis 127.0.0.1:6379 \
//	  --mysql "user:pass@tcp(host:3306)/db" \
//	  --output config.yaml
//
// This performs:
//  1. Static code scan (AST analysis — routes, Redis keys, SQL, cron, logs)
//  2. Dynamic runtime probe (connect to Redis/MySQL, snapshot state)
//  3. AI analysis (send results to Claude, generate config + rules)
//  4. Write draft config.yaml with TODO markers for human review
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/ai"
	"github.com/hsdnh/ai-ops-agent/internal/scanner"
)

func main() {
	project := flag.String("project", "", "Project name")
	source := flag.String("source", ".", "Path to project source code")
	redisAddr := flag.String("redis", "", "Redis address (host:port)")
	redisPass := flag.String("redis-pass", "", "Redis password (or set REDIS_PASSWORD env)")
	mysqlDSN := flag.String("mysql", "", "MySQL DSN (or set MYSQL_DSN env)")
	output := flag.String("output", "config.yaml", "Output config file path")
	apiKey := flag.String("api-key", "", "Claude API key (or set CLAUDE_API_KEY env)")
	model := flag.String("model", "claude-sonnet-4-20250514", "AI model to use")
	scanOnly := flag.Bool("scan-only", false, "Only scan, skip AI generation")
	flag.Parse()

	if *project == "" {
		fmt.Fprintln(os.Stderr, "Error: --project is required")
		os.Exit(1)
	}

	// Resolve env vars
	if *redisPass == "" {
		*redisPass = os.Getenv("REDIS_PASSWORD")
	}
	if *mysqlDSN == "" {
		*mysqlDSN = os.Getenv("MYSQL_DSN")
	}
	if *apiKey == "" {
		*apiKey = os.Getenv("CLAUDE_API_KEY")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Step 1: Static code scan
	log.Printf("=== Step 1: Scanning project source code ===")
	log.Printf("Path: %s", *source)

	scanResult, err := scanner.ScanProject(*source)
	if err != nil {
		log.Fatalf("Scan failed: %v", err)
	}

	log.Printf("Scan complete: %d Go files", scanResult.GoFiles)
	log.Printf("  Routes:     %d", len(scanResult.Routes))
	log.Printf("  Redis keys: %d", len(scanResult.RedisKeys))
	log.Printf("  SQL tables: %d", len(scanResult.SQLTables))
	log.Printf("  Cron jobs:  %d", len(scanResult.CronJobs))
	log.Printf("  Log patterns: %d", len(scanResult.LogPatterns))
	log.Printf("  Config refs: %d", len(scanResult.ConfigRefs))

	// Print scan results
	fmt.Println("\n" + scanResult.FormatForAI())

	// Step 2: Dynamic runtime probe
	var probeResult *scanner.ProbeResult
	if *redisAddr != "" || *mysqlDSN != "" {
		log.Printf("\n=== Step 2: Probing runtime services ===")
		probeResult = scanner.ProbeRuntime(ctx, *redisAddr, *redisPass, *mysqlDSN)

		if probeResult.Redis != nil {
			if probeResult.Redis.Alive {
				log.Printf("  Redis: OK (v%s, %dMB mem, %d keys)",
					probeResult.Redis.Version, probeResult.Redis.UsedMemoryMB, probeResult.Redis.TotalKeys)
			} else {
				log.Printf("  Redis: FAILED to connect")
			}
		}
		if probeResult.MySQL != nil {
			if probeResult.MySQL.Alive {
				log.Printf("  MySQL: OK (v%s, %d connections, %d tables)",
					probeResult.MySQL.Version, probeResult.MySQL.Connections, len(probeResult.MySQL.Tables))
			} else {
				log.Printf("  MySQL: FAILED to connect")
			}
		}

		fmt.Println("\n" + probeResult.FormatForAI())
	} else {
		log.Printf("\n=== Step 2: Skipping runtime probe (no addresses provided) ===")
	}

	if *scanOnly {
		log.Printf("\n=== Scan-only mode, skipping AI generation ===")
		return
	}

	// Step 3: AI config generation
	if *apiKey == "" {
		log.Printf("\n=== Step 3: Skipping AI generation (no API key) ===")
		log.Printf("Set CLAUDE_API_KEY or use --api-key to enable AI config generation")
		log.Printf("You can use --scan-only to just view scan results")
		return
	}

	log.Printf("\n=== Step 3: Generating config with AI ===")
	client := ai.NewClient(ai.ProviderClaude, *apiKey, *model)

	configYAML, summary, err := scanner.GenerateConfig(ctx, client, scanResult, probeResult, *project)
	if err != nil {
		log.Fatalf("Config generation failed: %v", err)
	}

	// Step 4: Write config
	if configYAML != "" {
		if err := os.WriteFile(*output, []byte(configYAML), 0644); err != nil {
			log.Fatalf("Write config failed: %v", err)
		}
		log.Printf("\n=== Config written to %s ===", *output)
	}

	if summary != "" {
		fmt.Println("\n" + summary)
	}

	log.Printf("\n=== Done! ===")
	log.Printf("Review %s and adjust thresholds marked with TODO", *output)
	log.Printf("Then start monitoring: ai-ops-agent -config %s", *output)
}
