# cdn-pool

SOCKS5h 代理：名单里的域名不走系统 DNS，改用 `cidr.txt` 里每个网段当 ECS 去查 `cname`，轮询解析出的边缘 IP 再拨号。
客户端自己做 TLS、自己带 Header，代理只负责选 IP。

## 配置 `config.yaml`

```yaml
listen: "0.0.0.0:1080"
cidr-file: "cidr.txt"
hosts:
  - cdn.example.com
  # - "*.example.com"
cname: "cdn.example.com"   # ESA CNAME，必填
dns: "223.5.5.5:53"
refresh: 10m
max-fails: 3
cooldown: 30s
dial-timeout: 8s
```

`cidr.txt` 一行一个 IPv4 网段，前缀按官方回源段原样写（`114.232.93.0/25`、`117.49.26.96/27`）。不写掩码时按 `/24`。`#` 开头是注释。同一网段只保留一次，不同前缀不会被收成同一个 `/24`。

启动时用这些网段做 ECS（`SourceNetmask` = 文件里的前缀长度）查 `cname` 填池；之后每 `refresh` 再查一遍并热替换。

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

`docker-compose.yml` 挂本地的 `config.yaml` 和 `cidr.txt`。

```bash
docker run --rm -p 1080:1080 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/cidr.txt:/app/cidr.txt \
  ghcr.io/myflv/cdn-pool:latest
```

发布由 tag 触发：`git tag v0.1.0 && git push origin v0.1.0`。

## 行为

- 只实现 SOCKS5 CONNECT，无认证。
- 匹配 `hosts` 的域名：忽略真实 DNS，从 ECS 解析出的 IP 轮询，保留客户端端口。ECS 使用文件中的官方前缀，不再强制 `/24`。
- 某个 IP 连续失败 `max-fails` 次后冷却 `cooldown`，到期自动加回。
- 原生 DNS + ECS（`miekg/dns`）定期刷新池子。
