# V2bX

一个基于多种内核的V2board节点服务端，修改自XrayR，支持V2ay,Trojan,Shadowsocks协议。

**注意： 本项目需要搭配[修改版V2board](https://github.com/tristesky/V2bX)**

## 特点

* 永久开源且免费。
* 支持Vmess/Vless, Trojan， Shadowsocks, Hysteria1/2多种协议。
* 支持Vless和XTLS等新特性。
* 支持单实例对接多节点，无需重复启动。
* 支持限制在线IP。
* 支持限制Tcp连接数。
* 支持节点端口级别、用户级别限速。
* 配置简单明了。
* 修改配置自动重启实例。
* 支持多种内核，易扩展。
* 支持条件编译，可仅编译需要的内核。

## 功能介绍

| 功能        | v2ray | trojan | shadowsocks | hysteria1/2 |
|-----------|-------|--------|-------------|----------|
| 自动申请tls证书 | √     | √      | √           | √        |
| 自动续签tls证书 | √     | √      | √           | √        |
| 在线人数统计    | √     | √      | √           | √        |
| 审计规则      | √     | √      | √           | √         |
| 自定义DNS    | √     | √      | √           | √        |
| 在线IP数限制   | √     | √      | √           | √        |
| 连接数限制     | √     | √      | √           | √         |
| 跨节点IP数限制  |√      |√       |√            |√          |
| 按照用户限速    | √     | √      | √           | √         |
| 动态限速(未测试) | √     | √      | √           | √         |

## TODO

- [ ] 重新实现动态限速
- [ ] 完善使用文档

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/tristesky/V2bX-script/master/install.sh && bash install.sh
```

### 手动安装

[手动安装教程](https://v2bx.v-50.me/v2bx/v2bx-xia-zai-he-an-zhuang/install/manual)

## 构建
``` bash
# 通过-tags选项指定要编译的内核， 可选 xray， sing, hysteria2
GOEXPERIMENT=jsonv2 go build -v -o build_assets/V2bX -tags "sing xray hysteria2 with_quic with_grpc with_utls with_wireguard with_acme with_gvisor" -trimpath -ldflags "-X 'github.com/InazumaV/V2bX/cmd.version=$version' -s -w -buildid="
```

## 配置文件及详细使用教程

[详细使用教程](https://v2bx.v-50.me/)

### Redis 分布式在线 IP 与活跃节点限制

`LimitConfig.OnlineIPLimit` 可以开启基于 Redis 的跨节点在线 IP 限制。不配置或 `Enable` 为 `false` 时保持原有本地限制逻辑。
`LimitConfig.ActiveNodeLimit` 可以进一步限制同一用户长期同时使用的节点数量：短时间测速连接不会正式占位，持续超限的节点会被清退并进入冷却拒绝状态。

推荐使用 Redis 主从 + Sentinel，并让各 V2bX 节点通过 WireGuard/VPN 内网地址连接 Sentinel：

```json
"LimitConfig": {
  "OnlineIPLimit": {
    "Enable": true,
    "Type": "redis",
    "KeyPrefix": "v2bx:online_ip",
    "TTL": 120,
    "RefreshInterval": 20,
    "RejectCacheTTL": 3,
    "Timeout": 200,
    "FailureCooldown": 30,
    "IPv6Prefix": 128,
    "RedisConfig": {
      "Addresses": [
        "10.10.0.2:26379",
        "10.10.0.3:26379",
        "10.10.0.4:26379"
      ],
      "MasterName": "mymaster",
      "Password": "",
      "Db": 0
    }
  },
  "ActiveNodeLimit": {
    "Enable": true,
    "Limit": 0,
    "ActivationDelay": 60,
    "BlockTTL": 600,
    "KeyPrefix": "v2bx:active_node"
  }
}
```

`ActiveNodeLimit.Limit` 为 `0` 时自动使用面板给每个用户下发的 `device_limit`，无需按套餐在节点手工配置数字。Redis/Sentinel 不可用时两种分布式限制都 fail-open，避免 Redis 故障导致全站不可用。`ActiveNodeLimit` 未填写 Redis 参数时会继承 `OnlineIPLimit` 的连接和超时配置，但使用独立 Redis key。

完整部署步骤见 [Redis 分布式在线 IP 限制部署文档](docs/redis-online-ip.md)。

## 免责声明

* 此项目用于本人自用，因此本人不能保证向后兼容性。
* 由于本人能力有限，不能保证所有功能的可用性，如果出现问题请在Issues反馈。
* 本人不对任何人使用本项目造成的任何后果承担责任。
* 本人比较多变，因此本项目可能会随想法或思路的变动随性更改项目结构或大规模重构代码，若不能接受请勿使用。

## 赞助

[赞助链接](https://v-50.me/)

## Thanks

* [Project X](https://github.com/XTLS/)
* [V2Fly](https://github.com/v2fly)
* [VNet-V2ray](https://github.com/ProxyPanel/VNet-V2ray)
* [Air-Universe](https://github.com/crossfw/Air-Universe)
* [XrayR](https://github.com/XrayR/XrayR)
* [sing-box](https://github.com/SagerNet/sing-box)

## Stars 增长记录

[![Stargazers over time](https://starchart.cc/wyx2685/V2bX.svg)](https://starchart.cc/wyx2685/V2bX)
