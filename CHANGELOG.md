# Changelog

## v0.2.0 (2026-04-08)

### 新功能
- **分布式集群**: Master-Worker 架构，子服务器完整监控 + 汇报
- **自主调查**: AI 自动执行 shell/MySQL/Redis 命令排查异常
- **因果链追踪**: 双向追踪（异常→代码行，代码变更→影响范围）
- **数据验证**: 检查站断言 + 影子验证，发现逻辑 bug
- **预期模型**: 学习正常链路模式，检测异常步骤/顺序/耗时
- **合成探测**: 主动发测试请求验证功能可用性
- **变更时间线**: Git 部署/配置变更关联到 Issue
- **维护窗口**: 按服务/规则/时间段静默告警
- **Runbook 一键排查**: Issue 关联排查按钮
- **父子 Incident**: 相关 Issue 聚合为一个事件
- **配置体检**: 启动前检测所有连接和依赖
- **基线学习**: 24h 自动校准正常范围
- **AI 模型热切换**: 面板上选模型，不用重启
- **多模型路由**: 不同任务用不同模型（分析/聊天/调查）
- **自定义 AI**: 支持 DeepSeek/通义千问/Ollama/任意 OpenAI 兼容 API
- **D3.js 可视化**: 拓扑图（粒子动画）、火焰图、瀑布时间线
- **AI Terminal**: 交互式服务器端 AI 助手
- **中文面板**: 全中文界面，20+ 功能区块

### 修复
- Collector 连接失败返回 alive=0（不再丢失）
- Dashboard 全 API 鉴权（不只是 manage 路由）
- AI client 并发安全（per-request system prompt）
- 健康检查中国网络兼容（多 DNS 探测）
- Issue 状态机: IMPROVING 能正常 resolve，DATA_GAP 能回到 OPEN
- CRITICAL/FATAL 首轮立即通知（跳过 DETECTING）
- Issue 指纹包含 labels（多实体不合并）
- 基线/时序按 name+labels 分桶
- 纯 Go SQLite（零 CGO，单二进制）
- 前端 token 自动透传

### 部署
- 一键安装脚本（交互式配置向导）
- Docker + docker-compose
- GitHub Actions CI/CD 自动发版
- systemd 服务自动配置

## v0.1.0 (2026-04-08)

### 初始版本
- L0 项目扫描（AST 分析 + AI 生成配置）
- L1 指标采集（Redis/MySQL/HTTP/Log）
- L1.5 SDK 代码追踪（窗口采样）
- L2 AI 分析（Claude/OpenAI）
- Issue 生命周期管理（9 状态 + 抗抖动）
- Dashboard Web 面板
- Bark/Telegram/Console 告警
- YAML 配置驱动
