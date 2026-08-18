# cdn-pool

SOCKS5h 代理：名单里的域名不走系统 DNS，改用 IP 池拨号。池子来自 `ip-file` 的静态地址，和/或 `cidr-file` 里每个网段当 ECS 去查 `cname`。
客户端自己做 TLS、自己带 Header，代理只负责选 IP。

## 配置 `config.yaml`

```yaml
listen: "0.0.0.0:1080"
cidr-file: "cidr.txt"      # 可省略；文件不存在或空都行
ip-file: "ip.txt"          # 可省略；静态 IPv4，直接进池
hosts:
  - cdn.example.com
  # - "*.example.com"
cname: "cdn.example.com"   # 只用 ip-file 时可省略
dns: "223.5.5.5:53"
refresh: 10m
max-fails: 3
cooldown: 30s
dial-timeout: 8s
```

两个文件都允许不存在或空。至少要有一边能凑出池子：

- `ip-file`：一行一个 IPv4，`3.164.64.1` 或 `3.164.64.1/32`，直接进池，不查 DNS。
- `cidr-file`：一行一个网段，前缀按官方回源段原样写（`114.232.93.0/25`）。不写掩码时按 `/24`。用 ECS（`SourceNetmask` = 文件前缀）查 `cname`，解析结果并进池。这时 `cname` 必填。
- 两边都有就合并去重。只有静态 IP、没有 CIDR 时不必配 `cname`，也不会定时刷新。

| 客户端要连 | 代理 |
|---|---|
| `hosts` 里的域名 | 轮询 ECS 解析出的 IP，拨 `选中IP:原端口` |
| 其它域名 | 正常 DNS，直连 |
| 字面 IP | 原样拨（除非 `hosts` 里写了 `*`） |

## 运行

```bash
go run . -c config.yaml
# 或
docker compose up -d
```

必须用 **SOCKS5h**（域名在代理侧解析），否则客户端自己查 DNS，池子用不上：

```bash
curl --socks5-hostname 127.0.0.1:1080 https://cdn.example.com/cdn-cgi/trace

export ALL_PROXY=socks5h://127.0.0.1:1080
curl https://cdn.example.com/cdn-cgi/trace
```

## Docker

```bash
docker compose up -d
```

`docker-compose.yml` 挂本地的 `config.yaml`、`cidr.txt`，需要静态池再挂 `ip.txt`。

```bash
docker run --rm -p 1080:1080 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/cidr.txt:/app/cidr.txt \
  ghcr.io/myflv/cdn-pool:latest
```

发布由 tag 触发：`git tag v0.1.0 && git push origin v0.1.0`。

## 行为

- 只实现 SOCKS5 CONNECT，无认证。
- 匹配 `hosts` 的域名：忽略真实 DNS，从池子轮询，保留客户端端口。静态 IP 原样用；CIDR 走 ECS，前缀按文件，不强制 `/24`。`cname` 只在要用 ECS 时才需要。
- 某个 IP 连续失败 `max-fails` 次后冷却 `cooldown`，到期自动加回。
- 原生 DNS + ECS（`miekg/dns`）定期刷新池子。
