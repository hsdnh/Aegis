package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hsdnh/ai-ops-agent/internal/agent"
	"github.com/hsdnh/ai-ops-agent/internal/ai"
	"github.com/hsdnh/ai-ops-agent/internal/alert"
	"github.com/hsdnh/ai-ops-agent/internal/causal"
	"github.com/hsdnh/ai-ops-agent/internal/collector"
	"github.com/hsdnh/ai-ops-agent/internal/config"
	"github.com/hsdnh/ai-ops-agent/internal/dashboard"
	"github.com/hsdnh/ai-ops-agent/internal/issue"
	"github.com/hsdnh/ai-ops-agent/internal/rule"
	"github.com/hsdnh/ai-ops-agent/internal/scanner"
	"github.com/hsdnh/ai-ops-agent/internal/storage"
	"github.com/hsdnh/ai-ops-agent/pkg/types"
	"github.com/robfig/cron/v3"
)

// Set via -ldflags at build time
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	once := flag.Bool("once", false, "Run one cycle and exit")
	dashAddr := flag.String("dashboard", "127.0.0.1:9090", "Dashboard listen address (empty to disable)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("ai-ops-agent %s (commit: %s, built: %s)\n", version, commit, buildDate)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	log.Printf("AI Ops Agent starting for project: %s", cfg.Project)

	// Build collectors
	collectorRegistry := collector.NewRegistry()
	registerCollectors(collectorRegistry, cfg)

	// Build rules
	var rules []rule.Rule
	for _, rc := range cfg.Rules {
		rules = append(rules, rule.Rule{
			Name:        rc.Name,
			MetricName:  rc.MetricName,
			Operator:    rule.Operator(rc.Operator),
			Threshold:   rc.Threshold,
			SeverityStr: rc.Severity,
			Message:     rc.Message,
			Labels:      rc.Labels,
		})
	}
	ruleEngine := rule.NewEngine(rules)

	// Build dependency map for alert suppression
	depMap := rule.DefaultMercariHunterDeps() // TODO: load from config for other projects

	// Build alerters
	alertMgr := alert.NewManager()
	registerAlerters(alertMgr, cfg)

	// Build issue tracker
	issueTracker := issue.NewTracker()

	// Create agent
	a := agent.New(cfg.Project, collectorRegistry, ruleEngine, depMap, alertMgr, issueTracker)

	// Setup SQLite persistence + baseline learning
	if cfg.Storage.Enabled {
		dbPath := cfg.Storage.Path
		if dbPath == "" {
			dbPath = "./data/aiops.db"
		}
		os.MkdirAll("./data", 0755)
		db, err := storage.Open(dbPath)
		if err != nil {
			log.Printf("WARNING: SQLite init failed: %v (running in memory-only mode)", err)
		} else {
			a.SetStorage(db)
			// Load persisted issues on startup — restore into tracker so lifecycle continues
			if savedIssues, err := db.LoadOpenIssues(); err == nil && len(savedIssues) > 0 {
				issueTracker.RestoreIssues(savedIssues)
				log.Printf("Restored %d issues from database", len(savedIssues))
			}
			log.Printf("SQLite persistence enabled: %s", dbPath)
		}
	}

	// Setup L2 AI analysis
	var aiClient *ai.Client
	if cfg.AI.Enabled && cfg.AI.APIKey != "" {
		aiClient = ai.NewClient(ai.Provider(cfg.AI.Provider), cfg.AI.APIKey, cfg.AI.Model)
		analyst := ai.NewAnalyst(aiClient, cfg.Project)
		if cfg.AI.SystemPrompt != "" {
			analyst.SetDomainKnowledge(cfg.AI.SystemPrompt)
		}
		a.SetAnalyst(analyst)
		log.Printf("L2 AI analysis enabled (provider: %s, model: %s)", cfg.AI.Provider, cfg.AI.Model)
	}

	// Setup causal graph (from L0 scan if source_path configured)
	if cfg.SourcePath != "" {
		scanResult, err := scanner.ScanProject(cfg.SourcePath)
		if err == nil {
			cg := causal.NewGraph()
			cg.BuildFromScan(scanResult)
			a.SetCausalGraph(cg)
			a.SetCodeContext(scanResult.FormatForAI())
			log.Printf("Causal graph built: %d nodes, %d edges (from %s)",
				cg.NodeCount(), cg.EdgeCount(), cfg.SourcePath)
		} else {
			log.Printf("WARNING: L0 scan failed: %v", err)
		}
	}

	// Setup shadow verification — connect to first MySQL from collectors config
	if len(cfg.Shadow) > 0 {
		var shadowDB *sql.DB
		if len(cfg.Collectors.MySQL) > 0 && cfg.Collectors.MySQL[0].DSN != "" {
			shadowDB, _ = sql.Open("mysql", cfg.Collectors.MySQL[0].DSN)
		}
		sv := causal.NewShadowVerifier(shadowDB, nil, 200)
		for _, sc := range cfg.Shadow {
			sv.AddCheck(causal.ShadowCheck{
				Name: sc.Name, Type: sc.Type, Description: sc.Description,
				Severity: sc.Severity, Query: sc.Query, Expect: sc.Expect,
				SourceQuery: sc.SourceQuery, TargetQuery: sc.TargetQuery,
				CompareMode: sc.CompareMode, Enabled: true,
			})
		}
		a.SetShadowVerifier(sv)
		log.Printf("Shadow verification enabled: %d checks", len(cfg.Shadow))
	}

	// Setup synthetic probes
	if len(cfg.Probes) > 0 {
		sr := causal.NewSyntheticRunner()
		for _, pc := range cfg.Probes {
			sr.AddProbe(causal.SyntheticProbe{
				Name: pc.Name, Type: pc.Type, Enabled: true,
				Config: causal.ProbeConfig{
					URL: pc.URL, Method: pc.Method,
					ExpectStatus: pc.ExpectStatus, ExpectBody: pc.ExpectBody,
					RedisAddr: pc.RedisAddr, TimeoutSec: pc.TimeoutSec,
				},
			})
		}
		a.SetSyntheticRunner(sr)
		log.Printf("Synthetic probes enabled: %d probes", len(cfg.Probes))
	}

	// Setup expectation model
	a.SetExpectationModel(causal.NewExpectationModel(50))

	// Setup dashboard
	var store *dashboard.Store
	var eventLog *dashboard.EventLog
	if *dashAddr != "" {
		store = dashboard.NewStore(cfg.Project)
		eventLog = dashboard.NewEventLog(500)

		var chatSvc *dashboard.ChatService
		if aiClient != nil {
			chatSvc = dashboard.NewChatService(aiClient, store, eventLog)
		}

		mgr := dashboard.NewManageService(cfg.SourcePath, "./data", eventLog)
		dashToken := os.Getenv("AIOPS_DASHBOARD_TOKEN")
		srv := dashboard.NewServer(store, *dashAddr, eventLog, chatSvc, mgr, dashToken)
		if dashToken != "" {
			log.Printf("Dashboard auth enabled (token required)")
		}

		// Register AI terminal if AI configured
		if aiClient != nil {
			terminal := dashboard.NewAITerminal(aiClient, store, eventLog)
			srv.SetAITerminal(terminal)
		}

		// Register causal graph API if available
		if a.CausalGraph() != nil {
			srv.RegisterCausalAPI(causal.NewGraphAPI(a.CausalGraph()))
		}

		go func() {
			if err := srv.Start(); err != nil {
				log.Printf("Dashboard server error: %v", err)
			}
		}()

		a.SetDashboard(store, eventLog)
	}

	if *once {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		snapshot, err := a.RunOnce(ctx)
		if err != nil {
			log.Fatalf("Cycle failed: %v", err)
		}
		log.Printf("Cycle %s: %d metrics, %d alerts",
			snapshot.CycleID, countMetrics(snapshot), len(snapshot.Alerts))
		return
	}

	// Run on cron schedule
	c := cron.New()
	_, err = c.AddFunc(cfg.Schedule, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		snapshot, err := a.RunOnce(ctx)
		if err != nil {
			log.Printf("Cycle failed: %v", err)
			return
		}
		log.Printf("Cycle %s: %d metrics, %d alerts",
			snapshot.CycleID, countMetrics(snapshot), len(snapshot.Alerts))
	})
	if err != nil {
		log.Fatalf("Invalid cron schedule %q: %v", cfg.Schedule, err)
	}

	c.Start()
	log.Printf("Monitoring loop started with schedule: %s", cfg.Schedule)
	if *dashAddr != "" {
		log.Printf("Dashboard available at http://localhost%s", *dashAddr)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)
	c.Stop()
	a.Shutdown()
	log.Printf("Goodbye.")
}

func registerCollectors(registry *collector.Registry, cfg *config.Config) {
	for _, rc := range cfg.Collectors.Redis {
		registry.Register(collector.NewRedisCollector(collector.RedisCollectorConfig{
			Addr:     rc.Addr,
			Password: rc.Password,
			DB:       rc.DB,
			Checks:   toRedisChecks(rc.Checks),
		}))
	}

	for _, mc := range cfg.Collectors.MySQL {
		mysqlCollector, err := collector.NewMySQLCollector(collector.MySQLCollectorConfig{
			DSN:    mc.DSN,
			Checks: toMySQLChecks(mc.Checks),
		})
		if err != nil {
			log.Printf("WARNING: MySQL collector init failed: %v", err)
			continue
		}
		registry.Register(mysqlCollector)
	}

	for _, hc := range cfg.Collectors.HTTP {
		registry.Register(collector.NewHTTPCollector(collector.HTTPCollectorConfig{
			URL:     hc.URL,
			Auth:    hc.Auth,
			Timeout: hc.Timeout,
			Checks:  toHTTPChecks(hc.Checks),
		}))
	}

	for _, lc := range cfg.Collectors.Log {
		registry.Register(collector.NewLogCollector(collector.LogCollectorConfig{
			Source:        lc.Source,
			Unit:          lc.Unit,
			FilePath:      lc.FilePath,
			Container:     lc.Container,
			ErrorPatterns: lc.ErrorPatterns,
			Minutes:       lc.Minutes,
		}))
	}

	log.Printf("Registered %d collectors", len(registry.All()))
}

func registerAlerters(mgr *alert.Manager, cfg *config.Config) {
	// Always add console alerter
	if cfg.Alerts.Console != nil {
		mgr.Register(alert.NewConsoleAlerter())
	}

	if cfg.Alerts.Bark != nil {
		mgr.Register(alert.NewBarkAlerter(alert.BarkConfig{
			ServerURL: cfg.Alerts.Bark.ServerURL,
			Keys:      cfg.Alerts.Bark.Keys,
		}))
	}

	if cfg.Alerts.Telegram != nil {
		mgr.Register(alert.NewTelegramAlerter(alert.TelegramConfig{
			Token:   cfg.Alerts.Telegram.Token,
			ChatIDs: cfg.Alerts.Telegram.ChatIDs,
		}))
	}
}

func countMetrics(s *types.Snapshot) int {
	count := 0
	for _, r := range s.Results {
		count += len(r.Metrics)
	}
	return count
}

// Type conversion helpers
func toRedisChecks(in []struct {
	KeyPattern string  `yaml:"key_pattern"`
	Threshold  float64 `yaml:"threshold"`
	Alert      string  `yaml:"alert"`
}) []collector.RedisCheck {
	var out []collector.RedisCheck
	for _, c := range in {
		out = append(out, collector.RedisCheck{
			KeyPattern: c.KeyPattern,
			Threshold:  c.Threshold,
			Alert:      c.Alert,
		})
	}
	return out
}

func toMySQLChecks(in []struct {
	Query     string  `yaml:"query"`
	Name      string  `yaml:"name"`
	Threshold float64 `yaml:"threshold"`
	Alert     string  `yaml:"alert"`
}) []collector.MySQLCheck {
	var out []collector.MySQLCheck
	for _, c := range in {
		out = append(out, collector.MySQLCheck{
			Query:     c.Query,
			Name:      c.Name,
			Threshold: c.Threshold,
			Alert:     c.Alert,
		})
	}
	return out
}

func toHTTPChecks(in []struct {
	JSONPath  string  `yaml:"json_path"`
	Name      string  `yaml:"name"`
	Threshold float64 `yaml:"threshold"`
	Alert     string  `yaml:"alert"`
}) []collector.HTTPCheck {
	var out []collector.HTTPCheck
	for _, c := range in {
		out = append(out, collector.HTTPCheck{
			JSONPath:  c.JSONPath,
			Name:      c.Name,
			Threshold: c.Threshold,
			Alert:     c.Alert,
		})
	}
	return out
}
