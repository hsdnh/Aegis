package collector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hsdnh/ai-ops-agent/pkg/types"
	"github.com/redis/go-redis/v9"
)

type RedisCheck struct {
	KeyPattern string  `yaml:"key_pattern"`
	Threshold  float64 `yaml:"threshold"`
	Alert      string  `yaml:"alert"`
}

type RedisCollectorConfig struct {
	Addr     string       `yaml:"addr"`
	Password string       `yaml:"password"`
	DB       int          `yaml:"db"`
	Checks   []RedisCheck `yaml:"checks"`
}

type RedisCollector struct {
	cfg    RedisCollectorConfig
	client *redis.Client
}

func NewRedisCollector(cfg RedisCollectorConfig) *RedisCollector {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	return &RedisCollector{cfg: cfg, client: client}
}

func (r *RedisCollector) Name() string { return "redis" }
func (r *RedisCollector) Close() error { return r.client.Close() }

func (r *RedisCollector) Collect(ctx context.Context) (*types.CollectResult, error) {
	now := time.Now()
	result := &types.CollectResult{
		CollectorName: r.Name(),
		CollectedAt:   now,
	}

	// Basic info
	info, err := r.client.Info(ctx, "memory", "clients", "stats").Result()
	if err != nil {
		return nil, fmt.Errorf("redis INFO failed: %w", err)
	}
	r.parseInfo(info, result)

	// Check key patterns (using SCAN instead of KEYS to avoid blocking Redis)
	for _, check := range r.cfg.Checks {
		var keys []string
		var cursor uint64
		for {
			var batch []string
			var err error
			batch, cursor, err = r.client.Scan(ctx, cursor, check.KeyPattern, 100).Result()
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("scan %s: %v", check.KeyPattern, err))
				break
			}
			keys = append(keys, batch...)
			if cursor == 0 || len(keys) >= 10000 { // safety cap
				break
			}
		}

		var totalLen int64
		for _, key := range keys {
			keyType, _ := r.client.Type(ctx, key).Result()
			var length int64
			switch keyType {
			case "list":
				length, _ = r.client.LLen(ctx, key).Result()
			case "set":
				length, _ = r.client.SCard(ctx, key).Result()
			case "zset":
				length, _ = r.client.ZCard(ctx, key).Result()
			case "hash":
				length, _ = r.client.HLen(ctx, key).Result()
			case "string":
				length = 1
			}
			totalLen += length
		}

		result.Metrics = append(result.Metrics, types.Metric{
			Name:      fmt.Sprintf("redis.keys.%s.total_length", sanitizeMetricName(check.KeyPattern)),
			Value:     float64(totalLen),
			Labels:    map[string]string{"pattern": check.KeyPattern},
			Timestamp: now,
			Source:    "redis",
		})
	}

	// Connection test (readonly check - security)
	_, err = r.client.Ping(ctx).Result()
	if err != nil {
		result.Metrics = append(result.Metrics, types.Metric{
			Name: "redis.connection.alive", Value: 0, Timestamp: now, Source: "redis",
		})
	} else {
		result.Metrics = append(result.Metrics, types.Metric{
			Name: "redis.connection.alive", Value: 1, Timestamp: now, Source: "redis",
		})
	}

	// Check if Redis is in readonly mode (security indicator)
	configGet, err := r.client.ConfigGet(ctx, "slave-read-only").Result()
	if err == nil && len(configGet) >= 2 {
		if val, ok := configGet["slave-read-only"]; ok && val == "yes" {
			result.Logs = append(result.Logs, types.LogEntry{
				Timestamp: now, Level: "WARNING",
				Message: "Redis slave-read-only is enabled - check for unauthorized config changes",
				Source:  "redis",
			})
		}
	}

	return result, nil
}

func (r *RedisCollector) parseInfo(info string, result *types.CollectResult) {
	now := time.Now()
	lines := strings.Split(info, "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]

		switch key {
		case "used_memory":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				result.Metrics = append(result.Metrics, types.Metric{
					Name: "redis.memory.used_bytes", Value: v, Timestamp: now, Source: "redis",
				})
			}
		case "connected_clients":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				result.Metrics = append(result.Metrics, types.Metric{
					Name: "redis.clients.connected", Value: v, Timestamp: now, Source: "redis",
				})
			}
		case "maxmemory":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				result.Metrics = append(result.Metrics, types.Metric{
					Name: "redis.memory.max_bytes", Value: v, Timestamp: now, Source: "redis",
				})
			}
		case "rejected_connections":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				result.Metrics = append(result.Metrics, types.Metric{
					Name: "redis.connections.rejected", Value: v, Timestamp: now, Source: "redis",
				})
			}
		}
	}
}

func sanitizeMetricName(s string) string {
	r := strings.NewReplacer("*", "all", ":", "_", ".", "_")
	return r.Replace(s)
}
