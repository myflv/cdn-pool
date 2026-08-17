# cdn-pool

SOCKS5h 代理：名单里的域名不走系统 DNS，改从 `ip.txt` 轮询一个边缘 IP 再拨号。
客户端自己做 TLS、自己带 Header，代理只负责选 IP。

## 配置 `config.yaml`

```yaml
listen: "0.0.0.0:1080"
ip-file: "ip.txt"
hosts:
  - cdn.example.com
  # - "*.example.com"
max-fails: 3
cooldown: 30s
dial-timeout: 8s
```

`ip.txt` 一行一个 IP（不要端口，端口用客户端要连的那个）。`#` 开头是注释。

| 客户端要连 | 代理 |
|---|---|
| `hosts` 里的域名 | 轮询 `ip.txt`，拨 `选中IP:原端口` |
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

`docker-compose.yml` 挂本地的 `config.yaml` 和 `ip.txt`。

```bash
docker run --rm -p 1080:1080 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/ip.txt:/app/ip.txt \
  ghcr.io/myflv/cdn-pool:latest
```

发布由 tag 触发：`git tag v0.1.0 && git push origin v0.1.0`。

## 行为

- 只实现 SOCKS5 CONNECT，无认证。
- 匹配 `hosts` 的域名：忽略真实 DNS，从 `ip.txt` 轮询，保留客户端端口。
- 某个 IP 连续失败 `max-fails` 次后冷却 `cooldown`，到期自动加回。
