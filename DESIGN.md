# AI Ops Agent — 完整设计文档

## 一、项目定位

**一句话**：AI 驱动的生产运行时智能监控与自愈框架。

**与现有项目的区别**：
- 现有开源项目（Self-Healing SRE Agent、Healing Agent、AIDevOps）全部聚焦 CI/CD 自愈（代码提交 → 构建失败 → AI 修复）
- 我们做的是**生产运行时监控**：系统已经在跑了 → 采集业务指标+日志 → AI 发现异常模式 → 关联代码定位根因 → 推送修改建议 → 验证修复结果
- 框架本身通用，通过配置文件对接任何项目，MVP 先在 mercari-hunter（日本二手市场监控系统）上验证

**解决的核心问题**：
过去每个生产 bug 平均花几小时人工排查（查日志、查数据库、看代码、猜根因），AI Ops Agent 要把这个过程缩短到几分钟自动完成。

---

## 二、真实案例 — 这些 bug 触发了这个项目

| # | Bug 描述 | 排查耗时 | 根因 | AI 能多快发现 |
|---|---------|---------|------|-------------|
| 1 | Worker 反复离线 | 3h | 服务器时钟偏移 44 秒，心跳判定过期 | L2: 关联心跳时间戳和服务器时间差异 |
| 2 | 自动下单从不触发 | 4h | Worker 没连上 MySQL，静默失败 | L1: MySQL 连接检测 = 0 |
| 3 | 推送显示 S 级但 DB 存 A 级 | 5h | 多 goroutine 共享 runtime 实例产生竞态 | L2: 发现推送评级和 DB 评级不一致 |
| 4 | 搜索任务堆积 10 万 | 2h | 每个任务重建 browserRuntime，极慢 | L1: 队列长度 > 阈值 |
| 5 | Redis 被入侵设成只读 | 3h | 外部攻击修改了 Redis 配置 | L1: 检测 slave-read-only 配置变更 |
| 6 | 商品 ID 双前缀导致去重失效 | 4h | dedup key 包含 scopeType，跨关键词不去重 | L2: 发现同商品被推送多次 |

---

## 三、能力分层（L0 → L4）

### L0: 项目分析（首次启动，一次性）

**目标**：AI 自动理解目标项目，生成监控配置，不需要用户手写规则。

```
输入：项目源码路径 + 运行环境连接信息

Step 1: 静态代码扫描（纯 AST 分析，不需要 AI）
├── 扫描 HTTP 路由注册 → 发现所有 API 接口
│   gin.GET("/v1/overview", handler) → {GET, /v1/overview}
│   gin.POST("/v1/order/create", handler) → {POST, /v1/order/create}
│
├── 扫描 Redis 操作 → 发现所有 Key 模式
│   rdb.LPush("mh:queue:search:pending", task) → {LPush, mh:queue:search:pending}
│   rdb.SetNX("mh:push:dedup:"+itemID, 1) → {SetNX, mh:push:dedup:{itemID}}
│
├── 扫描 SQL 语句 → 发现所有表和操作
│   "INSERT INTO proxy_orders ..." → {INSERT, proxy_orders}
│   "SELECT * FROM search_tasks WHERE ..." → {SELECT, search_tasks}
│
├── 扫描定时任务 → 发现调度频率
│   cron.AddFunc("*/5 * * * *", searchDispatch) → {每5分钟, 搜索分发}
│
├── 扫描日志模式 → 发现错误关键词
│   log.Error("下单失败: %v", err) → {error_pattern: "下单失败"}
│
└── 扫描配置文件 → 发现连接地址
    redis.addr: 127.0.0.1:6379
    mysql.dsn: user:pass@tcp(host:3306)/db

Step 2: 动态探测（连上运行环境看实际状态）
├── Redis SCAN → 所有 key + 类型 + 长度
├── MySQL information_schema → 表行数 + 索引状态
├── 逐个 API 调用 → 状态码 + 延迟 + 响应结构
└── 进程状态 → Worker 数量 + PID + 启动时间

Step 3: AI 综合分析（一次 Claude 调用）
├── 输入：静态扫描结果 + 动态探测结果
├── 输出：
│   - 每个组件的正常运行特征
│   - 推荐的监控指标和告警阈值
│   - 组件间的关联关系（"如果 A 异常 + B 正常，说明..."）
│   - 需要特别关注的风险点
└── 自动生成 config.yaml

Step 4: 基线学习期（前 24-48 小时）
├── 连续采集但不告警
├── 记录各指标的正常范围（均值、标准差、周期性波动）
│   队列长度: 通常 0-300，每小时峰值约 500
│   API 延迟: P50=80ms, P99=300ms
│   error 日志: 约 500 条/小时（这是正常的）
└── 生成动态基线 → 替代固定阈值
```

### L1: 全量采集 + 规则告警（持续运行）

**目标**：不遗漏地采集所有能采到的数据，硬阈值兜底。

```
每 30 分钟执行一次：

采集层（插件式，不需要 AI）
├── Redis 采集器
│   - 所有 key 的长度变化
│   - 内存使用量 / 连接数 / 被拒绝连接数
│   - 配置变更检测（安全）
│
├── MySQL 采集器
│   - 连接状态 / 线程数 / 慢查询数
│   - 自定义 SQL 检查（待处理订单数、失败订单数等）
│
├── HTTP 采集器
│   - 每个 API 接口的状态码 + 延迟
│   - JSON 响应中关键字段的值
│
└── 日志采集器
    - journalctl / docker logs / 文件
    - 按模式匹配统计 error/warning 数量
    - 提取最近的错误日志内容

规则引擎
├── 静态规则：queue_length > 1000 → CRITICAL
├── 基线规则：error_rate > baseline_mean + 3*std → WARNING
└── 存活检测：connection_alive == 0 → FATAL

输出：系统快照（全部 Metric + LogEntry）+ 触发的告警
```

### L2: AI 智能分析（核心价值）

**目标**：做人类级别的根因分析，发现硬阈值无法表达的异常。

```
输入：L1 的完整系统快照 + 历史基线 + 已知 Issue 列表

Analyst Agent（单一 AI，支持自校正追问）

第一轮分析：
├── 单指标异常检测
│   "队列从 300 涨到 50000，偏离基线 166 倍"
│
├── 跨指标关联分析
│   "下单接口调用量 = 0 + MySQL error 日志 47 条 + Worker 心跳正常 5 台
│    → 推断：Worker 在运行但 MySQL 连接失败"
│
├── 趋势预测
│   "Redis 内存使用率每天增长 2%，按当前速度 15 天后达到 maxmemory"
│
├── 代码关联（结合 L0 的代码扫描结果）
│   "队列堆积在 mh:queue:search:pending
│    代码中该队列由 searchDispatch 写入，由 searchWorker 消费
│    消费端每任务调用 NewBrowserRuntime()，可能是性能瓶颈"
│
└── 置信度评估
    90%+ → 直接推送结论
    60-90% → 标记"需要验证"，请求更多数据
    <60% → 仅记录，不推送

自校正循环（如果置信度不够高）：
├── Analyst: "置信度 70%，需要额外信息：
│             1. Worker 最近 50 条日志
│             2. MySQL SHOW PROCESSLIST"
├── 系统执行额外采集
├── Analyst 第二轮: "确认根因，置信度 95%"
└── 最多 3 轮，超过则标记为"需人工介入"
```

### Issue 状态管理（贯穿 L2-L3）

**目标**：追踪问题的完整生命周期，不重复告警，知道什么时候修好了。

```
Issue 生命周期：

  OPEN → MONITORING → RESOLVED → CLOSED
   │         │            │          │
   │         │            │          └── 连续 3 个周期无异常，自动关闭
   │         │            └── 修复验证通过
   │         └── 推送给用户后，持续跟踪（恶化/持平/改善）
   └── AI 首次发现异常

每个 Issue 包含：
┌──────────────────────────────────────────────┐
│ Issue #3                                     │
│                                              │
│ 状态: OPEN                                   │
│ 严重度: CRITICAL                              │
│ 首次发现: 2026-04-07 03:30                    │
│ 持续时间: 2 小时                              │
│ 趋势: 恶化中 📈                              │
│                                              │
│ 标题: Worker 未连接 MySQL — 自动下单已停摆     │
│                                              │
│ 证据:                                        │
│  - /v1/order/create 最近 2 小时调用量: 0      │
│  - proxy_orders 表最近 2 小时新增: 0 条        │
│  - Worker 日志 "connection refused" × 47      │
│  - Worker 心跳正常 (5台在线)                   │
│  - MySQL 连接数: 正常 (12/100)                │
│                                              │
│ AI 根因分析:                                  │
│  Worker 进程在运行但无法连接 MySQL。            │
│  MySQL 本身正常（连接数未满），                 │
│  推断 Worker 的 DSN 配置有误或 DNS 解析失败。  │
│                                              │
│ 修改建议:                                     │
│  1. 检查 Worker 配置文件中的 MySQL 地址         │
│  2. 在 Worker 机器上执行 mysql -h <host> 测试  │
│  3. 相关代码位置:                              │
│     - config/database.go:23 (DSN 定义)        │
│     - worker/order.go:127 (下单连接)           │
│                                              │
│ 历史记录:                                     │
│  03:00 ✅ 正常                                │
│  03:30 ❌ 发现异常 (OPEN)                     │
│  04:00 ❌ 恶化 (error 日志从 47→120)           │
│  04:30 ❌ 持续 (推送第二次提醒)                │
│  05:00 ✅ 恢复 (用户修复后 RESOLVED)           │
│  05:30 ✅ 确认修复 (CLOSED)                   │
└──────────────────────────────────────────────┘

推送策略（避免骚扰）：
├── 新 Issue → 立即推送
├── 状态变化（恶化/改善/修复）→ 推送
├── 持续未修复 → 每 2 小时提醒一次（可配置）
└── 已推送且无变化 → 不再推送
```

### L3: 部署感知 + 自动验证

**目标**：用户修完代码部署后，自动验证是否真的修好了，以及是否引入新问题。

```
部署检测方式（三选一，按优先级）：
├── 方式 1: Git Webhook → 收到 push 事件立即触发
├── 方式 2: 定期 git log → 检测新 commit
└── 方式 3: 进程监控 → 检测 PID 变化或启动时间变化

检测到部署后：
├── 立即触发一次完整采集（不等 30 分钟）
├── 针对所有 OPEN/MONITORING 的 Issue 逐个验证
│   ├── 提取 Issue 的关键指标
│   ├── 采集当前值
│   ├── 对比：异常是否消失？
│   └── 更新 Issue 状态
│
├── 回归检测：对比部署前后的全量指标
│   ├── 新出现的异常 → 可能是本次部署引入
│   └── 与 git diff 关联 → "延迟上升可能与 commit abc1234 修改的查询有关"
│
└── 推送验证报告
    ┌──────────────────────────────────────────┐
    │ 📋 部署后验证报告                         │
    │ commit: abc1234 "fix: 修复 mysql 连接"    │
    │                                          │
    │ ✅ Issue #3 Worker 未连 MySQL    → 已修复 │
    │ ⚠️ Issue #5 队列堆积 5万→8000   → 改善中 │
    │                                          │
    │ 🆕 新发现:                                │
    │ ⚠️ /v1/search/tasks 延迟 100ms→800ms     │
    │    可能与本次 commit 修改的查询逻辑有关     │
    └──────────────────────────────────────────┘
```

### L4: 自动修复（远期目标）

**目标**：AI 生成修复代码，经过交叉验证后创建 PR，等人工确认。

```
这个阶段才需要多 Agent 协作：

Analyst Agent  → "根因定位完成，需要修改 proxy_order.go:530"
     │
     ▼
Coder Agent    → 生成修复代码（用 Claude）
     │
     ▼
Reviewer Agent → 审查修复代码（故意用不同模型，如 Gemini）
     │            "写代码的不能审自己的代码"
     │            不同模型有不同的盲区，交叉验证更安全
     │
     ├── 通过 → 创建 PR，推送给用户确认
     └── 不通过 → 打回给 Coder，附上修改意见（最多 3 轮）

安全护栏：
├── 只允许修改 AI 能理解的范围内的代码
├── 每次修复必须通过 AST 语法验证
├── 修复后自动跑测试（如果有）
├── PR 描述包含完整分析链路（证据→根因→修复逻辑）
└── 永远不自动合并，必须人工确认
```

---

## 四、架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                        AI Ops Agent                         │
│                                                             │
│  ┌─────────┐    ┌──────────┐    ┌──────────┐    ┌────────┐ │
│  │   L0    │    │    L1    │    │    L2    │    │   L3   │ │
│  │ 项目分析 │───→│ 采集+规则 │───→│ AI 分析  │───→│部署验证│ │
│  │(一次性)  │    │(每30min) │    │(每30min) │    │(事件驱)│ │
│  └─────────┘    └──────────┘    └──────────┘    └────────┘ │
│       │              │               │               │      │
│       │              │               │               │      │
│       ▼              ▼               ▼               ▼      │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                    共享基础设施                        │   │
│  │                                                      │   │
│  │  ┌──────────┐  ┌──────────┐  ┌───────┐  ┌────────┐  │   │
│  │  │ 采集器    │  │ Issue    │  │ 知识库 │  │ 推送    │  │   │
│  │  │ Registry │  │ Tracker  │  │ Store  │  │ Alert  │  │   │
│  │  │          │  │          │  │        │  │ Mgr    │  │   │
│  │  │ -Redis   │  │ -创建    │  │ -基线  │  │        │  │   │
│  │  │ -MySQL   │  │ -更新    │  │ -历史  │  │ -Bark  │  │   │
│  │  │ -HTTP    │  │ -关闭    │  │  Issue │  │ -TG    │  │   │
│  │  │ -Log     │  │ -推送策略│  │ -案例库│  │ -Slack │  │   │
│  │  │ -自定义  │  │          │  │        │  │ -邮件  │  │   │
│  │  └──────────┘  └──────────┘  └───────┘  └────────┘  │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                    AI 层                              │   │
│  │                                                      │   │
│  │  ┌────────────────────────────────────────────────┐  │   │
│  │  │ Analyst Agent（L0/L2/L3 共用）                  │  │   │
│  │  │                                                │  │   │
│  │  │ - 项目分析（L0: 理解代码结构 → 生成配置）        │  │   │
│  │  │ - 异常检测（L2: 读快照 → 发现模式 → 根因分析）   │  │   │
│  │  │ - 验证判断（L3: 对比部署前后 → 判断是否修复）     │  │   │
│  │  │ - 支持自校正追问（最多 3 轮）                    │  │   │
│  │  │ - 置信度评分（>90% 确认 / 60-90% 追问 / <60%）  │  │   │
│  │  └────────────────────────────────────────────────┘  │   │
│  │                                                      │   │
│  │  ┌──────────────┐  ┌──────────────┐  (L4 远期)      │   │
│  │  │ Coder Agent  │  │Reviewer Agent│                  │   │
│  │  │ (生成修复)    │  │(交叉审查)    │                  │   │
│  │  └──────────────┘  └──────────────┘                  │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

---

## 五、Agent 设计原则

### 不搞假的"多 Agent 商讨"

```
❌ 错误做法：
Agent A 调 Claude → 得到分析 X
Agent B 调 Claude → 得到分析 Y
Agent C 调 Claude → 把 X+Y 总结
→ 花 3 倍钱，结果和一次调用差不多（底层同一个模型）

✅ 正确做法：
多个采集器（不是 Agent）并行采集数据
一个 Analyst Agent 拿到全部数据后一次性分析
如果不确定 → 自校正追问（要更多数据，不是要更多 Agent）
```

### 什么时候真的需要多 Agent

```
唯一需要的场景：L4 自动修复时的交叉验证

Coder Agent (Claude)    → 生成修复代码
Reviewer Agent (Gemini) → 审查修复代码（故意用不同模型）

原因：不同模型有不同的盲区和幻觉模式
     同一个模型写的代码让自己审 = 形同虚设
     不同模型交叉验证才有真正的对抗效果
```

### Agent 接口设计（预留扩展）

```go
// 统一的 Agent 接口，现在只实现 Analyst，未来可扩展
type Agent interface {
    Name() string
    Analyze(ctx context.Context, input AgentInput) (*AgentOutput, error)
}

// 现阶段
analyst := NewAnalystAgent(claudeClient)

// L4 阶段扩展
coder    := NewCoderAgent(claudeClient)
reviewer := NewReviewerAgent(geminiClient)
```

---

## 六、数据流：一个完整周期

```
00:00  定时器触发（或部署事件触发）
        │
00:01  L1 采集层并行执行
        ├── Redis 采集器 → 30 个 Metric
        ├── MySQL 采集器 → 15 个 Metric
        ├── HTTP 采集器  → 20 个 Metric + 响应体缓存
        └── Log 采集器   → 5 个 Metric + 200 条错误日志
        │
00:02  L1 规则引擎
        ├── 70 个 Metric 逐条匹配规则
        ├── 3 条规则触发 → 3 个 Alert
        └── 输出：系统快照（Snapshot）
        │
00:03  L2 AI 分析
        ├── 构建 Prompt:
        │   "系统快照 + 基线数据 + 当前 OPEN 的 Issue"
        │
        ├── 第一轮 Claude 调用（约 5 秒）
        │   → "发现 3 个异常，其中 1 个置信度 70% 需要追问"
        │
        ├── 执行额外采集（Analyst 要求的）
        │   → Worker 最近 50 条日志
        │
        ├── 第二轮 Claude 调用（约 3 秒）
        │   → "确认根因，置信度 95%"
        │
        └── 输出：AI 分析报告 + Issue 创建/更新
        │
00:04  Issue 管理
        ├── 新异常 → 创建 Issue #7
        ├── 已知异常 → 更新 Issue #3（恶化）
        ├── 已修复异常 → 关闭 Issue #5
        └── 生成推送内容
        │
00:05  告警推送
        ├── 新 Issue → Bark 推送到手机
        ├── 状态变化 → Telegram 通知
        └── 已推送且无变化 → 跳过（不骚扰）
        │
00:05  周期结束，数据存入知识库
        等待下一个 30 分钟
```

---

## 七、通用化设计

### 框架不绑定任何特定项目

```yaml
# mercari-hunter 的配置
project: mercari-hunter
source_code: /path/to/mercari-hunter
language: go

collectors:
  redis:
    - addr: "127.0.0.1:6379"
      password: "${REDIS_PASSWORD}"
  mysql:
    - dsn: "${MYSQL_DSN}"
  http:
    - url: "http://localhost:8080/v1/overview"
  log:
    - source: journalctl
      unit: mercari-worker-process
```

```yaml
# 另一个 Python 项目的配置
project: my-flask-app
source_code: /path/to/flask-app
language: python

collectors:
  redis:
    - addr: "redis.internal:6379"
  http:
    - url: "http://localhost:5000/health"
  log:
    - source: docker
      container: flask-app
```

### 插件化扩展点

```
可扩展的组件：
├── 采集器: 实现 Collector 接口即可添加新数据源
│   内置: Redis, MySQL, HTTP, Log
│   未来: PostgreSQL, MongoDB, Prometheus, CloudWatch
│
├── 告警通道: 实现 Alerter 接口即可添加新推送方式
│   内置: Bark, Telegram, Console
│   未来: Slack, 钉钉, 企业微信, PagerDuty, Email
│
├── 代码扫描器: 按语言实现不同的 AST 解析
│   内置: Go (gin 路由, go-redis, database/sql)
│   未来: Python (Flask/Django), Node.js (Express), Java (Spring)
│
└── AI Provider: 统一接口，可切换模型
    内置: Claude (Anthropic)
    未来: GPT (OpenAI), Gemini (Google), 本地模型
```

---

## 八、技术选型

| 组件 | 选型 | 理由 |
|------|------|------|
| 语言 | Go | 与 mercari-hunter 一致；单二进制部署；并发采集性能好 |
| AI API | Claude API | 支持长上下文（分析大量日志）；代码理解能力强 |
| 配置 | YAML + 环境变量 | 通用且易读；敏感信息走环境变量 |
| 存储 | SQLite | 知识库、Issue 追踪、历史数据；零依赖，嵌入二进制 |
| 调度 | 内置 cron | 单二进制，不依赖系统 cron |
| 部署 | 单二进制 | go build 出一个文件，扔到服务器就跑 |

---

## 九、实施路线

```
Phase 1 (当前): L0 + L1 基础 ✅ 已完成部分
├── 项目骨架、采集器插件系统、规则引擎、告警通道
├── Redis/MySQL/HTTP/Log 四种采集器
├── Bark/Telegram/Console 三种推送
└── YAML 配置驱动 + cron 调度

Phase 2 (下一步): L0 完善 + L2 AI 分析
├── Go 代码静态扫描器（路由、Redis key、SQL 表）
├── 动态探测（连上去看实际状态）
├── 接入 Claude API — Analyst Agent
├── 基线学习（前 24-48 小时）
├── AI 异常检测 + 根因分析
└── Issue 状态管理 + 推送策略

Phase 3: L3 部署验证
├── Git 变更检测（webhook / polling）
├── 部署后立即验证
├── 回归检测
└── 修复确认报告

Phase 4 (远期): L4 自动修复
├── Coder Agent（生成修复代码）
├── Reviewer Agent（交叉验证，不同模型）
├── PR 自动创建
└── 安全护栏（AST 验证、测试运行、人工确认）
```

---

## 十、成本估算

```
每次监控周期的 AI 调用：
├── L2 分析: 约 2000-5000 token input + 500-1000 token output
├── 自校正追问: 0-2 次额外调用
├── 单次成本: 约 $0.01-0.03 (Claude Sonnet)
│
├── 每天 48 次 (每 30 分钟) = $0.48-1.44/天
├── 每月 ≈ $15-45
│
└── 如果用更便宜的模型做初筛:
    Haiku 初筛 → 无异常直接跳过 → 有异常再调 Sonnet
    可降低到 $5-15/月
```

---

## 十一、与竞品的差异

| 维度 | Self-Healing SRE | Healing Agent | AIDevOps | **AI Ops Agent (我们)** |
|------|-----------------|---------------|----------|----------------------|
| 监控时机 | CI 失败后 | 运行时异常 | CI/CD 全流程 | **生产运行时持续监控** |
| 接入方式 | 改代码加 hook | 加装饰器 | 配 GitHub Action | **零侵入，纯外部观察** |
| 语言支持 | Python only | Python only | 多语言 | **Go 优先，可扩展** |
| 分析能力 | 单错误修复 | 单函数修复 | 多 agent 分工 | **全系统关联分析** |
| Issue 管理 | 无 | 无 | GitHub Issue | **内建生命周期管理** |
| 部署验证 | 无 | 无 | 有 | **自动验证+回归检测** |
| 基线学习 | 无 | 无 | 无 | **动态基线** |
| 通用性 | 固定项目 | Python 项目 | GitHub 项目 | **任何项目，YAML 配置** |

---

## 十二、根据外部评审的补充设计（GPT + Gemini 反馈采纳）

### 12.1 已实现的架构补强

#### 数据脱敏层 (`internal/sanitize/`)
- 在 L1 采集完毕、提交给 L2 AI 分析之前，强制过滤敏感数据
- 覆盖：API Key/Token、JWT、AWS Key、连接字符串密码、内网 IP、邮箱、手机号、Hex Token、Cookie
- 字段名敏感检测（password、secret、token 等字段直接替换为 `<REDACTED>`）
- 白名单机制：只有声明为安全的字段才上报给大模型

#### Issue 一等公民模型 (`internal/issue/tracker.go`)
完整状态机：
```
DETECTING → OPEN → ACKED/MONITORING → IMPROVING → RESOLVED → CLOSED
                                                       ↑
                                                  REOPENED (24h内)
                                                       
特殊状态: FLAPPING (反复横跳) / DATA_GAP (采集缺失)
```

Anti-flapping 机制：
- `consecutive_bad >= 2` 才从 DETECTING → OPEN（确认后才报）
- `consecutive_good >= 3` 才从 OPEN → RESOLVED（稳定后才关）
- `reopen_count >= 3` 且在 24h 内 → 标记 FLAPPING，停止逐次推送
- FLAPPING 状态下改为每日汇总播报
- 支持 `/mute #IssueID 24h` 静默

Issue 指纹 = hash(service + metricName + severity + labels)，不是简单的 title 去重。
同指纹 24h 内复现 → reopen 旧 Issue，不新建。

#### 快照完整度评分 (`types.SnapshotHealth`)
- 每次采集记录 `completeness`（成功采集器 / 总采集器）
- `trustworthy = completeness >= 0.5`
- 不可信快照中，采集缺失的指标不会被当作"正常" → Issue 进入 DATA_GAP 而非 RESOLVED

#### Agent 自身健康检查 (`internal/health/`)
- 每个周期开始前检查：网络连通性（DNS + TCP 8.8.8.8:53）、磁盘可写性
- 如果 Agent 自身网络不通 → 抑制所有 FATAL 级告警（可能是网络分区导致误报）
- 解决"谁来监控监控者"问题

#### 采集器自保护
- 每个采集器有独立超时（默认 30s），一个慢采集器不会拖垮整轮
- 所有采集器并行执行（之前是串行）
- 未来可加：目标系统高负载时自动降级采集频率

#### 依赖树告警收敛 (`internal/rule/suppression.go`)
- MySQL 连接断开 → 自动抑制依赖 MySQL 的所有下游告警（pending_orders、failed_orders 等）
- Redis 连接断开 → 自动抑制队列长度等下游告警
- API 不可达 → 自动抑制 JSON path 检查告警
- 避免"MySQL 挂了 → 收到 20 条不同的告警"的告警风暴

#### 事件驱动 AI 调用 (`internal/analyzer/gate.go`)
不是每 30 分钟固定调 AI，而是只在以下条件触发：
- CRITICAL/FATAL 规则触发
- 告警数量显著增加
- 从零告警变为有告警（状态变化）
- 有 OPEN 的 Issue 需要持续分析
- 错误日志量突增（>100）
- 快照健康度下降（采集器故障）

其余"一切正常"的周期 → 跳过 AI 调用 → 省钱

### 12.2 计划采纳但未实现（Phase 2-3）

#### 三层采集节奏
```
Tier 1 (15-60s):  存活检测 + 核心 SLA
  - Redis/MySQL/API ping
  - Worker 心跳
  
Tier 2 (2-5min):  关键业务指标
  - 队列长度变化
  - 订单成功/失败率
  - 推送延迟
  
Tier 3 (15-30min): 重型扫描 + 基线更新
  - 全量日志分析
  - 表大小/索引状态
  - AI 深度分析
```

#### 业务不变量 DSL
```yaml
invariants:
  - name: "订单创建率不应长期为零"
    condition: "rate(mysql.check.new_orders, 1h) == 0"
    when: "workers_online > 0"
    severity: critical
    
  - name: "同商品24h内不应重复推送"
    condition: "push_dedup_violations > 0"
    severity: warning
    
  - name: "推送评级与DB评级应一致"
    condition: "push_grade != db_grade"
    severity: critical
```
这是最有差异化壁垒的功能 — 大厂监控不做业务语义。

#### 领域知识注入 (Runbooks)
```yaml
domain_knowledge:
  - trigger: "大量 HTTP 403 + error 包含 'proxy'"
    meaning: "代理池枯竭，不是代码 bug"
    action: "检查代理供应商余额和可用 IP 数"
    
  - trigger: "Redis SLAVEOF 配置变更"
    meaning: "可能被入侵，历史上发生过"
    action: "立即检查 Redis AUTH 和防火墙规则"
```

#### Change Feed（超越 Git）
```
监控范围不只是代码部署：
├── Git push（代码变更）
├── 配置文件变更（config.yaml / .env）
├── Feature flag 切换
├── DB schema 变更（ALTER TABLE）
├── SSL 证书即将过期
├── 系统时间漂移
├── 第三方 API 退化（目标网站改版/限流加强）
└── 服务器资源变化（磁盘满/内存不足）
```

#### Parent Incident 关联
```
而不是：
  ❌ Alert: MySQL 连接失败
  ❌ Alert: 订单数为 0
  ❌ Alert: API /order/create 500
  ❌ Alert: 错误日志超 100 条

应该是：
  🔴 Incident #1: MySQL 连接中断
  ├── 影响: 下单功能停摆
  ├── 关联: 订单数归零、API 500、错误日志飙升
  └── 这些都是同一个根因的下游表现
```

#### 用户反馈闭环
```
推送告警后：
  用户回复 ✅ → "根因正确" → 存入知识库，增强未来分析
  用户回复 ❌ → "根因错误" → 记录失败案例，调整 prompt
  用户回复 🔇 → "已知问题，静默 24h"

长期积累 → 模型针对你的系统越来越准
```

#### Prompt Caching
```
每次 AI 调用的 prompt 结构：
┌─────────────────────────────────┐
│ 静态上下文（缓存，不重复传输）    │  ← Claude Prompt Caching
│ - 系统架构描述                   │     节省 ~80% input token
│ - 组件关联关系                   │
│ - 领域知识 / Runbooks           │
│ - 历史 Issue 模式               │
├─────────────────────────────────┤
│ 动态内容（每次不同）              │  ← 只传变化部分
│ - 本周期快照 delta               │
│ - 新触发的规则                   │
│ - 当前 OPEN Issue 状态更新       │
└─────────────────────────────────┘
```

### 12.3 记录但暂不采纳

| 建议 | 暂不做的原因 |
|------|------------|
| eBPF 内核探针 | Windows 不支持，部署复杂度高，MVP 不需要 |
| 本地小模型初筛 | 规则引擎 + AI gate 已经能做初筛，不需要额外部署模型 |
| On-Premises 本地大模型 | 商业化阶段才考虑 |
| 可选埋点 SDK | 违背"零侵入"定位，后期按需加 |
| 常量传播/调用图分析 | 过于学术，AST + 动态探测够用 |

### 12.4 L0 可行性校准

经 GPT 评估，纯 AST 对 Go + gin + go-redis + database/sql 项目的覆盖率：
- HTTP 路由：80%-95%（直接注册的）
- Redis key：60%-80%（字面量的）
- SQL 表名：50%-70%（原生 SQL 的）
- 日志模式：30%-60%

**结论：L0 输出改为"draft config + 置信度 + 待确认清单"，不承诺全自动。**

### 12.5 商业化定位校准

**不卖**："又一个监控平台"（正面撞 Datadog/New Relic）

**要卖**："面向中小团队的、代码感知的 RCA + 业务不变量监控"

**定价模型**：按 service/project + investigation 次数，不按 host/GB
- Free: L1 基础监控
- Pro $99-199/服务/月: L2 AI 分析 + Issue 管理
- Team $299-999/组织/月: 多项目 + L3 部署验证
- Self-hosted: 企业定制

**北极星指标**：MTTD（平均发现时间）、MTTR（平均恢复时间）、误报率、一次定位根因比例

**ICP（理想客户画像）**：Go + Redis + MySQL + 队列型服务的中小团队

---

## 十三、当前代码结构（更新后）

```
ai-ops-agent/
├── cmd/agent/main.go              # 入口，组装所有组件
├── internal/
│   ├── agent/agent.go             # 核心编排：自检→采集→规则→抑制→Issue→通知
│   ├── collector/
│   │   ├── collector.go           # Collector 接口 + 并行采集 + 健康评分
│   │   ├── redis.go               # Redis 采集器（队列/内存/连接/安全检测）
│   │   ├── mysql.go               # MySQL 采集器（连接/自定义查询）
│   │   ├── http.go                # HTTP 采集器（健康检查/JSON提取）
│   │   └── log.go                 # 日志采集器（journalctl/docker/file）
│   ├── rule/
│   │   ├── engine.go              # 阈值规则引擎
│   │   └── suppression.go         # 依赖树告警收敛
│   ├── issue/
│   │   └── tracker.go             # Issue 生命周期管理（9 状态 + 抗抖动）
│   ├── alert/
│   │   ├── alert.go               # Alerter 接口 + 派发管理
│   │   ├── bark.go                # Bark 推送
│   │   ├── telegram.go            # Telegram 推送
│   │   └── console.go             # 控制台输出
│   ├── sanitize/
│   │   └── sanitize.go            # 数据脱敏（密钥/PII/Token 过滤）
│   ├── health/
│   │   └── health.go              # Agent 自身健康检查（网络/磁盘）
│   ├── analyzer/
│   │   └── gate.go                # 事件驱动 AI 调用门控
│   ├── config/config.go           # YAML 配置加载
│   ├── knowledge/                 # (L2) 知识库
│   └── report/                    # (L2) 报告生成
├── pkg/types/types.go             # 完整类型系统（Issue/Snapshot/Health）
├── config.yaml                    # mercari-hunter 示例配置
├── DESIGN.md                      # 本文档
└── go.mod
