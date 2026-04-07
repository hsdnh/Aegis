package scanner

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// ProbeResult holds the runtime state of all discovered services.
type ProbeResult struct {
	Redis  *RedisProbeResult  `json:"redis,omitempty"`
	MySQL  *MySQLProbeResult  `json:"mysql,omitempty"`
	Probed time.Time          `json:"probed_at"`
}

type RedisProbeResult struct {
	Alive       bool           `json:"alive"`
	Version     string         `json:"version"`
	UsedMemoryMB int           `json:"used_memory_mb"`
	MaxMemoryMB  int           `json:"max_memory_mb"`
	ConnectedClients int       `json:"connected_clients"`
	Keys        []RedisKeyInfo `json:"keys"`
	TotalKeys   int            `json:"total_keys"`
}

type RedisKeyInfo struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	Length int64  `json:"length"`
	TTL    int64  `json:"ttl_seconds"` // -1 = no expiry, -2 = not found
}

type MySQLProbeResult struct {
	Alive       bool            `json:"alive"`
	Version     string          `json:"version"`
	Tables      []MySQLTableInfo `json:"tables"`
	Connections int             `json:"connections"`
	MaxConn     int             `json:"max_connections"`
}

type MySQLTableInfo struct {
	Name      string `json:"name"`
	Rows      int64  `json:"rows"`
	SizeMB    float64 `json:"size_mb"`
	IndexSizeMB float64 `json:"index_size_mb"`
}

// ProbeRuntime connects to discovered services and snapshots their current state.
func ProbeRuntime(ctx context.Context, redisAddr, redisPass, mysqlDSN string) *ProbeResult {
	result := &ProbeResult{Probed: time.Now()}

	if redisAddr != "" {
		result.Redis = probeRedis(ctx, redisAddr, redisPass)
	}
	if mysqlDSN != "" {
		result.MySQL = probeMySQL(ctx, mysqlDSN)
	}

	return result
}

func probeRedis(ctx context.Context, addr, password string) *RedisProbeResult {
	result := &RedisProbeResult{}

	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	defer client.Close()

	// Ping
	if err := client.Ping(ctx).Err(); err != nil {
		result.Alive = false
		return result
	}
	result.Alive = true

	// Server info
	info, err := client.Info(ctx, "server", "memory", "clients", "keyspace").Result()
	if err == nil {
		result.Version = parseInfoValue(info, "redis_version")
		if v := parseInfoInt(info, "used_memory"); v > 0 {
			result.UsedMemoryMB = int(v / 1024 / 1024)
		}
		if v := parseInfoInt(info, "maxmemory"); v > 0 {
			result.MaxMemoryMB = int(v / 1024 / 1024)
		}
		if v := parseInfoInt(info, "connected_clients"); v > 0 {
			result.ConnectedClients = int(v)
		}
	}

	// Scan all keys (limited to first 500)
	var cursor uint64
	var allKeys []string
	for {
		keys, newCursor, err := client.Scan(ctx, cursor, "*", 100).Result()
		if err != nil {
			break
		}
		allKeys = append(allKeys, keys...)
		cursor = newCursor
		if cursor == 0 || len(allKeys) >= 500 {
			break
		}
	}
	result.TotalKeys = len(allKeys)

	// Get info for each key (limited to 100 most interesting)
	limit := len(allKeys)
	if limit > 100 {
		limit = 100
	}
	for _, key := range allKeys[:limit] {
		ki := RedisKeyInfo{Key: key}

		keyType, err := client.Type(ctx, key).Result()
		if err != nil {
			continue
		}
		ki.Type = keyType

		switch keyType {
		case "string":
			ki.Length = 1
		case "list":
			ki.Length, _ = client.LLen(ctx, key).Result()
		case "set":
			ki.Length, _ = client.SCard(ctx, key).Result()
		case "zset":
			ki.Length, _ = client.ZCard(ctx, key).Result()
		case "hash":
			ki.Length, _ = client.HLen(ctx, key).Result()
		}

		ttl, err := client.TTL(ctx, key).Result()
		if err == nil {
			ki.TTL = int64(ttl.Seconds())
		}

		result.Keys = append(result.Keys, ki)
	}

	return result
}

func probeMySQL(ctx context.Context, dsn string) *MySQLProbeResult {
	result := &MySQLProbeResult{}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return result
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(30 * time.Second)

	if err := db.PingContext(ctx); err != nil {
		result.Alive = false
		return result
	}
	result.Alive = true

	// Version
	db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&result.Version)

	// Connection info
	var connStr, maxStr string
	db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME='Threads_connected'").Scan(&connStr)
	db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME='max_connections'").Scan(&maxStr)
	fmt.Sscanf(connStr, "%d", &result.Connections)
	fmt.Sscanf(maxStr, "%d", &result.MaxConn)

	// Table sizes
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, TABLE_ROWS,
		       ROUND(DATA_LENGTH/1024/1024, 2) AS size_mb,
		       ROUND(INDEX_LENGTH/1024/1024, 2) AS index_mb
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
		ORDER BY DATA_LENGTH DESC
		LIMIT 50`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t MySQLTableInfo
			rows.Scan(&t.Name, &t.Rows, &t.SizeMB, &t.IndexSizeMB)
			result.Tables = append(result.Tables, t)
		}
	}

	return result
}

// FormatForAI returns a human-readable probe summary.
func (p *ProbeResult) FormatForAI() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("运行时探测结果 (%s)\n\n", p.Probed.Format("2006-01-02 15:04:05")))

	if p.Redis != nil {
		sb.WriteString("### Redis\n")
		if !p.Redis.Alive {
			sb.WriteString("  ❌ 连接失败\n")
		} else {
			sb.WriteString(fmt.Sprintf("  版本: %s, 内存: %dMB/%dMB, 客户端: %d, 总Key: %d\n",
				p.Redis.Version, p.Redis.UsedMemoryMB, p.Redis.MaxMemoryMB,
				p.Redis.ConnectedClients, p.Redis.TotalKeys))
			for _, k := range p.Redis.Keys {
				if k.Length > 100 { // only show significant keys
					sb.WriteString(fmt.Sprintf("  - %s (%s, len=%d, ttl=%ds)\n", k.Key, k.Type, k.Length, k.TTL))
				}
			}
		}
		sb.WriteString("\n")
	}

	if p.MySQL != nil {
		sb.WriteString("### MySQL\n")
		if !p.MySQL.Alive {
			sb.WriteString("  ❌ 连接失败\n")
		} else {
			sb.WriteString(fmt.Sprintf("  版本: %s, 连接: %d/%d\n",
				p.MySQL.Version, p.MySQL.Connections, p.MySQL.MaxConn))
			for _, t := range p.MySQL.Tables {
				sb.WriteString(fmt.Sprintf("  - %s: %d rows, %.1fMB data, %.1fMB index\n",
					t.Name, t.Rows, t.SizeMB, t.IndexSizeMB))
			}
		}
	}

	return sb.String()
}

// --- Info parsing helpers ---

func parseInfoValue(info, key string) string {
	for _, line := range strings.Split(info, "\r\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimPrefix(line, key+":")
		}
	}
	return ""
}

func parseInfoInt(info, key string) int64 {
	val := parseInfoValue(info, key)
	var n int64
	fmt.Sscanf(val, "%d", &n)
	return n
}
