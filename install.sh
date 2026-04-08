#!/bin/bash
#
# AI Ops Agent — One-Line Installer & Manager
#
# Usage:
#   curl -sS https://raw.githubusercontent.com/aiopsagent/ai-ops-agent/main/install.sh | bash
#
# Or download and run:
#   curl -sS -O https://raw.githubusercontent.com/aiopsagent/ai-ops-agent/main/install.sh
#   chmod +x install.sh && ./install.sh

set -e

# ============================================================
# Configuration
# ============================================================
REPO="hsdnh/ai-ops-agent"
INSTALL_DIR="/opt/ai-ops-agent"
DATA_DIR="/opt/ai-ops-agent/data"
CONFIG_FILE="/opt/ai-ops-agent/config.yaml"
SERVICE_NAME="ai-ops-agent"
DASHBOARD_PORT="9090"
GITHUB_RAW="https://raw.githubusercontent.com/${REPO}/main"
GITHUB_RELEASE="https://github.com/${REPO}/releases/latest/download"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
WHITE='\033[1;37m'
NC='\033[0m' # No Color
BOLD='\033[1m'
DIM='\033[2m'

# ============================================================
# Utility Functions
# ============================================================
print_logo() {
    echo -e "${PURPLE}"
    echo "    _    ___    ___                _                    _   "
    echo "   / \  |_ _|  / _ \ _ __  ___   / \   __ _  ___ _ __ | |_ "
    echo "  / _ \  | |  | | | | '_ \/ __| / _ \ / _\` |/ _ \ '_ \| __|"
    echo " / ___ \ | |  | |_| | |_) \__ \/ ___ \ (_| |  __/ | | | |_ "
    echo "/_/   \_\___|  \___/| .__/|___/_/   \_\__, |\___|_| |_|\__|"
    echo "                    |_|               |___/                 "
    echo -e "${NC}"
    echo -e "${DIM}Production Runtime Intelligent Monitoring & Self-Healing Framework${NC}"
    echo ""
}

print_sep() {
    echo -e "${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

print_status() {
    echo -e "${GREEN}[OK]${NC} $1"
}

print_warn() {
    echo -e "${YELLOW}[!]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_info() {
    echo -e "${CYAN}[i]${NC} $1"
}

detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS=$ID
        OS_VERSION=$VERSION_ID
    elif [ -f /etc/debian_version ]; then
        OS="debian"
    elif [ -f /etc/redhat-release ]; then
        OS="centos"
    else
        OS="unknown"
    fi

    ARCH=$(uname -m)
    case $ARCH in
        x86_64)  ARCH="amd64" ;;
        aarch64) ARCH="arm64" ;;
        armv7l)  ARCH="arm" ;;
    esac
}

check_root() {
    if [ "$(id -u)" != "0" ]; then
        print_error "This script requires root privileges. Run with sudo."
        exit 1
    fi
}

# ============================================================
# Installation Functions
# ============================================================
install_agent() {
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} Installing AI Ops Agent${NC}"
    print_sep
    echo ""

    detect_os
    print_info "OS: $OS $OS_VERSION | Arch: $ARCH"

    # ── Interactive Setup ──────────────────────────────────
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} 配置向导${NC}"
    print_sep
    echo ""

    # Dashboard access mode
    echo -e "  ${CYAN}面板访问方式:${NC}"
    echo -e "  1. 仅本机访问 (127.0.0.1:${DASHBOARD_PORT}) — 安全，需 SSH 隧道"
    echo -e "  2. 远程访问 (0.0.0.0:${DASHBOARD_PORT}) — 方便，需设置密码"
    read -p "  选择 [1/2] (默认 2): " access_mode
    access_mode=${access_mode:-2}

    DASHBOARD_BIND="127.0.0.1:${DASHBOARD_PORT}"
    DASHBOARD_TOKEN=""

    if [ "$access_mode" = "2" ]; then
        DASHBOARD_BIND="0.0.0.0:${DASHBOARD_PORT}"
        # Generate random token or let user set
        DEFAULT_TOKEN=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 16)
        echo ""
        echo -e "  ${YELLOW}远程访问需要设置访问密码 (token)${NC}"
        read -p "  输入密码 (回车使用随机密码 ${DEFAULT_TOKEN}): " user_token
        DASHBOARD_TOKEN=${user_token:-$DEFAULT_TOKEN}
        echo -e "  ${GREEN}密码设置为: ${WHITE}${DASHBOARD_TOKEN}${NC}"
    fi

    # AI model selection
    echo ""
    echo -e "  ${CYAN}AI 模型 (用于智能分析):${NC}"
    echo -e "  1. Claude Opus 4 (最强推荐)"
    echo -e "  2. Claude Sonnet 4 (性价比)"
    echo -e "  3. GPT-4o"
    echo -e "  4. DeepSeek V3 (国内推荐)"
    echo -e "  5. 通义千问"
    echo -e "  6. Ollama 本地 (免费)"
    echo -e "  7. 暂不配置 AI"
    read -p "  选择 [1-7] (默认 7): " ai_choice
    ai_choice=${ai_choice:-7}

    AI_PROVIDER=""
    AI_MODEL=""
    AI_BASEURL=""
    AI_KEY=""
    AI_ENABLED="false"

    case $ai_choice in
        1) AI_PROVIDER="claude"; AI_MODEL="claude-opus-4-20250514"; AI_ENABLED="true"
           read -p "  输入 Claude API Key: " AI_KEY ;;
        2) AI_PROVIDER="claude"; AI_MODEL="claude-sonnet-4-20250514"; AI_ENABLED="true"
           read -p "  输入 Claude API Key: " AI_KEY ;;
        3) AI_PROVIDER="openai"; AI_MODEL="gpt-4o"; AI_ENABLED="true"
           read -p "  输入 OpenAI API Key: " AI_KEY ;;
        4) AI_PROVIDER="openai_compatible"; AI_MODEL="deepseek-chat"; AI_BASEURL="https://api.deepseek.com/v1"; AI_ENABLED="true"
           read -p "  输入 DeepSeek API Key: " AI_KEY ;;
        5) AI_PROVIDER="openai_compatible"; AI_MODEL="qwen-max"; AI_BASEURL="https://dashscope.aliyuncs.com/compatible-mode/v1"; AI_ENABLED="true"
           read -p "  输入通义千问 API Key: " AI_KEY ;;
        6) AI_PROVIDER="openai_compatible"; AI_MODEL="llama3"; AI_BASEURL="http://localhost:11434/v1"; AI_ENABLED="true" ;;
        7) print_info "跳过 AI 配置 (可以之后在面板里设置)" ;;
    esac

    # Cluster mode
    echo ""
    echo -e "  ${CYAN}运行模式:${NC}"
    echo -e "  1. 单机模式 (默认)"
    echo -e "  2. 主节点 (接收子服务器汇报)"
    echo -e "  3. 子节点 (向主节点汇报)"
    read -p "  选择 [1-3] (默认 1): " cluster_mode
    cluster_mode=${cluster_mode:-1}

    AGENT_MODE="standalone"
    MASTER_URL=""
    NODE_NAME=""
    CLUSTER_TOKEN=""

    case $cluster_mode in
        2) AGENT_MODE="master"
           CLUSTER_TOKEN=$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 16)
           echo -e "  ${GREEN}集群通信密码: ${WHITE}${CLUSTER_TOKEN}${NC}"
           echo -e "  ${YELLOW}子节点安装时需要这个密码${NC}" ;;
        3) AGENT_MODE="worker"
           read -p "  主节点地址 (例如 http://10.0.0.1:9090): " MASTER_URL
           read -p "  本节点名称 (例如 worker-1): " NODE_NAME
           read -p "  集群通信密码: " CLUSTER_TOKEN ;;
    esac

    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} 开始安装...${NC}"
    print_sep
    echo ""

    # ── Download & Install ──────────────────────────────────
    mkdir -p "$INSTALL_DIR" "$DATA_DIR"
    print_status "Created directories"

    echo ""
    print_info "Downloading latest release..."
    BINARY_NAME="ai-ops-agent-linux-${ARCH}"
    if curl -fsSL "${GITHUB_RELEASE}/${BINARY_NAME}" -o "${INSTALL_DIR}/ai-ops-agent" 2>/dev/null; then
        chmod +x "${INSTALL_DIR}/ai-ops-agent"
        print_status "Binary downloaded: ${INSTALL_DIR}/ai-ops-agent"
    else
        print_warn "Pre-built binary not found. Building from source..."
        install_from_source
    fi

    if curl -fsSL "${GITHUB_RELEASE}/ai-ops-agent-instrument-linux-${ARCH}" -o "${INSTALL_DIR}/ai-ops-agent-instrument" 2>/dev/null; then
        chmod +x "${INSTALL_DIR}/ai-ops-agent-instrument"
        print_status "Instrument tool downloaded"
    fi

    ln -sf "${INSTALL_DIR}/ai-ops-agent" /usr/local/bin/ai-ops-agent
    ln -sf "${INSTALL_DIR}/ai-ops-agent-instrument" /usr/local/bin/ai-ops-agent-instrument 2>/dev/null
    print_status "Symlinks created"

    # Generate config with user's choices
    if [ ! -f "$CONFIG_FILE" ]; then
        generate_config
    else
        print_warn "Config exists: $CONFIG_FILE (not overwritten)"
    fi

    # Create systemd service with user's choices
    create_service

    # Save env file for tokens
    cat > "${INSTALL_DIR}/.env" << ENVEOF
AIOPS_DASHBOARD_TOKEN=${DASHBOARD_TOKEN}
AIOPS_CLUSTER_TOKEN=${CLUSTER_TOKEN}
ENVEOF
    chmod 600 "${INSTALL_DIR}/.env"
    print_status "Tokens saved to ${INSTALL_DIR}/.env"

    echo ""
    print_sep
    echo -e "${GREEN}${BOLD} 安装完成!${NC}"
    print_sep
    echo ""
    SERVER_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost')
    echo -e "  配置文件:  ${WHITE}${CONFIG_FILE}${NC}"
    echo -e "  数据目录:  ${WHITE}${DATA_DIR}${NC}"

    if [ "$access_mode" = "2" ]; then
        echo -e "  面板地址:  ${WHITE}http://${SERVER_IP}:${DASHBOARD_PORT}?token=${DASHBOARD_TOKEN}${NC}"
        echo -e "  访问密码:  ${WHITE}${DASHBOARD_TOKEN}${NC}"
    else
        echo -e "  面板地址:  ${WHITE}http://127.0.0.1:${DASHBOARD_PORT}${NC} (需 SSH 隧道)"
    fi

    if [ "$AI_ENABLED" = "true" ]; then
        echo -e "  AI 模型:   ${WHITE}${AI_PROVIDER}/${AI_MODEL}${NC}"
    fi

    if [ "$AGENT_MODE" != "standalone" ]; then
        echo -e "  集群模式:  ${WHITE}${AGENT_MODE}${NC}"
        echo -e "  集群密码:  ${WHITE}${CLUSTER_TOKEN}${NC}"
    fi

    echo ""
    echo -e "  ${YELLOW}下一步:${NC}"
    echo -e "  1. 检查配置:  ${CYAN}nano ${CONFIG_FILE}${NC}"
    echo -e "  2. 启动:      ${CYAN}systemctl start ${SERVICE_NAME}${NC}"

    if [ "$access_mode" = "2" ]; then
        echo -e "  3. 打开浏览器: ${CYAN}http://${SERVER_IP}:${DASHBOARD_PORT}?token=${DASHBOARD_TOKEN}${NC}"
    fi
    echo ""
}

install_from_source() {
    # Check Go
    if ! command -v go &> /dev/null; then
        print_info "Installing Go..."
        curl -fsSL https://go.dev/dl/go1.22.5.linux-${ARCH}.tar.gz | tar -C /usr/local -xzf -
        export PATH=$PATH:/usr/local/go/bin
        print_status "Go installed"
    fi

    # Clone and build
    print_info "Cloning repository..."
    TMP_DIR=$(mktemp -d)
    git clone --depth 1 "https://github.com/${REPO}.git" "$TMP_DIR" 2>/dev/null
    cd "$TMP_DIR"

    print_info "Building..."
    CGO_ENABLED=0 go build -o "${INSTALL_DIR}/ai-ops-agent" ./cmd/agent/
    CGO_ENABLED=0 go build -o "${INSTALL_DIR}/ai-ops-agent-instrument" ./cmd/instrument/
    print_status "Built from source"

    rm -rf "$TMP_DIR"
}

generate_config() {
    cat > "$CONFIG_FILE" << YAML
# AI Ops Agent 配置文件
# 安装时间: $(date '+%Y-%m-%d %H:%M:%S')

project: my-project
schedule: "*/30 * * * *"

collectors:
  # Redis 监控 (取消注释并填写地址)
  # redis:
  #   - addr: "127.0.0.1:6379"
  #     password: ""
  #     checks:
  #       - key_pattern: "queue:*:pending"
  #         threshold: 1000
  #         alert: "队列堆积"

  # MySQL 监控
  # mysql:
  #   - dsn: "user:pass@tcp(127.0.0.1:3306)/dbname"
  #     checks:
  #       - query: "SELECT COUNT(*) FROM orders WHERE status='pending'"
  #         name: "pending_orders"
  #         threshold: 100
  #         alert: "订单堆积"

  # HTTP 健康检查
  # http:
  #   - url: "http://localhost:8080/health"

  # 日志监控
  # log:
  #   - source: file
  #     file_path: "/var/log/app.log"
  #     error_patterns: ["error", "panic", "fatal"]
  #     minutes: 30

storage:
  enabled: true
  path: "${DATA_DIR}/aiops.db"

rules: []

ai:
  enabled: ${AI_ENABLED}
  provider: "${AI_PROVIDER}"
  api_key: "${AI_KEY}"
  model: "${AI_MODEL}"
  base_url: "${AI_BASEURL}"

alerts:
  console: {}
  # bark:
  #   keys: ["your-bark-key"]
  # telegram:
  #   token: "your-bot-token"
  #   chat_ids: [123456789]
YAML
    print_status "Config created: $CONFIG_FILE"
}

create_service() {
    # Build ExecStart command based on user choices
    EXEC_CMD="${INSTALL_DIR}/ai-ops-agent -config ${CONFIG_FILE} -dashboard ${DASHBOARD_BIND}"
    if [ "$AGENT_MODE" = "worker" ]; then
        EXEC_CMD="$EXEC_CMD -mode worker -master ${MASTER_URL} -node ${NODE_NAME}"
    elif [ "$AGENT_MODE" = "master" ]; then
        EXEC_CMD="$EXEC_CMD -mode master"
    fi

    cat > /etc/systemd/system/${SERVICE_NAME}.service << EOF
[Unit]
Description=AI Ops Agent - 智能监控平台
After=network.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-${INSTALL_DIR}/.env
ExecStart=${EXEC_CMD}
WorkingDirectory=${INSTALL_DIR}
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable ${SERVICE_NAME} 2>/dev/null
    print_status "Systemd service created"
}

# ============================================================
# Management Functions
# ============================================================
start_agent() {
    systemctl start ${SERVICE_NAME}
    sleep 1
    if systemctl is-active --quiet ${SERVICE_NAME}; then
        print_status "Agent started"
        echo -e "  Dashboard: ${CYAN}http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):${DASHBOARD_PORT}${NC}"
    else
        print_error "Failed to start. Check: journalctl -u ${SERVICE_NAME} -f"
    fi
}

stop_agent() {
    systemctl stop ${SERVICE_NAME}
    print_status "Agent stopped"
}

restart_agent() {
    systemctl restart ${SERVICE_NAME}
    sleep 1
    if systemctl is-active --quiet ${SERVICE_NAME}; then
        print_status "Agent restarted"
    else
        print_error "Failed to restart. Check: journalctl -u ${SERVICE_NAME} -f"
    fi
}

show_status() {
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} Agent Status${NC}"
    print_sep
    echo ""

    if systemctl is-active --quiet ${SERVICE_NAME} 2>/dev/null; then
        echo -e "  Status:    ${GREEN}Running${NC}"
        PID=$(systemctl show -p MainPID ${SERVICE_NAME} | cut -d= -f2)
        echo -e "  PID:       ${WHITE}${PID}${NC}"
        UPTIME=$(systemctl show -p ActiveEnterTimestamp ${SERVICE_NAME} | cut -d= -f2)
        echo -e "  Since:     ${WHITE}${UPTIME}${NC}"
        MEM=$(ps -o rss= -p $PID 2>/dev/null | awk '{printf "%.1f MB", $1/1024}')
        echo -e "  Memory:    ${WHITE}${MEM}${NC}"
    else
        echo -e "  Status:    ${RED}Stopped${NC}"
    fi

    echo ""
    echo -e "  Config:    ${WHITE}${CONFIG_FILE}${NC}"
    echo -e "  Data:      ${WHITE}${DATA_DIR}${NC}"
    echo -e "  Dashboard: ${WHITE}http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):${DASHBOARD_PORT}${NC}"

    if [ -d "$DATA_DIR" ]; then
        SIZE=$(du -sh "$DATA_DIR" 2>/dev/null | cut -f1)
        echo -e "  Data size: ${WHITE}${SIZE}${NC}"
    fi
    echo ""
}

show_logs() {
    echo -e "${CYAN}Press Ctrl+C to exit${NC}"
    journalctl -u ${SERVICE_NAME} -f --no-pager
}

edit_config() {
    if command -v nano &>/dev/null; then
        nano "$CONFIG_FILE"
    elif command -v vi &>/dev/null; then
        vi "$CONFIG_FILE"
    else
        print_error "No editor found. Manually edit: $CONFIG_FILE"
    fi
}

init_project() {
    echo ""
    echo -e "${BOLD}${WHITE} L0 Project Scanner${NC}"
    echo ""
    read -p "  Project name: " proj_name
    read -p "  Source code path: " src_path
    read -p "  Redis address (empty to skip): " redis_addr
    read -p "  MySQL DSN (empty to skip): " mysql_dsn
    read -p "  Claude API key (empty to skip AI): " api_key

    echo ""
    print_info "Scanning project..."

    CMD="${INSTALL_DIR}/ai-ops-agent-init --project ${proj_name} --source ${src_path}"
    [ -n "$redis_addr" ] && CMD="$CMD --redis $redis_addr"
    [ -n "$mysql_dsn" ] && CMD="$CMD --mysql '$mysql_dsn'"
    [ -n "$api_key" ] && CMD="$CMD --api-key $api_key"
    CMD="$CMD --output ${CONFIG_FILE}"

    eval $CMD

    echo ""
    print_status "Config generated: $CONFIG_FILE"
    print_info "Review and adjust, then start the agent."
}

install_probes() {
    echo ""
    read -p "  Target project path: " target_path
    if [ -z "$target_path" ]; then
        print_error "Path required"
        return
    fi
    print_info "Installing SDK probes..."
    ${INSTALL_DIR}/ai-ops-agent-instrument "$target_path/..."
    echo ""
    print_status "Probes installed. Remember to add to main.go:"
    echo -e "  ${CYAN}import _ \"github.com/aiopsagent/ai-ops-agent/sdk/autotrace\"${NC}"
}

strip_probes() {
    echo ""
    read -p "  Target project path: " target_path
    if [ -z "$target_path" ]; then
        print_error "Path required"
        return
    fi
    print_info "Stripping SDK probes..."
    ${INSTALL_DIR}/ai-ops-agent-instrument -strip "$target_path/..."
    print_status "Probes removed. Source code restored."
}

uninstall_agent() {
    echo ""
    echo -e "${RED}${BOLD} This will completely remove AI Ops Agent${NC}"
    echo ""
    read -p "  Also strip SDK probes from source? (y/N): " strip_src
    read -p "  Type 'yes' to confirm uninstall: " confirm

    if [ "$confirm" != "yes" ]; then
        print_info "Aborted."
        return
    fi

    # Stop service
    systemctl stop ${SERVICE_NAME} 2>/dev/null
    systemctl disable ${SERVICE_NAME} 2>/dev/null
    rm -f /etc/systemd/system/${SERVICE_NAME}.service
    systemctl daemon-reload
    print_status "Service removed"

    # Strip probes
    if [ "$strip_src" = "y" ] || [ "$strip_src" = "Y" ]; then
        read -p "  Source code path: " src_path
        if [ -n "$src_path" ]; then
            ${INSTALL_DIR}/ai-ops-agent-instrument -strip "$src_path/..." 2>/dev/null
            print_status "Probes stripped"
        fi
    fi

    # Remove files
    rm -f /usr/local/bin/ai-ops-agent
    rm -f /usr/local/bin/ai-ops-agent-instrument
    rm -rf "$INSTALL_DIR"
    print_status "Files removed"

    echo ""
    print_status "AI Ops Agent completely uninstalled."
}

update_agent() {
    print_info "Updating AI Ops Agent..."
    systemctl stop ${SERVICE_NAME} 2>/dev/null

    detect_os
    BINARY_NAME="ai-ops-agent-linux-${ARCH}"
    if curl -fsSL "${GITHUB_RELEASE}/${BINARY_NAME}" -o "${INSTALL_DIR}/ai-ops-agent.new" 2>/dev/null; then
        mv "${INSTALL_DIR}/ai-ops-agent.new" "${INSTALL_DIR}/ai-ops-agent"
        chmod +x "${INSTALL_DIR}/ai-ops-agent"
        print_status "Binary updated"
    else
        print_warn "Download failed. Trying build from source..."
        install_from_source
    fi

    systemctl start ${SERVICE_NAME}
    print_status "Agent updated and restarted"
}

# ============================================================
# Main Menu
# ============================================================
show_menu() {
    clear
    print_logo

    # Quick status line
    if systemctl is-active --quiet ${SERVICE_NAME} 2>/dev/null; then
        echo -e "  Status: ${GREEN}● Running${NC}    Dashboard: ${CYAN}http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):${DASHBOARD_PORT}${NC}"
    else
        echo -e "  Status: ${RED}● Stopped${NC}"
    fi
    echo ""

    print_sep
    echo -e "${BOLD}${WHITE} 快速开始${NC}"
    print_sep
    echo -e "  ${GREEN}1${NC}.  安装 AI Ops Agent"
    echo -e "  ${GREEN}2${NC}.  扫描项目 (自动生成监控配置)"
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} 服务控制${NC}"
    print_sep
    echo -e "  ${GREEN}3${NC}.  启动"
    echo -e "  ${GREEN}4${NC}.  停止"
    echo -e "  ${GREEN}5${NC}.  重启"
    echo -e "  ${GREEN}6${NC}.  查看状态"
    echo -e "  ${GREEN}7${NC}.  查看日志 (实时)"
    echo -e "  ${GREEN}8${NC}.  编辑配置"
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} SDK 探针管理${NC}"
    print_sep
    echo -e "  ${GREEN}9${NC}.  安装探针 (深度代码追踪)"
    echo -e "  ${GREEN}10${NC}. 卸载探针 (还原源码)"
    echo ""
    print_sep
    echo -e "${BOLD}${WHITE} 维护${NC}"
    print_sep
    echo -e "  ${GREEN}11${NC}. 更新版本"
    echo -e "  ${RED}12${NC}. 完全卸载"
    echo ""
    print_sep
    echo -e "  ${GREEN}0${NC}.  退出"
    print_sep
    echo ""
}

# ============================================================
# Entry Point
# ============================================================
main() {
    check_root
    detect_os

    # If run with arguments, execute directly
    case "${1:-}" in
        install)    install_agent; exit 0 ;;
        start)      start_agent; exit 0 ;;
        stop)       stop_agent; exit 0 ;;
        restart)    restart_agent; exit 0 ;;
        status)     show_status; exit 0 ;;
        logs)       show_logs; exit 0 ;;
        update)     update_agent; exit 0 ;;
        uninstall)  uninstall_agent; exit 0 ;;
    esac

    # Interactive menu
    while true; do
        show_menu
        read -p "  Enter selection [0-12]: " choice
        echo ""

        case $choice in
            1)  install_agent ;;
            2)  init_project ;;
            3)  start_agent ;;
            4)  stop_agent ;;
            5)  restart_agent ;;
            6)  show_status ;;
            7)  show_logs ;;
            8)  edit_config ;;
            9)  install_probes ;;
            10) strip_probes ;;
            11) update_agent ;;
            12) uninstall_agent ;;
            0)  echo -e "${GREEN}Bye!${NC}"; exit 0 ;;
            *)  print_error "Invalid option" ;;
        esac

        echo ""
        read -p "  Press Enter to continue..." _
    done
}

main "$@"
