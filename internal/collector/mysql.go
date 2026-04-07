package collector

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
)

type MySQLCheck struct {
	Query     string  `yaml:"query"`
	Name      string  `yaml:"name"`
	Threshold float64 `yaml:"threshold"`
	Alert     string  `yaml:"alert"`
}

type MySQLCollectorConfig struct {
	DSN    string       `yaml:"dsn"`
	Checks []MySQLCheck `yaml:"checks"`
}

type MySQLCollector struct {
	cfg MySQLCollectorConfig
	db  *sql.DB
}

func NewMySQLCollector(cfg MySQLCollectorConfig) (*MySQLCollector, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("mysql open: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &MySQLCollector{cfg: cfg, db: db}, nil
}

func (m *MySQLCollector) Name() string { return "mysql" }
func (m *MySQLCollector) Close() error { return m.db.Close() }

func (m *MySQLCollector) Collect(ctx context.Context) (*types.CollectResult, error) {
	now := time.Now()
	result := &types.CollectResult{
		CollectorName: m.Name(),
		CollectedAt:   now,
	}

	// Connection alive check — MUST return result (not nil) even on failure
	// so that alive=0 reaches the rule engine for alerting
	if err := m.db.PingContext(ctx); err != nil {
		result.Metrics = append(result.Metrics, types.Metric{
			Name: "mysql.connection.alive", Value: 0, Timestamp: now, Source: "mysql",
		})
		result.Errors = append(result.Errors, fmt.Sprintf("mysql ping failed: %v", err))
		return result, nil // return partial result, NOT error
	}
	result.Metrics = append(result.Metrics, types.Metric{
		Name: "mysql.connection.alive", Value: 1, Timestamp: now, Source: "mysql",
	})

	// Global status metrics
	m.collectGlobalStatus(ctx, result, now)

	// Custom checks
	for _, check := range m.cfg.Checks {
		var value float64
		err := m.db.QueryRowContext(ctx, check.Query).Scan(&value)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("check %s: %v", check.Name, err))
			continue
		}
		name := check.Name
		if name == "" {
			name = "mysql.custom_check"
		}
		result.Metrics = append(result.Metrics, types.Metric{
			Name:      fmt.Sprintf("mysql.check.%s", name),
			Value:     value,
			Labels:    map[string]string{"query": check.Query},
			Timestamp: now,
			Source:    "mysql",
		})
	}

	return result, nil
}

func (m *MySQLCollector) collectGlobalStatus(ctx context.Context, result *types.CollectResult, now time.Time) {
	statusQueries := map[string]string{
		"mysql.threads.connected": "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_connected'",
		"mysql.threads.running":   "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_running'",
		"mysql.slow_queries":      "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Slow_queries'",
		"mysql.questions":         "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Questions'",
	}

	for metricName, query := range statusQueries {
		var valStr string
		err := m.db.QueryRowContext(ctx, query).Scan(&valStr)
		if err != nil {
			continue
		}
		var val float64
		fmt.Sscanf(valStr, "%f", &val)
		result.Metrics = append(result.Metrics, types.Metric{
			Name: metricName, Value: val, Timestamp: now, Source: "mysql",
		})
	}
}
