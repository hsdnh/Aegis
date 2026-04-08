# AI Ops Agent — 生产运行时智能监控与自愈框架

AI 驱动的生产系统监控平台。自动采集指标、AI 分析根因、追踪代码链路、一键排查修复。

**68 个源文件 | 14000+ 行代码 | 17 个单元测试 | 零 CGO 依赖 | 单二进制部署**

## 一行安装

```bash
bash <(curl -sS https://raw.githubusercontent.com/hsdnh/Aegis/main/install.sh)
```

交互式菜单，输入数字选择功能。也支持 Docker：

```bash
docker run -d -p 9090:9090 -v ./config.yaml:/app/config.yaml ghcr.io/hsdnh/Aegis
```

## 核心能力

| 能力 | 说明 | 状态 |
|------|------|:---:|
| **L0 项目扫描** | AST 分析代码 → 自动发现 API/Redis/MySQL/定时任务 → AI 生成监控配置 | ✅ |
| **L1 指标采集** | Redis/MySQL/HTTP/日志 四种采集器，并行执行，30 秒超时保护 | ✅ |
| **L1.5 代码追踪** | SDK 探针注入，窗口式采样（关闭时 1ns 开销），函数级耗时追踪 | ✅ |
| **L2 AI 分析** | Claude/OpenAI 分析指标+日志+追踪数据，自校正最多 3 轮，置信度评分 | ✅ |
| **L3 Issue 管理** | 9 状态生命周期，抗抖动，依赖树告警收敛，父子 Incident 聚合 | ✅ |
| **因果链追踪** | 双向追踪：异常指标 → 代码行，代码变更 → 影响范围 | ✅ |
| **自主调查** | AI 自动执行 shell/MySQL/Redis 命令调查异常，带完整调查报告 | ✅ |
| **数据验证** | 检查站断言 + 影子验证，发现"跑通了但结果错"的逻辑 bug | ✅ |
| **分布式集群** | Master-Worker 架构，子服务器完整监控 + 向主节点汇报 | ✅ |
| **可视化面板** | 中文界面，20+ 功能区块，拓扑图/火焰图/瀑布图 | ✅ |

## 快速开始

```bash
# 构建
make build

# 单机运行
./bin/ai-ops-agent -config config.yaml -dashboard 127.0.0.1:9090

# 打开浏览器访问面板
open http://127.0.0.1:9090

# 单次运行（测试模式）
./bin/ai-ops-agent -config config.yaml -once
```

### 分布式部署

```bash
# 主节点（运行面板 + AI 分析 + 汇总所有子节点）
./bin/ai-ops-agent -mode master -config config.yaml -dashboard 0.0.0.0:9090

# 子节点（完整本地监控 + 向主节点汇报）
./bin/ai-ops-agent -mode worker -master http://主节点IP:9090 -node worker-1 -config worker.yaml
```

### SDK 探针（可选，深度代码追踪）

```bash
# 注入探针（不改业务逻辑，只在函数入口加追踪）
./bin/ai-ops-agent-instrument ./path/to/project/...

# 目标项目 main.go 加一行
import _ "github.com/hsdnh/Aegis/sdk/autotrace"

# 一键卸载（完全还原源码）
./bin/ai-ops-agent-instrument -strip ./path/to/project/...
```

## 配置

```yaml
project: my-project
schedule: "*/30 * * * *"

collectors:
  redis:
    - addr: "127.0.0.1:6379"
      password: "${REDIS_PASSWORD}"
      checks:
        - key_pattern: "queue:*:pending"
          threshold: 1000
          alert: "队列堆积"
  mysql:
    - dsn: "${MYSQL_DSN}"
      checks:
        - query: "SELECT COUNT(*) FROM orders WHERE status='pending'"
          name: "pending_orders"
          threshold: 50
          alert: "订单堆积"
  http:
    - url: "http://localhost:8080/health"
  log:
    - source: file
      file_path: "/var/log/app.log"
      error_patterns: ["error", "panic", "fatal"]

rules:
  - name: "队列堆积"
    metric_name: "redis.keys.queue_all_pending.total_length"
    operator: ">"
    threshold: 1000
    severity: "critical"

storage:
  enabled: true
  path: "./data/aiops.db"

ai:
  enabled: true
  provider: claude
  api_key: "${CLAUDE_API_KEY}"
  model: claude-sonnet-4-20250514

alerts:
  console: {}
  # bark:
  #   keys: ["your-key"]
  # telegram:
  #   token: "your-token"
  #   chat_ids: [123456789]
```

### 环境变量

| 变量 | 说明 |
|------|------|
| `REDIS_PASSWORD` | Redis 密码 |
| `MYSQL_DSN` | MySQL 连接字符串 |
| `CLAUDE_API_KEY` | Claude API 密钥（启用 AI 分析） |
| `AIOPS_DASHBOARD_TOKEN` | 面板访问令牌（启用鉴权） |
| `AIOPS_CLUSTER_TOKEN` | 集群节点间通信令牌 |
| `AIOPS_HEALTH_TARGET` | 自定义健康检查目标（默认多 DNS 探测） |

## 面板功能

面板默认监听 `127.0.0.1:9090`，全中文界面：

| 面板 | 功能 |
|------|------|
| 📡 集群节点 | 所有子服务器状态、健康评分、CPU/内存 |
| 🔗 问题追踪 | Issue 生命周期、根因、代码位置、一键排查按钮 |
| ⚡ 规则状态 | 规则触发状态指示器 |
| 🧠 AI 分析 | 置信度条、异常列表、根因、修复建议 |
| 🕵️ 自主调查 | AI 自动执行的调查命令和结论 |
| 📊 关键指标 | 按来源分组、基线偏离标注 |
| ✅ 数据验证 | 检查站断言 + 影子验证结果 |
| 🧪 健康探测 | 合成交易探测通过/失败 |
| 🔬 实时动态 | 每步操作实时流（3 秒刷新） |
| 💬 AI 助手 | 对话式查询系统状态 |
| ⚙️ 系统管理 | 探针安装卸载、维护静默、基线进度、配置体检 |

另有三个独立可视化页面：
- `/topology.html` — D3.js 力导向流量拓扑图（粒子动画）
- `/flamegraph.html` — 函数耗时火焰图
- `/waterfall.html` — 单请求瀑布时间线

## 安全

- 面板默认绑定 `127.0.0.1`（仅本机访问）
- 设置 `AIOPS_DASHBOARD_TOKEN` 启用令牌鉴权（所有 API 路由）
- AI 调查只执行白名单命令（ps/grep/tail 等只读）
- MySQL 只允许 SELECT，Redis 只允许读操作
- 数据脱敏层过滤密码/Token/PII 后再发给 AI
- Agent 自检防止网络分区误报

## 项目结构

```
ai-ops-agent/
├── cmd/
│   ├── agent/           # 主程序（--mode standalone/worker/master）
│   ├── init/            # L0 项目扫描器
│   ├── instrument/      # SDK 探针注入/卸载工具
│   └── uninstall/       # 完整卸载器
├── internal/
│   ├── agent/           # 核心 15 步监控流水线
│   ├── ai/              # Claude/OpenAI 客户端 + Analyst 分析引擎
│   ├── alert/           # Bark/Telegram/Console 推送
│   ├── analyzer/        # 事件驱动 AI 调用门控
│   ├── causal/          # 因果图 + 预期模型 + 影子验证 + 合成探测
│   ├── changefeed/      # 变更时间线（git/config/schema）
│   ├── cluster/         # Master-Worker 分布式集群
│   ├── collector/       # Redis/MySQL/HTTP/Log 采集器
│   ├── config/          # YAML 配置加载
│   ├── dashboard/       # Web 面板 + REST API + AI Terminal
│   ├── health/          # Agent 自身健康检查
│   ├── healthcheck/     # 启动配置体检
│   ├── investigator/    # 自主 AI 调查引擎
│   ├── issue/           # Issue 生命周期 + 静默 + Incident 聚合
│   ├── rule/            # 规则引擎 + 依赖树告警收敛
│   ├── runbook/         # 一键排查动作
│   ├── sanitize/        # 数据脱敏
│   ├── scanner/         # L0 Go AST 扫描 + 动态探测 + AI 配置生成
│   ├── storage/         # SQLite 持久化 + 基线学习
│   └── tracecollector/  # SDK 追踪数据接收
├── sdk/
│   ├── autotrace/       # 一行 import 自动初始化
│   └── probe/           # 追踪探针（环形缓冲/窗口采样/检查站/远程控制）
├── pkg/types/           # 共享类型定义
├── config.yaml          # 示例配置
├── install.sh           # 一键安装脚本（交互式菜单）
├── Makefile             # 构建/测试/发版
├── Dockerfile           # 容器化
├── docker-compose.yml   # Docker 一键部署
└── .github/workflows/   # CI + 自动发版
```

## 构建

```bash
make build          # 构建所有二进制
make test           # 运行测试
make docker         # 构建 Docker 镜像
make release        # 构建多平台发布包
make help           # 查看所有命令
```

## License

MIT
