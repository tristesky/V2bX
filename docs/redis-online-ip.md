# Redis 分布式在线 IP 与活跃节点限制部署文档

本文档说明如何部署 Redis 主从 + Sentinel，并通过 WireGuard 内网为 V2bX 提供分布式在线 IP 和长期活跃节点限制。

## 架构

推荐架构：

```text
V2bX 节点 1 ┐
V2bX 节点 2 ├── WireGuard 内网 ── Redis/Sentinel A
V2bX 节点 N ┘                    ├─ Redis/Sentinel B
                                   └─ Redis/Sentinel C
```

建议使用 3 台 Redis 服务器：

```text
redis-a  10.66.0.2  Redis master   + Sentinel
redis-b  10.66.0.3  Redis replica  + Sentinel
redis-c  10.66.0.4  Redis replica  + Sentinel
```

V2bX 节点通过 WireGuard 内网连接 Sentinel。用户不需要、也不应该连接 Redis。

## 限制数量从哪里来

限制用户在线 IP 数量仍然由面板决定。V2Board 用户套餐里的在线 IP 数会通过用户列表接口下发为 `device_limit`，V2bX 接收后存入 `panel.UserInfo.DeviceLimit`。

开启 `LimitConfig.OnlineIPLimit.Enable` 后，V2bX 会用 Redis 判断某个用户的新来源 IP 是否允许进入：

```text
uid + source_ip + device_limit -> Redis Lua 原子检查 -> 放行或拒绝
```

如果 Redis 或 Sentinel 不可用，V2bX 会 fail-open 直接放行，避免 Redis 故障导致节点全站不可用。

可选开启 `LimitConfig.SameIPActiveNodeLimit.Enable`，处理一个公网 IP 同时长期挂在很多节点上的共享场景：

```text
uid + source_ip + stable_node_id + active_node_limit -> Redis Lua 原子检查 -> 放行、观察或拒绝
```

`stable_node_id` 由面板地址、节点类型和节点 ID 组成。同一用户从同一个来源 IP 连接同一个节点的多条连接只计一个活跃节点；从该 IP 换到不同节点才会增加计数。不同来源 IP 各自统计，因此用户已经正常使用多个允许 IP 时，在节点之间切换不会被错误合并为全局节点超限。该功能限制的是同 IP 的长期活跃节点，不是真实物理设备数量。

`SameIPActiveNodeLimit.Limit` 为 `0` 时，自动使用面板下发给每个用户的 `device_limit`。例如套餐限制为 `3` 的用户，每一个来源 IP 最多保留 3 个长期活跃节点；套餐限制为 `15` 的用户，每一个来源 IP 最多保留 15 个，无需在节点配置中逐个用户维护数字。

> 这是独立开关：`device_limit` 只提供上限数值，`OnlineIPLimit` 只限制不同来源 IP 数量。只更新 V2bX 二进制或只配置 `OnlineIPLimit`，不会启用同一来源 IP 的多节点限制。

活跃节点使用两阶段策略，减少测速误伤和循环断线：

```text
新节点连接 -> 本机观察 ActivationDelay 秒
短时间测速结束 -> 不写入正式活跃节点集合
持续在线 -> 申请 Redis 正式名额
超限 -> 清退该节点，并在 BlockTTL 内进入强制收敛状态
强制收敛期间且名额仍满 -> 额外节点重连立即拒绝，不重新获得观察期
已有节点释放名额 -> 新连接可以恢复观察并正常取得名额
```

Redis 中保存的是带 TTL 的在线 IP 与正式活跃节点租约，不是永久名单。Xray TCP 入站（包括 VLESS + gRPC）和 Hysteria2 客户端连接会在存活期间按 `RefreshInterval` 续租；活跃节点在本节点最后一条连接断开后立即释放名额。

fail-open 仍然优先保证节点可用性：Redis 故障期间可能暂时放进超限连接。Redis 恢复后，正式租约续租会重新比较限制数量，超出的最新 IP 或节点会被清退。参与排序的 V2bX 机器应保持 NTP/chrony 时间同步。

## 端口

这些端口只应该在 WireGuard 内网可访问：

```text
6379   Redis
26379  Redis Sentinel
```

WireGuard 默认常用 UDP `51820`，也可以换成其它端口。

不要把 Redis 或 Sentinel 直接暴露到公网。

## 安装 WireGuard

在 3 台 Redis 服务器和所有需要访问 Redis 的 V2bX 节点上安装：

```bash
apt update
apt install -y wireguard
```

每台机器生成密钥：

```bash
umask 077
wg genkey | tee /etc/wireguard/private.key | wg pubkey > /etc/wireguard/public.key
```

`redis-a` 的 `/etc/wireguard/wg0.conf` 示例：

```ini
[Interface]
Address = 10.66.0.2/24
ListenPort = 51820
PrivateKey = <redis-a-private-key>

[Peer]
PublicKey = <redis-b-public-key>
AllowedIPs = 10.66.0.3/32
Endpoint = <redis-b-public-ip>:51820
PersistentKeepalive = 25

[Peer]
PublicKey = <redis-c-public-key>
AllowedIPs = 10.66.0.4/32
Endpoint = <redis-c-public-ip>:51820
PersistentKeepalive = 25

[Peer]
PublicKey = <v2bx-node-1-public-key>
AllowedIPs = 10.66.1.11/32
Endpoint = <v2bx-node-1-public-ip>:51820
PersistentKeepalive = 25
```

某台 V2bX 节点的 `/etc/wireguard/wg0.conf` 示例：

```ini
[Interface]
Address = 10.66.1.11/24
ListenPort = 51820
PrivateKey = <v2bx-node-private-key>

[Peer]
PublicKey = <redis-a-public-key>
AllowedIPs = 10.66.0.2/32
Endpoint = <redis-a-public-ip>:51820
PersistentKeepalive = 25

[Peer]
PublicKey = <redis-b-public-key>
AllowedIPs = 10.66.0.3/32
Endpoint = <redis-b-public-ip>:51820
PersistentKeepalive = 25

[Peer]
PublicKey = <redis-c-public-key>
AllowedIPs = 10.66.0.4/32
Endpoint = <redis-c-public-ip>:51820
PersistentKeepalive = 25
```

启动 WireGuard：

```bash
systemctl enable --now wg-quick@wg0
wg show
ping -c 3 10.66.0.2
```

### WireGuard Peer 注意事项

如果使用 full mesh，每一台 Redis/Sentinel 服务器都必须添加所有 V2bX 节点作为 Peer；每一台 V2bX 节点也必须添加所有 Redis/Sentinel 服务器作为 Peer。

例如有 3 台 Redis/Sentinel 和 5 台 V2bX 节点，则每台 Redis/Sentinel 至少需要配置另外 2 台 Redis/Sentinel Peer，以及 5 台 V2bX 节点 Peer。每台 V2bX 节点则至少需要配置 3 台 Redis/Sentinel Peer。

如果节点数量很多，建议使用 WireGuard hub 或其它 overlay 网络减少 Peer 管理。关键要求是：所有 V2bX 节点都能通过 WireGuard 内网访问每台 Redis/Sentinel 服务器的 `6379` 和 `26379` 端口。

> 注意：V2bX 连接 Sentinel 后，客户端会从 Sentinel 获取当前 master 地址，然后继续连接对应 Redis master 的 `6379` 端口。所以不能只放通 `26379`，也必须放通 Redis 的 `6379`。

## 安装 Redis 和 Sentinel

在 3 台 Redis 服务器上执行：

```bash
apt update
apt install -y redis-server redis-sentinel
```

准备一个强密码：

```text
REDIS_PASSWORD=<换成足够长的随机密码>
```

## 配置 Redis

`redis-a` 的 `/etc/redis/redis.conf`：

```conf
bind 127.0.0.1 10.66.0.2
port 6379
protected-mode yes

requirepass <REDIS_PASSWORD>
masterauth <REDIS_PASSWORD>

appendonly yes
appendfsync everysec
```

`redis-b` 的 `/etc/redis/redis.conf`：

```conf
bind 127.0.0.1 10.66.0.3
port 6379
protected-mode yes

requirepass <REDIS_PASSWORD>
masterauth <REDIS_PASSWORD>
replicaof 10.66.0.2 6379

appendonly yes
appendfsync everysec
```

`redis-c` 的 `/etc/redis/redis.conf`：

```conf
bind 127.0.0.1 10.66.0.4
port 6379
protected-mode yes

requirepass <REDIS_PASSWORD>
masterauth <REDIS_PASSWORD>
replicaof 10.66.0.2 6379

appendonly yes
appendfsync everysec
```

重启 Redis：

```bash
systemctl restart redis-server
systemctl status redis-server --no-pager
```

检查复制状态：

```bash
redis-cli -h 10.66.0.2 -a '<REDIS_PASSWORD>' INFO replication
```

## 配置 Sentinel

`redis-a` 的 `/etc/redis/sentinel.conf`：

```conf
bind 127.0.0.1 10.66.0.2
port 26379
protected-mode yes

sentinel monitor mymaster 10.66.0.2 6379 2
sentinel auth-pass mymaster <REDIS_PASSWORD>
sentinel down-after-milliseconds mymaster 5000
sentinel failover-timeout mymaster 60000
sentinel parallel-syncs mymaster 1
sentinel announce-ip 10.66.0.2
sentinel announce-port 26379
```

`redis-b` 使用相同配置，但改为：

```conf
bind 127.0.0.1 10.66.0.3
sentinel announce-ip 10.66.0.3
```

`redis-c` 使用：

```conf
bind 127.0.0.1 10.66.0.4
sentinel announce-ip 10.66.0.4
```

> 注意：`sentinel auth-pass mymaster` 是 Sentinel 连接 Redis master/replica 使用的认证，不等于客户端连接 Sentinel 本身的认证。如果希望 V2bX 连接 Sentinel 时也需要认证，需要额外配置 Redis Sentinel ACL 或 `requirepass`，并在 V2bX 中填写 `SentinelUsername` / `SentinelPassword`。

启动 Sentinel：

```bash
systemctl enable --now redis-sentinel
systemctl status redis-sentinel --no-pager
```

检查 Sentinel：

```bash
redis-cli -h 10.66.0.2 -p 26379 SENTINEL get-master-addr-by-name mymaster
redis-cli -h 10.66.0.3 -p 26379 SENTINEL replicas mymaster
```

维护窗口内可以测试故障转移：

```bash
systemctl stop redis-server
redis-cli -h 10.66.0.3 -p 26379 SENTINEL get-master-addr-by-name mymaster
systemctl start redis-server
```

## 配置 V2bX

在所有参与限制的 Xray/Hysteria2 节点配置的 `LimitConfig` 下添加：

```json
"LimitConfig": {
  "OnlineIPLimit": {
    "Enable": true,
    "Type": "redis",
    "Scope": "main-v2board",
    "KeyPrefix": "v2bx:online_ip",
    "TTL": 120,
    "RefreshInterval": 20,
    "RejectCacheTTL": 3,
    "Timeout": 200,
    "FailureCooldown": 30,
    "IPv6Prefix": 128,
    "RedisConfig": {
      "Addresses": [
        "10.66.0.2:26379",
        "10.66.0.3:26379",
        "10.66.0.4:26379"
      ],
      "MasterName": "mymaster",
      "Password": "<REDIS_PASSWORD>",
      "Db": 0,
      "TLS": false
    }
  },
  "SameIPActiveNodeLimit": {
    "Enable": true,
    "Limit": 0,
    "ActivationDelay": 60,
    "BlockTTL": 600,
    "KeyPrefix": "v2bx:same_ip_active_node"
  }
}
```

字段说明：

```text
Enable           是否启用 Redis 在线 IP 限制。
Type             限制器后端类型，目前支持 redis；留空时也按 redis 处理。
Scope            共享命名空间。同一套 V2Board 面板下的所有节点必须一致。
KeyPrefix        Redis key 前缀。
TTL              在线 IP 租约时间，单位秒。
RefreshInterval  本地放行缓存时间和活跃 Xray TCP/Hysteria2 连接续租间隔，单位秒。越小越严格，越大 Redis 请求越少。
RejectCacheTTL   被拒绝 IP 的短缓存时间，单位秒。
Timeout          Redis 操作超时，单位毫秒。
FailureCooldown  Redis 出错后 fail-open 熔断时间，单位秒。
IPv6Prefix       128 表示完整 IPv6；64 表示按 /64 统计。
Addresses        Sentinel 的 WireGuard 内网地址，不是公网地址。
MasterName       Sentinel master 名称。
Password         Redis master 密码。
```

活跃节点字段说明：

```text
SameIPActiveNodeLimit.Enable           是否启用同来源 IP 长期活跃节点限制。
SameIPActiveNodeLimit.Limit            0 表示按每个用户的 device_limit；正数表示每个来源 IP 使用固定上限。
SameIPActiveNodeLimit.ActivationDelay  新节点观察时间，单位秒。建议 60，用于过滤普通测速。
SameIPActiveNodeLimit.BlockTTL         确认超限后的收敛/拒绝时间，单位秒。建议 600。
SameIPActiveNodeLimit.KeyPrefix        Redis key 前缀，默认 v2bx:same_ip_active_node。
SameIPActiveNodeLimit.IPv6Prefix       IPv6 聚合前缀；留空时继承 OnlineIPLimit.IPv6Prefix。
```

上例的 `SameIPActiveNodeLimit` 会复用 `OnlineIPLimit` 的 `RedisConfig`、`Scope`、`TTL`、`RefreshInterval`、`RejectCacheTTL`、`Timeout`、`FailureCooldown` 和 `IPv6Prefix`。若只开启该限制，则必须在 `SameIPActiveNodeLimit` 中完整填写 Redis 参数。旧字段名 `ActiveNodeLimit` 仍兼容读取，但升级后建议改为 `SameIPActiveNodeLimit` 以明确其按来源 IP 分组的含义。

### 例如：

```json
{
  "Core": "xray",
  "ApiHost": "https://你的面板地址",
  "ApiKey": "xxx",
  "NodeID": 1,
  "NodeType": "vless",
  "Timeout": 30,

  "LimitConfig": {
    "OnlineIPLimit": {
      "Enable": true,
      "Type": "redis",
      "Scope": "main-v2board",
      "KeyPrefix": "v2bx:online_ip",
      "TTL": 120,
      "RefreshInterval": 20,
      "RejectCacheTTL": 3,
      "Timeout": 200,
      "FailureCooldown": 30,
      "IPv6Prefix": 128,
      "RedisConfig": {
        "Addresses": [
          "10.66.0.2:26379",
          "10.66.0.3:26379",
          "10.66.0.4:26379"
        ],
        "MasterName": "mymaster",
        "Password": "你的Redis密码",
        "Db": 0,
        "TLS": false
      }
    },
    "SameIPActiveNodeLimit": {
      "Enable": true,
      "Limit": 0,
      "ActivationDelay": 60,
      "BlockTTL": 600,
      "KeyPrefix": "v2bx:same_ip_active_node"
    }
  }
}
```

如果 Redis 使用 ACL 用户，也可以配置：

```json
"Username": "v2bx",
"Password": "<redis-user-password>"
```

如果 Sentinel 自身也启用了认证，配置：

```json
"SentinelUsername": "sentinel-user",
"SentinelPassword": "<sentinel-password>"
```

修改配置后重启 V2bX。

启动后应在每个参与限制的节点日志中看到 `same-ip active node limiter enabled`。若出现 `same-ip active node limiter disabled; OnlineIPLimit does not restrict one source IP using multiple nodes`，表示该节点只启用了 IP 数限制，无法拦截一个 IP 长期占用多个节点。确认超限时，负责清退的节点会记录 `same-ip active node rejected`；Hysteria2 节点还会记录 `disconnecting hysteria2 client rejected by distributed limit`。

Hysteria2 被清退的 QUIC 来源地址会持续丢包，直到该连接真正触发 `Disconnect` 后才释放本地封禁；这样被判定超限的既有连接不会在固定丢包窗口结束后自行恢复数据传输。

## 运维建议

Redis 里保存的是临时在线状态，丢失可以接受；用户下一次连接时会重新统计。

同一个面板的所有节点建议显式配置相同 `Scope`。如果一个节点写 `https://panel.example.com`，另一个节点写 `http://127.0.0.1`，默认 scope 会不同，导致不能共享同一份在线 IP 状态。

`Timeout` 建议保持较短，通常 `100` 到 `300` 毫秒即可。Redis 慢或不可达时，V2bX 会 fail-open。

多数部署可以使用：

```text
TTL = 120
RefreshInterval = 20
RejectCacheTTL = 3
FailureCooldown = 30
IPv6Prefix = 128
```

如果 IPv6 用户因为隐私地址被误判成多个 IP，可以改成：

```json
"IPv6Prefix": 64
```

## 验证

从 V2bX 节点测试 Sentinel：

```bash
redis-cli -h 10.66.0.2 -p 26379 SENTINEL get-master-addr-by-name mymaster
```

测试当前 Redis master：

```bash
redis-cli -h 10.66.0.2 -p 26379 SENTINEL get-master-addr-by-name mymaster

MASTER_IP=$(redis-cli -h 10.66.0.2 -p 26379 --raw SENTINEL get-master-addr-by-name mymaster | head -n 1)
redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' PING
```

初始部署时 master 通常是 `10.66.0.2`；发生故障转移后，master 可能变成 `10.66.0.3` 或 `10.66.0.4`，所以建议先从 Sentinel 查询当前 master。

用户连接后查看在线 IP key：

```bash
MASTER_IP=$(redis-cli -h 10.66.0.2 -p 26379 --raw SENTINEL get-master-addr-by-name mymaster | head -n 1)

redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' --scan --pattern 'v2bx:online_ip:*'
redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' ZRANGE '<key>' 0 -1 WITHSCORES
```

开启同 IP 活跃节点限制后，用户在节点上持续连接超过 `ActivationDelay` 后查看正式节点租约。Redis key 最后一段是规范化来源 IP 的短哈希，同一来源 IP 的节点都在同一个集合中：

```bash
redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' --scan --pattern 'v2bx:same_ip_active_node:*'
redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' ZRANGE '<active-node-key>' 0 -1 WITHSCORES
redis-cli -h "$MASTER_IP" -a '<REDIS_PASSWORD>' ZRANGE '<active-node-key>:blocked' 0 -1 WITHSCORES
```

套餐限制为 3 时，3 个长期连接节点应出现在正式 key；第 4 个节点可以在观察期短暂使用，但持续超过观察期后会被清退。此后名额仍满时，它或其它额外新节点会立即被拒绝，不会每隔 60 秒重新连接再掉线。

模拟 Redis 故障：

```bash
# 在当前 master 所在机器上执行，测试 Sentinel 是否会自动切主
systemctl stop redis-server

# 从另一台 Redis/Sentinel 机器查询新 master
redis-cli -h 10.66.0.3 -p 26379 SENTINEL get-master-addr-by-name mymaster

# 测试完成后恢复原机器 Redis
systemctl start redis-server
```

如果要模拟 Sentinel/Redis 全部不可用，可以在维护窗口内停止相关服务。此时 V2bX 应该出现类似日志：

```text
online ip redis unavailable, fail-open
same-ip active node redis unavailable, fail-open
```

此时已有连接和新连接都应该继续放行。测试完成后重新启动 Redis 和 Sentinel。

## 安全检查清单

- Redis 和 Sentinel 只监听 WireGuard IP 或本机地址。
- 同一面板中需要参与限制的所有 Xray/Hysteria2 节点都配置相同的 `SameIPActiveNodeLimit` 参数。
- 公网防火墙禁止访问 `6379` 和 `26379`。
- WireGuard 内网防火墙允许 V2bX 节点访问 Redis/Sentinel 的 `6379` 和 `26379`。
- Redis 使用强密码或 ACL 用户。
- WireGuard peer 尽量使用 `/32` `AllowedIPs`。
- Redis 服务器优先选择稳定机器，不建议放在最繁忙、最容易被攻击的代理节点上。
- 所有 V2bX 节点启用 NTP/chrony 时间同步，以稳定判断超限租约的先后次序。
- 监控 Redis 延迟、Sentinel failover、V2bX fail-open 日志。
