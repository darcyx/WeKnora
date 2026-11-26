#!/usr/bin/env bash
# WeKnora 镜像构建并推送至 Harbor（支持 macOS 交叉编译 linux/amd64 与 Linux 原生构建）
set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

export DOCKER_BUILDKIT=1

# 默认配置（均可通过环境变量或命令行参数覆盖）
HARBOR_REGISTRY="${HARBOR_REGISTRY:-harbor.dobest.com}"
HARBOR_PROJECT="${HARBOR_PROJECT:-sdk}"
APP_IMAGE_NAME="${APP_IMAGE_NAME:-weknora-app}"
APP_BASE_IMAGE_NAME="${APP_BASE_IMAGE_NAME:-weknora-app-base}"
APP_BASE_TAG="${APP_BASE_TAG:-v1}"
UI_IMAGE_NAME="${UI_IMAGE_NAME:-weknora-ui}"
PLATFORM="${PLATFORM:-linux/amd64}"
BUILDER_NAME="${HARBOR_BUILDER_NAME:-weknora-harbor-builder}"
CUSTOM_BUILDER="${HARBOR_BUILDER:-}"
APK_MIRROR_DEFAULT="${APK_MIRROR_ARG-mirrors.aliyun.com}"
NPM_REGISTRY_DEFAULT="${NPM_REGISTRY-https://registry.npmmirror.com}"
PIP_INDEX_URL_DEFAULT="${PIP_INDEX_URL-https://mirrors.tencent.com/pypi/simple}"
RUSTUP_DIST_SERVER_DEFAULT="${RUSTUP_DIST_SERVER-https://rsproxy.cn}"
RUSTUP_UPDATE_ROOT_DEFAULT="${RUSTUP_UPDATE_ROOT-https://rsproxy.cn/rustup}"
CARGO_MIRROR_DEFAULT="${CARGO_MIRROR_ARG-sparse+https://rsproxy.cn/index/}"

BUILD_APP=false
BUILD_APP_BASE=false
BUILD_FRONTEND=false
CUSTOM_TAG="${HARBOR_TAG:-main}"
PUSH_LATEST=false
DO_PUSH=true
DRY_RUN=false
NO_CACHE=false
USE_REGISTRY_CACHE=false
WITH_ANYDOC="${WITH_ANYDOC:-1}"
WITH_BROWSERSKILL="${WITH_BROWSERSKILL:-1}"
BROWSERSKILL_IMAGE="${BROWSERSKILL_IMAGE:-}"
USE_PREBUILT_BASE="${USE_PREBUILT_BASE:-1}"
APP_BASE_IMAGE="${APP_BASE_IMAGE:-}"

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }

show_help() {
    cat <<EOF
WeKnora Harbor 构建推送脚本（支持 macOS / Linux 构建 linux 镜像）

用法:
  $0 [选项]

服务选择（默认同时构建 app + frontend）:
  --all                   构建并推送 app + frontend（默认）
  --app                   仅构建并推送 weknora-app（默认复用 weknora-app-base 基础镜像）
  --app-base              构建并推送 weknora-app-base 运行环境基础镜像（apt/python/node/uv/browserskill）
  --frontend              仅构建并推送 weknora-ui

镜像与平台选项:
  -t, --tag <tag>         指定镜像标签（默认: main）
  --base-tag <tag>        指定基础镜像 weknora-app-base 标签（默认: ${APP_BASE_TAG}）
  --base-image <img>      构建 app 时指定自定义基础镜像（默认: ${HARBOR_REGISTRY}/${HARBOR_PROJECT}/${APP_BASE_IMAGE_NAME}:${APP_BASE_TAG}）
  --no-base-image         构建 app 时不复用预构建基础镜像，直接从 debian:12.12-slim 完整重装 apt + browserskill
  --latest                额外同时推送 :latest 标签（默认仅推送 :main）
  --platform <platforms>  目标平台（默认: linux/amd64，支持 linux/arm64 或 linux/amd64,linux/arm64）
  --registry <host>       Harbor 域名（默认: ${HARBOR_REGISTRY}）
  --project <name>        Harbor 项目名（默认: ${HARBOR_PROJECT}）

构建调优选项:
  --fast                  极速模式：等价于 --no-anydoc --no-browserskill（跳过全部两套 Rust 编译）
  --no-anydoc             关闭 Rust anydoc 引擎编译（WITH_ANYDOC=0）
  --no-browserskill       跳过 BrowserSkill（Node + Rust）编译（WITH_BROWSERSKILL=0，省 ~12 分钟）
  --browserskill-image <img>
                          直接从已有镜像复用 /opt/weknora/browserskill（0 秒且保留 BrowserSkill 功能）
                          例: --browserskill-image harbor.dobest.com/sdk/weknora-app:main
  --builder <name>        指定 buildx builder（如远端 Linux x86_64 SSH builder，彻底省去跨架构模拟）
  --registry-cache        启用 Harbor 远程构建缓存（:buildcache），适合多机共享或 CI 增量提速
  --no-cache              禁用 Docker 构建缓存
  --no-push               仅构建不推送（单平台模式下自动 --load 到本地 Docker）
  --dry-run               仅打印最终构建推送命令，不实际执行
  -h, --help              显示帮助

常用环境变量:
  GOPROXY                 Go 代理（默认: https://goproxy.cn,direct）
  APK_MIRROR_ARG          Debian apt 镜像源（默认: mirrors.aliyun.com；传空字符串禁用）
  NPM_REGISTRY            前端/BrowserSkill npm 镜像源（默认: https://registry.npmmirror.com；传空禁用）
  PIP_INDEX_URL           Python pip 镜像源（默认: https://mirrors.tencent.com/pypi/simple；传空禁用）
  RUSTUP_DIST_SERVER      Rustup 镜像源（默认: https://rsproxy.cn；传空禁用）
  CARGO_MIRROR_ARG        Cargo crates.io 镜像源（默认: sparse+https://rsproxy.cn/index/；传空禁用）
  NODE_MAX_OLD_SPACE_SIZE 前端打包内存上限（默认: 4096）
  VITE_BASE_PATH          前端静态资源路径前缀（默认: /）
  HARBOR_BUILDER          指定默认 buildx builder 名称

示例:
  $0                                   # 复用本机 orbstack/docker 缓存，构建 linux/amd64 并推 Harbor
  $0 --app --fast                      # 极速构建 app（跳过 anydoc + browserskill 两套 Rust 编译）
  $0 --app --no-anydoc --browserskill-image harbor.dobest.com/sdk/weknora-app:main
                                       # 复用已有镜像里的 BrowserSkill + 跳过 anydoc（快且功能完整）
  $0 --app --tag v0.9.1-fix1           # 仅构建并推送 app 指定 tag
  $0 --builder linux-amd64             # 使用远端 Linux x86_64 SSH builder 原生高速编译
  $0 --registry-cache                  # 同步 BuildKit 缓存到 Harbor 供后续跨机秒编
  $0 --dry-run                         # 预演命令
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --all)
            BUILD_APP=true
            BUILD_FRONTEND=true
            shift
            ;;
        --app)
            BUILD_APP=true
            shift
            ;;
        --app-base)
            BUILD_APP_BASE=true
            shift
            ;;
        --frontend|--ui)
            BUILD_FRONTEND=true
            shift
            ;;
        -t|--tag)
            [ $# -ge 2 ] || { log_error "--tag 缺少参数"; exit 1; }
            CUSTOM_TAG="$2"
            shift 2
            ;;
        --base-tag)
            [ $# -ge 2 ] || { log_error "--base-tag 缺少参数"; exit 1; }
            APP_BASE_TAG="$2"
            shift 2
            ;;
        --base-image)
            [ $# -ge 2 ] || { log_error "--base-image 缺少参数"; exit 1; }
            USE_PREBUILT_BASE=1
            APP_BASE_IMAGE="$2"
            shift 2
            ;;
        --no-base-image)
            USE_PREBUILT_BASE=0
            shift
            ;;
        --latest)
            PUSH_LATEST=true
            shift
            ;;
        --no-latest)
            PUSH_LATEST=false
            shift
            ;;
        --platform)
            [ $# -ge 2 ] || { log_error "--platform 缺少参数"; exit 1; }
            PLATFORM="$2"
            shift 2
            ;;
        --registry)
            [ $# -ge 2 ] || { log_error "--registry 缺少参数"; exit 1; }
            HARBOR_REGISTRY="$2"
            shift 2
            ;;
        --project)
            [ $# -ge 2 ] || { log_error "--project 缺少参数"; exit 1; }
            HARBOR_PROJECT="$2"
            shift 2
            ;;
        --fast)
            WITH_ANYDOC=0
            WITH_BROWSERSKILL=0
            shift
            ;;
        --no-anydoc)
            WITH_ANYDOC=0
            shift
            ;;
        --no-browserskill)
            WITH_BROWSERSKILL=0
            shift
            ;;
        --browserskill-image)
            [ $# -ge 2 ] || { log_error "--browserskill-image 缺少参数"; exit 1; }
            WITH_BROWSERSKILL=prebuilt
            BROWSERSKILL_IMAGE="$2"
            shift 2
            ;;
        --builder)
            [ $# -ge 2 ] || { log_error "--builder 缺少参数"; exit 1; }
            CUSTOM_BUILDER="$2"
            shift 2
            ;;
        --registry-cache)
            USE_REGISTRY_CACHE=true
            shift
            ;;
        --no-cache)
            NO_CACHE=true
            shift
            ;;
        --no-push)
            DO_PUSH=false
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
        *)
            log_error "未知参数: $1（使用 --help 查看用法）"
            exit 1
            ;;
    esac
done

# 未显式指定服务时默认构建 app + frontend
if [ "$BUILD_APP" = false ] && [ "$BUILD_FRONTEND" = false ] && [ "$BUILD_APP_BASE" = false ]; then
    BUILD_APP=true
    BUILD_FRONTEND=true
fi

# 多平台必须推送到仓库（Docker 本地 daemon 无法 --load 多架构 manifest）
IS_MULTI_ARCH=false
case "$PLATFORM" in
    *,*) IS_MULTI_ARCH=true ;;
esac
if [ "$IS_MULTI_ARCH" = true ] && [ "$DO_PUSH" = false ]; then
    log_error "多平台构建 (${PLATFORM}) 不支持 --no-push，请去掉 --no-push 或指定单一平台"
    exit 1
fi

cd "$PROJECT_ROOT"

# 读取版本元数据
VERSION="unknown"
COMMIT_ID="unknown"
BUILD_TIME="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
if [ -x "$PROJECT_ROOT/scripts/get_version.sh" ]; then
    eval "$("$PROJECT_ROOT/scripts/get_version.sh" env)"
fi

IMAGE_TAG="${CUSTOM_TAG:-main}"

# 检查工作区脏状态（仅提示，不阻断）
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null || true)" ]; then
        log_warn "当前 Git 工作区存在未提交改动，构建产物将包含本地修改（commit=${COMMIT_ID}）"
    fi
fi

HOST_OS="$(uname -s)"
HOST_ARCH="$(uname -m)"
HOST_PLATFORM="linux/amd64"
case "$HOST_ARCH" in
    x86_64|amd64) HOST_PLATFORM="linux/amd64" ;;
    aarch64|arm64) HOST_PLATFORM="linux/arm64" ;;
esac

USE_BUILDX=true
if [ "$DRY_RUN" = false ]; then
    if ! command -v docker >/dev/null 2>&1; then
        log_error "未找到 docker 命令，请先安装 Docker"
        exit 1
    fi
    if ! docker info >/dev/null 2>&1; then
        log_error "Docker 服务未运行或当前用户无权限访问 Docker daemon"
        exit 1
    fi
    if ! docker buildx version >/dev/null 2>&1; then
        if [ "$HOST_OS" = "Linux" ] && [ "$PLATFORM" = "$HOST_PLATFORM" ]; then
            USE_BUILDX=false
            log_warn "未检测到 docker buildx，将回退为本机 docker build + docker push（平台: ${PLATFORM}）"
        else
            log_error "交叉/多平台构建 (${PLATFORM}) 需要 docker buildx，请升级或安装 docker-buildx-plugin"
            exit 1
        fi
    fi
fi

SELECTED_BUILDER=""
ensure_buildx_builder() {
    [ "$USE_BUILDX" = true ] || return 0

    # 1. 显式指定 builder（如远端 Linux x86_64 SSH builder）时优先使用
    if [ -n "$CUSTOM_BUILDER" ]; then
        SELECTED_BUILDER="$CUSTOM_BUILDER"
        if [ "$DRY_RUN" = false ]; then
            if ! docker buildx inspect "$SELECTED_BUILDER" >/dev/null 2>&1; then
                log_error "指定的 buildx builder 不存在: ${SELECTED_BUILDER}"
                exit 1
            fi
        fi
        return 0
    fi

    # 2. 仅多平台构建或启用 --registry-cache（mode=max 需要 docker-container 驱动）时才使用独立容器 builder
    if [ "$IS_MULTI_ARCH" = true ] || [ "$USE_REGISTRY_CACHE" = true ]; then
        SELECTED_BUILDER="$BUILDER_NAME"
        if [ "$DRY_RUN" = false ]; then
            if ! docker buildx inspect "$SELECTED_BUILDER" >/dev/null 2>&1; then
                log_info "创建 docker-container buildx builder: ${SELECTED_BUILDER}..."
                docker buildx create --name "$SELECTED_BUILDER" --driver docker-container >/dev/null
            fi
            docker buildx inspect --bootstrap "$SELECTED_BUILDER" >/dev/null
        fi
        return 0
    fi

    # 3. 单平台构建（如 linux/amd64）：直接复用本机 orbstack / default builder，共享本地已有镜像和层缓存，避免冷启动
    if [ "$DRY_RUN" = false ]; then
        if docker buildx inspect orbstack >/dev/null 2>&1; then
            SELECTED_BUILDER="orbstack"
        elif docker buildx inspect default >/dev/null 2>&1; then
            SELECTED_BUILDER="default"
        fi
    else
        if command -v docker >/dev/null 2>&1 && docker buildx inspect orbstack >/dev/null 2>&1; then
            SELECTED_BUILDER="orbstack"
        else
            SELECTED_BUILDER="default"
        fi
    fi
}

run_cmd() {
    if [ "$DRY_RUN" = true ]; then
        printf '[DRY-RUN]'
        for arg in "$@"; do
            printf ' %q' "$arg"
        done
        printf '\n'
    else
        "$@"
    fi
}

APP_REPO="${HARBOR_REGISTRY}/${HARBOR_PROJECT}/${APP_IMAGE_NAME}"
APP_BASE_REPO="${HARBOR_REGISTRY}/${HARBOR_PROJECT}/${APP_BASE_IMAGE_NAME}"
UI_REPO="${HARBOR_REGISTRY}/${HARBOR_PROJECT}/${UI_IMAGE_NAME}"
RESOLVED_APP_BASE_IMAGE="${APP_BASE_IMAGE:-${APP_BASE_REPO}:${APP_BASE_TAG}}"

ensure_buildx_builder

log_info "=============================================="
log_info "Harbor 仓库 : ${HARBOR_REGISTRY}/${HARBOR_PROJECT}"
log_info "目标平台    : ${PLATFORM} (本机: ${HOST_OS}/${HOST_ARCH})"
log_info "构建器      : $([ "$USE_BUILDX" = true ] && echo "buildx (${SELECTED_BUILDER:-current})" || echo "docker build")"
log_info "镜像标签    : ${IMAGE_TAG}$([ "$PUSH_LATEST" = true ] && [ "$IMAGE_TAG" != "latest" ] && echo " + latest" || true)"
log_info "推送开关    : $([ "$DO_PUSH" = true ] && echo "推送至 Harbor" || echo "仅本地构建 (--no-push)")"
log_info "构建服务    : $([ "$BUILD_APP_BASE" = true ] && echo -n "app-base(${APP_BASE_TAG}) ")$([ "$BUILD_APP" = true ] && echo -n "app ")$([ "$BUILD_FRONTEND" = true ] && echo -n "frontend")"
log_info "加速配置    : apt=${APK_MIRROR_DEFAULT:-default}, npm=${NPM_REGISTRY_DEFAULT:-default}, pip=${PIP_INDEX_URL_DEFAULT:-default}, anydoc=${WITH_ANYDOC}, base=$([ "$USE_PREBUILT_BASE" = "1" ] && echo "${RESOLVED_APP_BASE_IMAGE}" || echo "scratch"), browserskill=${WITH_BROWSERSKILL}$([ "$WITH_BROWSERSKILL" = "prebuilt" ] && echo "(${BROWSERSKILL_IMAGE})" || true), registry-cache=${USE_REGISTRY_CACHE}"
log_info "版本元数据  : version=${VERSION}, commit=${COMMIT_ID}"
log_info "=============================================="

if [ "$DO_PUSH" = true ] && [ "$DRY_RUN" = false ]; then
    log_info "提示：如尚未登录 Harbor，请先执行: docker login ${HARBOR_REGISTRY}"
fi

build_and_push_service() {
    local service="$1"
    local repo="$2"
    local dockerfile="$3"
    local context="$4"
    shift 4
    local extra_args=("$@")

    local tags=(-t "${repo}:${IMAGE_TAG}")
    if [ "$PUSH_LATEST" = true ] && [ "$IMAGE_TAG" != "latest" ]; then
        tags+=(-t "${repo}:latest")
    fi

    local cache_args=()
    if [ "$NO_CACHE" = true ]; then
        cache_args+=(--no-cache)
    elif [ "$USE_REGISTRY_CACHE" = true ] && [ "$USE_BUILDX" = true ]; then
        cache_args+=(--cache-from "type=registry,ref=${repo}:buildcache")
        if [ "$DO_PUSH" = true ]; then
            cache_args+=(--cache-to "type=registry,ref=${repo}:buildcache,mode=max")
        fi
    fi

    local builder_args=()
    if [ -n "$SELECTED_BUILDER" ]; then
        builder_args+=(--builder "$SELECTED_BUILDER")
    fi

    log_info "开始构建 ${service} -> ${repo}:${IMAGE_TAG} (${PLATFORM})..."

    if [ "$USE_BUILDX" = true ]; then
        local output_flag="--push"
        if [ "$DO_PUSH" = false ]; then
            output_flag="--load"
        fi
        run_cmd docker buildx build \
            ${builder_args[@]+"${builder_args[@]}"} \
            --platform "$PLATFORM" \
            "$output_flag" \
            ${cache_args[@]+"${cache_args[@]}"} \
            ${extra_args[@]+"${extra_args[@]}"} \
            "${tags[@]}" \
            -f "$dockerfile" \
            "$context"
    else
        run_cmd docker build \
            --platform "$PLATFORM" \
            ${cache_args[@]+"${cache_args[@]}"} \
            ${extra_args[@]+"${extra_args[@]}"} \
            "${tags[@]}" \
            -f "$dockerfile" \
            "$context"
        if [ "$DO_PUSH" = true ]; then
            run_cmd docker push "${repo}:${IMAGE_TAG}"
            if [ "$PUSH_LATEST" = true ] && [ "$IMAGE_TAG" != "latest" ]; then
                run_cmd docker push "${repo}:latest"
            fi
        fi
    fi

    log_success "${service} 完成: ${repo}:${IMAGE_TAG}"
}

if [ "$BUILD_APP_BASE" = true ]; then
    base_args=(
        --target runtime-base
        --build-arg "APK_MIRROR_ARG=${APK_MIRROR_DEFAULT}"
        --build-arg "NPM_REGISTRY=${NPM_REGISTRY_DEFAULT}"
        --build-arg "PIP_INDEX_URL=${PIP_INDEX_URL_DEFAULT}"
        --build-arg "RUSTUP_DIST_SERVER=${RUSTUP_DIST_SERVER_DEFAULT}"
        --build-arg "RUSTUP_UPDATE_ROOT=${RUSTUP_UPDATE_ROOT_DEFAULT}"
        --build-arg "CARGO_MIRROR_ARG=${CARGO_MIRROR_DEFAULT}"
        --build-arg "WITH_BROWSERSKILL=${WITH_BROWSERSKILL}"
    )
    if [ -n "$BROWSERSKILL_IMAGE" ]; then
        base_args+=(--build-arg "BROWSERSKILL_IMAGE=${BROWSERSKILL_IMAGE}")
    fi
    SAVED_IMAGE_TAG="$IMAGE_TAG"
    SAVED_PUSH_LATEST="$PUSH_LATEST"
    IMAGE_TAG="$APP_BASE_TAG"
    PUSH_LATEST=false
    build_and_push_service "app-base" "$APP_BASE_REPO" "docker/Dockerfile.app" "." "${base_args[@]}"
    IMAGE_TAG="$SAVED_IMAGE_TAG"
    PUSH_LATEST="$SAVED_PUSH_LATEST"
fi

if [ "$BUILD_APP" = true ]; then
    app_args=(
        --build-arg "GOPRIVATE_ARG=${GOPRIVATE:-}"
        --build-arg "GOPROXY_ARG=${GOPROXY:-https://goproxy.cn,direct}"
        --build-arg "GOSUMDB_ARG=${GOSUMDB:-off}"
        --build-arg "APK_MIRROR_ARG=${APK_MIRROR_DEFAULT}"
        --build-arg "NPM_REGISTRY=${NPM_REGISTRY_DEFAULT}"
        --build-arg "PIP_INDEX_URL=${PIP_INDEX_URL_DEFAULT}"
        --build-arg "RUSTUP_DIST_SERVER=${RUSTUP_DIST_SERVER_DEFAULT}"
        --build-arg "RUSTUP_UPDATE_ROOT=${RUSTUP_UPDATE_ROOT_DEFAULT}"
        --build-arg "CARGO_MIRROR_ARG=${CARGO_MIRROR_DEFAULT}"
        --build-arg "VERSION_ARG=${VERSION}"
        --build-arg "COMMIT_ID_ARG=${COMMIT_ID}"
        --build-arg "BUILD_TIME_ARG=${BUILD_TIME}"
        --build-arg "WITH_ANYDOC=${WITH_ANYDOC}"
        --build-arg "WITH_BROWSERSKILL=${WITH_BROWSERSKILL}"
        --build-arg "USE_PREBUILT_BASE=${USE_PREBUILT_BASE}"
    )
    if [ "$USE_PREBUILT_BASE" = "1" ]; then
        app_args+=(--build-arg "APP_BASE_IMAGE=${RESOLVED_APP_BASE_IMAGE}")
    fi
    if [ -n "$BROWSERSKILL_IMAGE" ]; then
        app_args+=(--build-arg "BROWSERSKILL_IMAGE=${BROWSERSKILL_IMAGE}")
    fi
    build_and_push_service "app" "$APP_REPO" "docker/Dockerfile.app" "." "${app_args[@]}"
fi

if [ "$BUILD_FRONTEND" = true ]; then
    ui_args=(
        --build-arg "VITE_FRONTEND_COMMIT=${COMMIT_ID}"
        --build-arg "NODE_MAX_OLD_SPACE_SIZE=${NODE_MAX_OLD_SPACE_SIZE:-4096}"
        --build-arg "VITE_BASE_PATH=${VITE_BASE_PATH:-/}"
    )
    if [ -n "$NPM_REGISTRY_DEFAULT" ]; then
        ui_args+=(--build-arg "NPM_REGISTRY=${NPM_REGISTRY_DEFAULT}")
    fi
    build_and_push_service "frontend" "$UI_REPO" "frontend/Dockerfile" "frontend/" "${ui_args[@]}"
fi

echo ""
log_success "全部构建任务完成！"
if [ "$DO_PUSH" = true ] && { [ "$BUILD_APP" = true ] || [ "$BUILD_FRONTEND" = true ]; }; then
    cat <<EOF

================ 线上 Linux 部署命令 ================
# 方式 1：使用封装脚本（自动记录上一版本、支持一键回滚与健康检查）
./scripts/deploy_harbor.sh --tag ${IMAGE_TAG}

# 方式 2：直接使用原生 docker compose（不改动原 docker-compose.yml）
WEKNORA_VERSION=${IMAGE_TAG} docker compose -f docker-compose.yml -f docker-compose.harbor.yml pull $([ "$BUILD_APP" = true ] && echo -n "app ")$([ "$BUILD_FRONTEND" = true ] && echo -n "frontend")
WEKNORA_VERSION=${IMAGE_TAG} docker compose -f docker-compose.yml -f docker-compose.harbor.yml up -d --no-build $([ "$BUILD_APP" = true ] && echo -n "app ")$([ "$BUILD_FRONTEND" = true ] && echo -n "frontend")

# 一键回滚上一版本：
./scripts/deploy_harbor.sh --rollback
=====================================================
EOF
fi
