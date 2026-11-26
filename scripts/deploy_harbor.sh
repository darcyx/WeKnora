#!/usr/bin/env bash
# WeKnora 线上 Linux Harbor 部署与回滚脚本（基于 docker-compose.yml + docker-compose.harbor.yml）
set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

HARBOR_REGISTRY="${HARBOR_REGISTRY:-harbor.dobest.com}"
HARBOR_PROJECT="${HARBOR_PROJECT:-sdk}"
STATE_FILE="${PROJECT_ROOT}/.weknora-harbor-last-release"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-120}"

TARGET_TAG=""
DO_ROLLBACK=false
SHOW_STATUS=false
DRY_RUN=false
WAIT_HEALTH=true
DEPLOY_APP=false
DEPLOY_FRONTEND=false

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }

show_help() {
    cat <<EOF
WeKnora 线上 Linux Harbor 部署脚本

用法:
  $0 [选项] [tag]

常用操作:
  -t, --tag <tag>    升级到指定镜像 tag（默认读取环境变量 WEKNORA_VERSION，否则 main）
  --app              仅更新 app 服务
  --frontend         仅更新 frontend 服务
  --all              更新 app + frontend（默认）
  --rollback         回滚到上次发布前记录的镜像版本
  --status           查看当前线上 app / frontend 运行镜像与健康状态
  --no-wait          升级后不等待 app 健康检查
  --dry-run          仅打印将执行的 compose 命令，不实际变更容器
  -h, --help         显示帮助

示例:
  $0 --tag v0.9.1-23d7b71e5       # 拉取并平滑升级 app + frontend
  $0 --app --tag v0.9.1-23d7b71e5 # 仅升级 app
  $0 --rollback                   # 回滚到升级前版本
  $0 --status                     # 查看当前运行镜像版本
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        -t|--tag)
            [ $# -ge 2 ] || { log_error "--tag 缺少参数"; exit 1; }
            TARGET_TAG="$2"
            shift 2
            ;;
        --app)
            DEPLOY_APP=true
            shift
            ;;
        --frontend|--ui)
            DEPLOY_FRONTEND=true
            shift
            ;;
        --all)
            DEPLOY_APP=true
            DEPLOY_FRONTEND=true
            shift
            ;;
        --rollback)
            DO_ROLLBACK=true
            shift
            ;;
        --status)
            SHOW_STATUS=true
            shift
            ;;
        --no-wait)
            WAIT_HEALTH=false
            shift
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        -*)
            log_error "未知参数: $1（使用 --help 查看用法）"
            exit 1
            ;;
        *)
            if [ -z "$TARGET_TAG" ]; then
                TARGET_TAG="$1"
                shift
            else
                log_error "多余的位置参数: $1"
                exit 1
            fi
            ;;
    esac
done

if [ "$DEPLOY_APP" = false ] && [ "$DEPLOY_FRONTEND" = false ]; then
    DEPLOY_APP=true
    DEPLOY_FRONTEND=true
fi

cd "$PROJECT_ROOT"

# 自动兼容 docker compose v2 与 docker-compose v1
COMPOSE_CMD=()
if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD=(docker-compose)
else
    log_error "未检测到 docker compose 或 docker-compose"
    exit 1
fi

COMPOSE_FILES=(-f "$PROJECT_ROOT/docker-compose.yml" -f "$PROJECT_ROOT/docker-compose.harbor.yml")

get_container_image() {
    local container="$1"
    docker inspect --format '{{.Config.Image}}' "$container" 2>/dev/null || true
}

get_container_tag() {
    local image="$1"
    if [ -z "$image" ]; then
        echo ""
        return
    fi
    # 提取最后一个冒号后的 tag（排除带端口的 registry 且无 tag 的情况）
    local last_part="${image##*/}"
    if [[ "$last_part" == *:* ]]; then
        echo "${last_part##*:}"
    else
        echo "latest"
    fi
}

get_container_health() {
    local container="$1"
    docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || echo "not_running"
}

show_status() {
    echo "================ 当前运行状态 ================"
    printf "%-18s %-14s %s\n" "CONTAINER" "STATUS" "IMAGE"
    for c in WeKnora-app WeKnora-frontend; do
        printf "%-18s %-14s %s\n" "$c" "$(get_container_health "$c")" "$(get_container_image "$c" || echo '-')"
    done
    if [ -f "$STATE_FILE" ]; then
        echo "----------------------------------------------"
        echo "已记录的上次发布版本 (${STATE_FILE}):"
        cat "$STATE_FILE"
    fi
    echo "=============================================="
}

if [ "$SHOW_STATUS" = true ]; then
    show_status
    exit 0
fi

# 检查 .env 是否存在（docker-compose.yml 中 app 依赖 env_file: .env）
if [ ! -f "$PROJECT_ROOT/.env" ]; then
    log_error "缺少 ${PROJECT_ROOT}/.env 文件，请先准备生产环境变量后再部署（可参考 .env.example）"
    exit 1
fi

# 构造服务列表
SERVICES=()
[ "$DEPLOY_APP" = true ] && SERVICES+=(app)
[ "$DEPLOY_FRONTEND" = true ] && SERVICES+=(frontend)

APP_TAG=""
UI_TAG=""

if [ "$DO_ROLLBACK" = true ]; then
    if [ ! -f "$STATE_FILE" ]; then
        log_error "未找到上次发布记录 (${STATE_FILE})，请使用 --tag <旧版本> 手动指定回滚版本"
        exit 1
    fi
    # shellcheck disable=SC1090
    source "$STATE_FILE"
    APP_TAG="${PREV_APP_TAG:-}"
    UI_TAG="${PREV_UI_TAG:-}"
    if [ "$DEPLOY_APP" = true ] && [ -z "$APP_TAG" ]; then
        log_error "回滚记录中缺少 PREV_APP_TAG"
        exit 1
    fi
    if [ "$DEPLOY_FRONTEND" = true ] && [ -z "$UI_TAG" ]; then
        log_error "回滚记录中缺少 PREV_UI_TAG"
        exit 1
    fi
    log_warn "准备回滚 -> app:${APP_TAG:-保持不变} frontend:${UI_TAG:-保持不变}"
else
    if [ -z "$TARGET_TAG" ]; then
        TARGET_TAG="${WEKNORA_VERSION:-main}"
    fi
    APP_TAG="$TARGET_TAG"
    UI_TAG="$TARGET_TAG"

    # 升级前记录当前运行的 tag，便于一键回滚
    if [ "$DRY_RUN" = false ]; then
        cur_app_img="$(get_container_image WeKnora-app)"
        cur_ui_img="$(get_container_image WeKnora-frontend)"
        cur_app_tag="$(get_container_tag "$cur_app_img")"
        cur_ui_tag="$(get_container_tag "$cur_ui_img")"
        if [ -n "$cur_app_tag" ] || [ -n "$cur_ui_tag" ]; then
            cat > "$STATE_FILE" <<EOF
# Recorded before deploy at $(date -u '+%Y-%m-%d %H:%M:%S UTC')
PREV_APP_IMAGE="${cur_app_img}"
PREV_APP_TAG="${cur_app_tag}"
PREV_UI_IMAGE="${cur_ui_img}"
PREV_UI_TAG="${cur_ui_tag}"
EOF
            log_info "已记录当前运行版本到 ${STATE_FILE}（app=${cur_app_tag:-none}, frontend=${cur_ui_tag:-none}）"
        fi
    fi
fi

export HARBOR_REGISTRY HARBOR_PROJECT
export WEKNORA_VERSION="${TARGET_TAG:-${APP_TAG:-latest}}"
[ -n "$APP_TAG" ] && export WEKNORA_APP_VERSION="$APP_TAG"
[ -n "$UI_TAG" ] && export WEKNORA_UI_VERSION="$UI_TAG"

log_info "目标镜像："
[ "$DEPLOY_APP" = true ] && log_info "  app      -> ${HARBOR_REGISTRY}/${HARBOR_PROJECT}/weknora-app:${WEKNORA_APP_VERSION}"
[ "$DEPLOY_FRONTEND" = true ] && log_info "  frontend -> ${HARBOR_REGISTRY}/${HARBOR_PROJECT}/weknora-ui:${WEKNORA_UI_VERSION}"

run_compose() {
    if [ "$DRY_RUN" = true ]; then
        printf '[DRY-RUN] HARBOR_REGISTRY=%q HARBOR_PROJECT=%q WEKNORA_APP_VERSION=%q WEKNORA_UI_VERSION=%q' \
            "$HARBOR_REGISTRY" "$HARBOR_PROJECT" "${WEKNORA_APP_VERSION:-}" "${WEKNORA_UI_VERSION:-}"
        for arg in "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" "$@"; do
            printf ' %q' "$arg"
        done
        printf '\n'
    else
        "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" "$@"
    fi
}

# 1. 先拉取镜像，拉取失败不触碰线上运行中的容器
log_info "拉取 Harbor 镜像..."
run_compose pull "${SERVICES[@]}"

# 2. 使用 --no-build 启动/重建目标服务，严禁在线上机器触发源码编译
log_info "更新并启动服务: ${SERVICES[*]}..."
run_compose up -d --no-build "${SERVICES[@]}"

# 3. 等待 app 健康检查通过
if [ "$DRY_RUN" = false ] && [ "$WAIT_HEALTH" = true ] && [ "$DEPLOY_APP" = true ]; then
    log_info "等待 WeKnora-app 健康检查通过（超时 ${HEALTH_TIMEOUT}s）..."
    elapsed=0
    while [ "$elapsed" -lt "$HEALTH_TIMEOUT" ]; do
        status="$(get_container_health WeKnora-app)"
        if [ "$status" = "healthy" ]; then
            log_success "WeKnora-app 健康检查通过！"
            break
        fi
        if [ "$status" = "unhealthy" ] || [ "$status" = "exited" ] || [ "$status" = "dead" ]; then
            log_error "WeKnora-app 状态异常: ${status}，请执行 docker logs --tail 100 WeKnora-app 排查，或执行 $0 --rollback 回滚"
            exit 1
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    if [ "$elapsed" -ge "$HEALTH_TIMEOUT" ]; then
        log_warn "等待 ${HEALTH_TIMEOUT}s 后 WeKnora-app 仍为 $(get_container_health WeKnora-app)，请手动检查容器日志"
    fi
fi

if [ "$DRY_RUN" = false ]; then
    show_status
    log_success "部署完成！"
fi
