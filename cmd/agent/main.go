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

	"github.com/hsdnh/Aegis/internal/agent"
	"github.com/hsdnh/Aegis/internal/ai"
	"github.com/hsdnh/Aegis/internal/alert"
	"github.com/hsdnh/Aegis/internal/causal"
	"github.com/hsdnh/Aegis/internal/changefeed"
	"github.com/hsdnh/Aegis/internal/cluster"
	"github.com/hsdnh/Aegis/internal/collector"
	"github.com/hsdnh/Aegis/internal/config"
	"github.com/hsdnh/Aegis/internal/dashboard"
	"github.com/hsdnh/Aegis/internal/health"
	"github.com/hsdnh/Aegis/internal/healthcheck"
	"github.com/hsdnh/Aegis/internal/investigator"
	"github.com/hsdnh/Aegis/internal/issue"
	"github.com/hsdnh/Aegis/internal/rule"
	"github.com/hsdnh/Aegis/internal/runbook"
	"github.com/hsdnh/Aegis/internal/scanner"
	"github.com/hsdnh/Aegis/internal/storage"
	"github.com/hsdnh/Aegis/pkg/types"
	"github.com/robfig/cron/v3"
)

// Set via -ldflags at build time
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	once := flag.Bool("once", false, "运行一次后退出（测试模式）")
	dashAddr := flag.String("dashboard", "127.0.0.1:9090", "面板监听地址（留空禁用）")
	showVersion := flag.Bool("version", false, "显示版本信息")
	mode := flag.String("mode", "standalone", "运行模式: standalone(单机), worker(子节点), master(主节点)")
	masterURL := flag.String("master", "", "主节点地址，worker 模式必填（如 http://10.0.0.1:9090）")
	nodeName := flag.String("node", "", "节点名称（默认使用主机名）")
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

	// Preflight health check — validate config before starting
	preflightCfg := healthcheck.PreflightConfig{
		SQLitePath: cfg.Storage.Path,
	}
	for _, rc := range cfg.Collectors.Redis {
		preflightCfg.RedisAddrs = append(preflightCfg.RedisAddrs, rc.Addr)
		preflightCfg.RedisPassword = rc.Password
	}
	for _, mc := range cfg.Collectors.MySQL {
		preflightCfg.MySQLDSNs = append(preflightCfg.MySQLDSNs, mc.DSN)
	}
	for _, hc := range cfg.Collectors.HTTP {
		preflightCfg.HTTPURLs = append(preflightCfg.HTTPURLs, hc.URL)
	}
	for _, lc := range cfg.Collectors.Log {
		preflightCfg.LogSources = append(preflightCfg.LogSources, healthcheck.LogSourceCheck{
			Source: lc.Source, Unit: lc.Unit, Path: lc.FilePath, Container: lc.Container,
		})
	}

	report := healthcheck.RunPreflight(context.Background(), preflightCfg)
	fmt.Print(report.FormatReport())

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
	depMap := rule.NewDependencyMap()
	if len(cfg.Dependencies) > 0 {
		for _, d := range cfg.Dependencies {
			depMap.AddDependency(d.Root, d.Dependents)
		}
		log.Printf("Loaded %d dependency rules from config", len(cfg.Dependencies))
	} else {
		// Auto-generate from collector config
		for range cfg.Collectors.MySQL {
			depMap.AddDependency("mysql.connection.alive", []string{"mysql.check.pending_orders", "mysql.check.failed_orders"})
		}
		for range cfg.Collectors.Redis {
			depMap.AddDependency("redis.connection.alive", []string{"redis.memory.used_bytes", "redis.clients.connected"})
		}
		for range cfg.Collectors.HTTP {
			depMap.AddDependency("http.response.alive", []string{"http.response.latency_ms"})
		}
	}

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

	// Setup L2 AI analysis — supports multi-model routing
	var aiClient *ai.Client      // default client (for analyst)
	var chatClient *ai.Client    // for dashboard chat/terminal (may differ)
	var investClient *ai.Client  // for autonomous investigation (may differ)

	if cfg.AI.Enabled && cfg.AI.APIKey != "" {
		// Main client
		aiClient = ai.NewClientWithURL(ai.Provider(cfg.AI.Provider), cfg.AI.APIKey, cfg.AI.Model, cfg.AI.BaseURL)
		chatClient = aiClient    // default: same client
		investClient = aiClient  // default: same client

		// Per-task model overrides
		if m := cfg.AI.ChatModel; m != nil {
			chatClient = ai.NewClientWithURL(ai.Provider(m.Provider), m.APIKey, m.Model, m.BaseURL)
			log.Printf("Chat model: %s/%s", m.Provider, m.Model)
		}
		if m := cfg.AI.AnalystModel; m != nil {
			aiClient = ai.NewClientWithURL(ai.Provider(m.Provider), m.APIKey, m.Model, m.BaseURL)
			log.Printf("Analyst model: %s/%s", m.Provider, m.Model)
		}
		if m := cfg.AI.InvestModel; m != nil {
			investClient = ai.NewClientWithURL(ai.Provider(m.Provider), m.APIKey, m.Model, m.BaseURL)
			log.Printf("Investigation model: %s/%s", m.Provider, m.Model)
		}

		analyst := ai.NewAnalyst(aiClient, cfg.Project)
		if cfg.AI.SystemPrompt != "" {
			analyst.SetDomainKnowledge(cfg.AI.SystemPrompt)
		}
		a.SetAnalyst(analyst)
		log.Printf("AI enabled: %s/%s (base: %s)", cfg.AI.Provider, cfg.AI.Model, cfg.AI.BaseURL)
	}

	// Setup causal graph (from L0 scan if source_path configured)
	// Auto-detect source_path if not configured
	if cfg.SourcePath == "" {
		detected := healthcheck.DetectProjects()
		if len(detected) > 0 {
			cfg.SourcePath = detected[0].Path
			log.Printf("Auto-detected project: %s (%s) at %s", detected[0].Name, detected[0].Language, detected[0].Path)
		}
	}

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

	// Setup autonomous investigator (if AI enabled)
	if aiClient != nil {
		creds := investigator.NewCredentials()
		// Auto-load credentials from collector config
		if len(cfg.Collectors.MySQL) > 0 {
			creds.SetMySQL(cfg.Collectors.MySQL[0].DSN)
		}
		if len(cfg.Collectors.Redis) > 0 {
			creds.SetRedis(cfg.Collectors.Redis[0].Addr, cfg.Collectors.Redis[0].Password)
		}
		inv := investigator.New(investClient, creds)
		a.SetInvestigator(inv)
		log.Printf("Autonomous investigator enabled (auto-mode)")
	}

	// Setup cluster mode (must be before dashboard so routes can be registered)
	runMode := cluster.Mode(*mode)
	nName := *nodeName
	if nName == "" {
		nName = cfg.Project
	}
	nodeInfo := cluster.NewNodeInfo(nName, version, runMode)

	var masterReceiver *cluster.MasterReceiver
	var workerReporter *cluster.WorkerReporter
	var nodeRegistry *cluster.NodeRegistry

	if runMode == cluster.ModeWorker && *masterURL != "" {
		workerReporter = cluster.NewWorkerReporter(*masterURL, nodeInfo)
		log.Printf("Cluster mode: WORKER → reporting to %s", *masterURL)
	}
	if runMode == cluster.ModeMaster {
		nodeRegistry = cluster.NewNodeRegistry()
		masterReceiver = cluster.NewMasterReceiver(nodeRegistry)
		if *dashAddr == "" {
			go func() {
				if err := masterReceiver.Start(":19800"); err != nil {
					log.Printf("Cluster receiver error: %v", err)
				}
			}()
			log.Printf("Cluster mode: MASTER — listening on :19800")
		}
	}

	// Setup dashboard
	var store *dashboard.Store
	var eventLog *dashboard.EventLog
	if *dashAddr != "" {
		store = dashboard.NewStore(cfg.Project)
		eventLog = dashboard.NewEventLog(500)

		var chatSvc *dashboard.ChatService
		if aiClient != nil {
			chatSvc = dashboard.NewChatService(chatClient, store, eventLog)
		}

		mgr := dashboard.NewManageService(cfg.SourcePath, "./data", eventLog)
		mgr.SetAIInfo(dashboard.AIInfo{
			Provider: cfg.AI.Provider, Model: cfg.AI.Model,
			BaseURL: cfg.AI.BaseURL, Enabled: cfg.AI.Enabled,
		})
		dashToken := os.Getenv("AIOPS_DASHBOARD_TOKEN")
		srv := dashboard.NewServer(store, *dashAddr, eventLog, chatSvc, mgr, dashToken)
		if dashToken != "" {
			log.Printf("Dashboard auth enabled (token required)")
		}

		// Register AI terminal if AI configured
		if aiClient != nil {
			terminal := dashboard.NewAITerminal(chatClient, store, eventLog)
			srv.SetAITerminal(terminal)
		}

		// Register causal graph API if available
		if a.CausalGraph() != nil {
			srv.RegisterCausalAPI(causal.NewGraphAPI(a.CausalGraph()))
		}

		// Register cluster routes on dashboard mux (master mode)
		if masterReceiver != nil {
			masterReceiver.RegisterRoutes(srv.Mux(), srv.APIWrap())
			log.Printf("Cluster mode: MASTER — accepting worker reports on dashboard port")
		}

		// Register ALL extra API routes (silence, runbook, investigate, probes, etc.)
		silenceMgr := issue.NewSilenceManager()
		rbRegistry := runbook.NewRegistry()
		for _, rb := range runbook.DefaultRunbooks() {
			rbRegistry.Add(rb)
		}
		changeFeed := changefeed.NewFeed(200)
		if cfg.SourcePath != "" {
			changeFeed.AddWatcher(changefeed.WatchConfig{Type: changefeed.ChangeDeploy, GitRepo: cfg.SourcePath})
		}

		extras := dashboard.ExtraServices{
			SilenceManager:  silenceMgr,
			RunbookRegistry: rbRegistry,
			ChangeFeed:      changeFeed,
			PreflightReport: report,
		}
		if aiClient != nil {
			extras.Investigator = investigator.New(investClient, func() *investigator.Credentials {
				c := investigator.NewCredentials()
				if len(cfg.Collectors.MySQL) > 0 { c.SetMySQL(cfg.Collectors.MySQL[0].DSN) }
				if len(cfg.Collectors.Redis) > 0 { c.SetRedis(cfg.Collectors.Redis[0].Addr, cfg.Collectors.Redis[0].Password) }
				return c
			}())
		}
		srv.RegisterExtraRoutes(extras)

		// Wire subsystems into agent
		a.SetSilenceManager(silenceMgr)
		a.SetChangeFeed(changeFeed)
		a.SetIncidentAggregator(issue.NewIncidentAggregator(issueTracker))

		// Register AI model switcher
		aiCfgMgr := dashboard.NewAIConfigManager(cfg.AI.Provider, cfg.AI.Model, cfg.AI.BaseURL, cfg.AI.APIKey, cfg.AI.Enabled)
		aiCfgMgr.SetSwitchCallback(func(newClient *ai.Client) {
			analyst := ai.NewAnalyst(newClient, cfg.Project)
			if cfg.AI.SystemPrompt != "" {
				analyst.SetDomainKnowledge(cfg.AI.SystemPrompt)
			}
			a.SetAnalyst(analyst)
			log.Printf("AI model hot-switched via dashboard")
		})
		aiCfgMgr.RegisterRoutes(srv.Mux(), srv.APIWrap())

		// Register config editor
		cfgEditor := dashboard.NewConfigEditor(*configPath, nil) // nil = restart required
		cfgEditor.RegisterRoutes(srv.Mux(), srv.APIWrap())

		// Register backup/restore
		backupMgr := dashboard.NewBackupManager("")
		backupMgr.RegisterRoutes(srv.Mux(), srv.APIWrap(), func() string { return cfg.SourcePath })

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
		// Worker: report to master
		if workerReporter != nil {
			report := workerReporter.BuildReport(snapshot, issueTracker.OpenIssues(), health.CheckSelf(ctx))
			if err := workerReporter.Send(report); err != nil {
				log.Printf("Worker report failed: %v", err)
			}
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
		// Worker: report to master after each cycle
		if workerReporter != nil {
			report := workerReporter.BuildReport(snapshot, issueTracker.OpenIssues(), health.CheckSelf(ctx))
			if err := workerReporter.Send(report); err != nil {
				log.Printf("Worker report failed: %v", err)
			}
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
