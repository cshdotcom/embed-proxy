# embed-proxy

SiliconFlow Embedding 清洗代理。解决 OpenWebUI / LiteLLM 调用硅基流动 `/v1/embeddings` 时因注入 `"encoding_format": null` 导致返回 `20015 参数非法` 的问题。

## 功能

- 接收 `POST /v1/embeddings`，删除 `encoding_format` 字段后转发至硅基流动
- 默认监听 `0.0.0.0:16540`，可被 gost / frp / cloudflared 等内网穿透工具直接转发
- 启动弹出交互菜单：一键安装 systemd 系统服务（开机自启）、卸载、改端口、设置 API Key
- 重装/升级服务时**自动合并保留已有配置**（API Key / 端口 / 上游地址不丢失）
- 单文件静态二进制，无任何外部依赖

## 快速开始

```bash
# 下载对应架构二进制后
chmod +x embed-proxy
sudo ./embed-proxy
```

菜单操作：

1. `4` 设置硅基 API Key
2. `1` 安装为系统服务（开机自启动，自动保留已有配置）
3. `3` 修改监听端口（默认 16540）
4. `2` 卸载系统服务（默认保留配置）

## OpenWebUI 接入

管理员 → 设置 → 文档：

- 语义向量模型引擎：`OpenAI`
- 语义向量模型：`BAAI/bge-m3`
- 基础 URL：`http://<服务器IP>:16540/v1`
  （OpenWebUI 以 Docker 运行时，加 `--add-host=host.docker.internal:host-gateway`，填 `http://host.docker.internal:16540/v1`）
- API Key：任意值（代理使用配置文件中的真实 Key）

## 配置文件

路径：`/etc/embed-proxy/config.json`（可用环境变量 `EMBED_PROXY_CONFIG` 覆盖）

```json
{
  "port": 16540,
  "siliconflow_api_key": "sk-xxx",
  "upstream_base": "https://api.siliconflow.cn/v1"
}
```

环境变量 `SILICONFLOW_API_KEY`、`EMBED_PROXY_PORT` 优先于配置文件。

## 构建

```bash
go build -trimpath -ldflags "-s -w" -o embed-proxy main.go
```

## 测试

```bash
go test -v ./...
```

## License

MIT
