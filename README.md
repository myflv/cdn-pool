# cdn-pool

把请求通过 CDN 边缘 IP 池转发到 `cdn-host`。每次请求轮询一个 IP，连续失败超过 `max-fails` 次会冷却 `cooldown` 后再加回。

## 配置 `config.yaml`

```yaml
listen: "0.0.0.0:18080"
cdn-host: "cdn.example.com"
ip-file: "ip.txt"
max-fails: 3
cooldown: 30s
dial-timeout: 8s
headers:
  User-Agent: "Mozilla/5.0"
  # Authorization: "Bearer xxx"
```

`ip.txt` 一行一个 IP，支持 `1.2.3.4` 或 `1.2.3.4:443`，`#` 开头是注释。

## 运行

```bash
go run .                 # 或 go build -o cdn-pool && ./cdn-pool
go run . -c /path/to/config.yaml
```

```bash
# HTTP 代理（绝对 URL，会注入 headers，Host/SNI 改成 cdn-host）
curl -x http://127.0.0.1:18080 http://cdn.example.com/cdn-cgi/trace

# HTTPS CONNECT（透明 TCP 隧道，客户端自己做 TLS）
curl -x http://127.0.0.1:18080 https://cdn.example.com/cdn-cgi/trace

# 反向代理
curl http://127.0.0.1:18080/cdn-cgi/trace

# 池状态
curl http://127.0.0.1:18080/_pool/stats
```

## Docker

```bash
docker compose up -d
```

`docker-compose.yml` 挂本地的 `config.yaml` 和 `ip.txt`。改完配置或 IP 列表后 `docker compose up -d` 会重启。

也可以直接跑镜像：

```bash
docker run --rm -p 18080:18080 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/ip.txt:/app/ip.txt \
  ghcr.io/myflv/cdn-pool:latest
```

发布由 tag 触发：`git tag v0.1.0 && git push origin v0.1.0`。

## 行为

- HTTP / 反向代理：出站连 `ip.txt` 里的 IP:443，TLS SNI 和 HTTP Host 强制改成 `cdn-host`，`headers` 覆盖到出站请求。
- CONNECT：透明 TCP 隧道到选中的 IP，客户端自己做 TLS，改不了里面的 Header / SNI。
- 某个 IP 连续失败 `max-fails` 次后踢出 `cooldown`，到期自动加回。
