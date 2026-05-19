# Redis 分布式在线 IP 限制部署文档

本文档说明如何部署 Redis 主从 + Sentinel，并通过 WireGuard 内网为 V2bX 提供分布式在线 IP 限制。

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

节点很多时，可以使用 WireGuard hub 或现成的 overlay 网络减少 peer 管理。关键要求只有一个：所有 V2bX 节点都能访问 `10.66.0.2:26379`、`10.66.0.3:26379`、`10.66.0.4:26379`。

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

在节点配置的 `LimitConfig` 下添加：

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
  }
}
```

字段说明：

```text
Enable           是否启用 Redis 在线 IP 限制。
Scope            共享命名空间。同一套 V2Board 面板下的所有节点必须一致。
KeyPrefix        Redis key 前缀。
TTL              在线 IP 租约时间，单位秒。
RefreshInterval  本地放行缓存时间，单位秒。越小越严格，越大 Redis 请求越少。
RejectCacheTTL   被拒绝 IP 的短缓存时间，单位秒。
Timeout          Redis 操作超时，单位毫秒。
FailureCooldown  Redis 出错后 fail-open 熔断时间，单位秒。
IPv6Prefix       128 表示完整 IPv6；64 表示按 /64 统计。
Addresses        Sentinel 的 WireGuard 内网地址，不是公网地址。
MasterName       Sentinel master 名称。
Password         Redis master 密码。
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

测试 Redis master：

```bash
redis-cli -h 10.66.0.2 -a '<REDIS_PASSWORD>' PING
```

用户连接后查看在线 IP key：

```bash
redis-cli -h 10.66.0.2 -a '<REDIS_PASSWORD>' --scan --pattern 'v2bx:online_ip:*'
redis-cli -h 10.66.0.2 -a '<REDIS_PASSWORD>' ZRANGE '<key>' 0 -1 WITHSCORES
```

模拟 Redis 故障：

```bash
systemctl stop redis-server redis-sentinel
```

V2bX 应该出现类似日志：

```text
online ip redis unavailable, fail-open
```

此时已有连接和新连接都应该继续放行。测试完成后重新启动 Redis 和 Sentinel。

## 安全检查清单

- Redis 和 Sentinel 只监听 WireGuard IP 或本机地址。
- 公网防火墙禁止访问 `6379` 和 `26379`。
- Redis 使用强密码或 ACL 用户。
- WireGuard peer 尽量使用 `/32` `AllowedIPs`。
- Redis 服务器优先选择稳定机器，不建议放在最繁忙、最容易被攻击的代理节点上。
- 监控 Redis 延迟、Sentinel failover、V2bX fail-open 日志。
