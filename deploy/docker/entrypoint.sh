#!/usr/bin/env bash
# =============================================================================
# entrypoint.sh —— filemanage-server 容器入口脚本（需求27/28）
#
# 功能：
#   1. 按 FM_NODES 在同一个容器内生成并启动多个 worker 节点（每节点独立端口、
#      独立数据目录，互不干扰）；
#   2. 按 FM_BALANCER 决定是否额外启动一个内置负载均衡进程（-mode balancer），
#      对本容器内所有 worker 做轮询反向代理，对外提供统一入口；
#   3. 转发 SIGTERM/SIGINT 给所有子进程，实现容器优雅停止；
#   4. 任一子进程退出则整个容器退出（wait -n），交给编排系统统一重启。
#
# 支持的环境变量（均可在 docker run -e / compose environment 中覆盖）：
#   FM_NODES          本容器内 worker 节点数，默认 1
#   FM_BASE_PORT      第一个 worker 的监听端口，节点 i 使用 FM_BASE_PORT+i，默认 8484
#   FM_DATA_DIR       数据根目录，默认 /data；节点 i 的数据在 $FM_DATA_DIR/node$i/
#   FM_BALANCER       是否启动内置负载均衡，1/0，默认 1
#                     （仅当 FM_NODES>1 或显式设置为 1 时才真正启动，
#                       单节点且未显式开启时不启动，避免无意义代理层）
#   FM_BALANCER_PORT  balancer 监听端口，默认 8480
#   FM_CONFIG         可选：指定一个已有的 server.json 时，直接以单节点模式
#                     exec 运行该配置，跳过下面所有生成逻辑
# =============================================================================
set -euo pipefail

SERVER_BIN="${SERVER_BIN:-filemanage-server}"   # 二进制名/路径，一般无需修改

# ---------- FM_CONFIG 直通模式 ----------
# 用户自带配置文件（如挂载进来的 server.json）时，直接单节点运行，不做任何生成。
if [[ -n "${FM_CONFIG:-}" ]]; then
    echo "[entrypoint] 检测到 FM_CONFIG=${FM_CONFIG}，以单节点模式直接运行该配置"
    exec "${SERVER_BIN}" -config "${FM_CONFIG}"
fi

# ---------- 读取并校验环境变量 ----------
# 记录 FM_BALANCER 是否被"显式设置"（docker 镜像 ENV 提供的默认值也算显式设置）
FM_BALANCER_EXPLICIT=0
if [[ -n "${FM_BALANCER+x}" ]]; then
    FM_BALANCER_EXPLICIT=1
fi

FM_NODES="${FM_NODES:-1}"                 # 本容器内 worker 节点数
FM_BASE_PORT="${FM_BASE_PORT:-8484}"      # worker 起始端口
FM_DATA_DIR="${FM_DATA_DIR:-/data}"       # 数据根目录
FM_BALANCER="${FM_BALANCER:-1}"           # 是否启动内置 balancer
FM_BALANCER_PORT="${FM_BALANCER_PORT:-8480}"  # balancer 监听端口
FM_ADMIN_BASE_PORT="${FM_ADMIN_BASE_PORT:-18485}"  # 管理口起始端口，节点 i 用 +i（0.1.0：需求8/需求38——每节点独立 adminListen，默认仅回环，经 docker exec / SSH 隧道访问）

if ! [[ "${FM_NODES}" =~ ^[0-9]+$ ]] || (( FM_NODES < 1 )); then
    echo "[entrypoint] 错误：FM_NODES 必须是 >=1 的整数，当前值：${FM_NODES}" >&2
    exit 1
fi

NODE_ADDRS=()   # 各 worker 节点的监听地址（127.0.0.1:port），供 balancer 使用
NODE_PIDS=()    # 所有后台子进程（worker + balancer）的 PID

# ---------- 优雅退出：把信号转发给所有子进程并等待 ----------
shutdown() {
    # 先解除 trap，避免 kill 引发的信号造成递归
    trap - TERM INT EXIT
    echo "[entrypoint] 收到退出信号，转发 SIGTERM 给子进程：${NODE_PIDS[*]:-无}"
    for pid in "${NODE_PIDS[@]:-}"; do
        [[ -n "${pid}" ]] || continue
        kill -TERM "${pid}" 2>/dev/null || true
    done
    wait 2>/dev/null || true
}
trap 'shutdown; exit 143' TERM INT
trap shutdown EXIT

# ---------- 为每个节点生成配置并创建数据目录 ----------
# 默认策略：每节点使用完全独立的目录（node$i/{files,trash,staging,versions} 与
# 独立的 meta.db），满足"多实例互不干扰"的部署要求（需求26）。
#
# —— 共享数据模式（可选，注释示例）——
# 若希望多实例共享同一份数据组成集群，应让各节点配置指向相同的：
#   db.dsn、roots[].path、trashDir、uploadStagingDir（及 dataDir，见下）
# 例如把下面 heredoc 中所有 "$node_dir/..." 统一替换为共享目录 "$FM_DATA_DIR/shared/..."：
#   "db":               {"driver": "sqlite", "dsn": "/data/shared/meta.db"},
#   "roots":            [{"id": "main", "name": "主目录", "path": "/data/shared/files"}],
#   "trashDir":         "/data/shared/trash",
#   "uploadStagingDir": "/data/shared/staging",
# 注意：用户家目录固定位于 <dataDir>/Users/<用户名>（用户文件与其回收站
# 实体都在其中）——多实例共享部署时须将 dataDir 一并指向共享卷
# （如 "dataDir": "/data/shared"），否则用户文件跨实例不可见。
# 注意：共享 sqlite 时各节点并发写同一 db 文件，建议数据目录放在本地盘（非网络盘）。
# 认证自 0.2.3 起为服务端三层认证图（不透明随机凭证，L3=共享库 auth_sessions 表）：
# 登录凭证存于共享元数据库，任一节点签发、全节点互认，无需再共享任何密钥文件；
# 各节点如同时指向 redis（cache.enabled），认证缓存 L2 亦天然共享（可选）。
for ((i = 0; i < FM_NODES; i++)); do
    port=$(( FM_BASE_PORT + i ))
    admin_port=$(( FM_ADMIN_BASE_PORT + i ))
    node_dir="${FM_DATA_DIR}/node${i}"
    mkdir -p "${node_dir}/files" "${node_dir}/trash" \
             "${node_dir}/staging" "${node_dir}/versions"

    # 生成节点配置 server.json（sqlite 驱动；ai.provider=none 显式关闭 AI，避免歧义）
    cat > "${node_dir}/server.json" <<EOF
{
  "schemaVersion": 8,
  "listen": "0.0.0.0:${port}",
  "adminListen": "127.0.0.1:${admin_port}",
  "adminAccess": {"mode": "lan", "ips": []},
  "lanOnly": false,
  "dataDir": "${node_dir}/data",
  "instanceId": "${HOSTNAME:-fm}-node${i}",
  "db": {"driver": "sqlite", "dsn": "${node_dir}/meta.db"},
  "roots": [{"id": "main", "name": "主目录", "path": "${node_dir}/files"}],
  "trashDir": "${node_dir}/trash",
  "uploadStagingDir": "${node_dir}/staging",
  "secretFile": "${node_dir}/jwt.secret",
  "versions": {"enabled": true, "maxVersions": 10, "dir": "${node_dir}/versions"},
  "ai": {"provider": "none"}
}
EOF

    NODE_ADDRS+=("127.0.0.1:${port}")
    echo "[entrypoint] 已生成节点 node${i} 配置：${node_dir}/server.json（监听 0.0.0.0:${port}）"
done

# ---------- 后台启动各 worker 节点 ----------
for ((i = 0; i < FM_NODES; i++)); do
    "${SERVER_BIN}" -config "${FM_DATA_DIR}/node${i}/server.json" &
    NODE_PIDS+=("$!")
    echo "[entrypoint] 已启动 worker node${i}，PID=$!"
done

# ---------- 按需生成并启动内置负载均衡 ----------
# 启动条件：FM_NODES>1（多节点必须有统一入口），或显式设置 FM_BALANCER=1。
START_BALANCER=0
if (( FM_NODES > 1 )) || { (( FM_BALANCER_EXPLICIT )) && [[ "${FM_BALANCER}" == "1" ]]; }; then
    START_BALANCER=1
fi

if (( START_BALANCER )); then
    bal_dir="${FM_DATA_DIR}/balancer"
    mkdir -p "${bal_dir}"

    # 把各节点地址拼成 JSON 数组元素："127.0.0.1:8484", "127.0.0.1:8485", ...
    workers_json=""
    for addr in "${NODE_ADDRS[@]}"; do
        if [[ -n "${workers_json}" ]]; then
            workers_json+=", "
        fi
        workers_json+="\"${addr}\""
    done

    cat > "${bal_dir}/balancer.json" <<EOF
{
  "schemaVersion": 8,
  "listen": "0.0.0.0:${FM_BALANCER_PORT}",
  "lanOnly": false,
  "balancer": {"workers": [${workers_json}]}
}
EOF

    "${SERVER_BIN}" -mode balancer -config "${bal_dir}/balancer.json" &
    NODE_PIDS+=("$!")
    echo "[entrypoint] 已启动内置 balancer，PID=$!，监听 0.0.0.0:${FM_BALANCER_PORT}，workers=[${workers_json}]"
else
    echo "[entrypoint] 未启动内置 balancer（FM_NODES=${FM_NODES}，FM_BALANCER=${FM_BALANCER}，未显式开启）"
fi

# ---------- 主循环 ----------
# wait -n：任一子进程退出即返回，随后整个脚本（容器）退出，
# 便于 docker/k8s 等编排系统按 restart 策略整体重启，避免"半死"状态。
echo "[entrypoint] 共启动 ${#NODE_PIDS[@]} 个进程，等待任一进程退出..."
status=0
wait -n || status=$?
echo "[entrypoint] 某子进程已退出（状态码 ${status}），容器即将退出"
exit "${status}"
