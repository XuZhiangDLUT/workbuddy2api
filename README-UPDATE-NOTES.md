# 更新注意事项（fork 维护备忘）

> 本仓库是 `Sliverkiss/workbuddy2api` 的 fork，含 2 个本地补丁提交。
> 每次更新前先读这一页。

## 本地补丁清单（合并后必须确认仍在）

| 提交 | 内容 | 验证方法 |
|---|---|---|
| `c240a1b` | feat(upstream): 检测 workbuddy 把 tool-call markup 以正文形式泄漏 | `grep -c detectWorkbuddyToolMarkup internal/upstream/tool_markup.go` |
| `76a64eb` | fix(stream): tool-call markup 泄漏时同号原地重发一次 | `grep -c guardToolMarkupFields internal/upstream/sse.go`；运行中容器：`docker exec workbuddy2api grep -c guardToolMarkupFields /app/wb2api` |

合并后跑测试：`docker run --rm -v $PWD:/src -w /src golang:1.26-alpine go test ./...`

## ⚠️ 端口绑定（最容易踩的坑）

上游面向 Windows 用户，master 上的 `docker-compose.yml` 端口是 `"7863:7863"`
（绑 0.0.0.0，对公网暴露）。本部署必须只绑本机回环：

- 保留/恢复 `compose.override.yml`（`ports: !override` → `127.0.0.1:7863:7863`）。
- 上游曾在更新中删除过这个文件（2026-09-18 那次），**每次更新后都要检查**。
- 验证：`docker compose config | grep -A4 'ports:'`，host_ip 必须是 127.0.0.1；
  或 `docker ps` 里 PORTS 列必须显示 `127.0.0.1:7863->7863`。

## 标准更新流程

```bash
cd /opt/workbuddy2api
git branch backup/pre-merge-$(git rev-parse --short HEAD)        # 1) 备份分支
tar --exclude=.git -czf /root/backups/wb2api-pre-update-$(date +%Y%m%d-%H%M).tar.gz -C /opt workbuddy2api
git fetch upstream && git log --oneline HEAD..upstream/master     # 2) 看上游新提交
git merge upstream/master --no-edit                               # 3) 合并（冲突先解决）
docker run --rm -v $PWD:/src -w /src golang:1.26-alpine go test ./...   # 4) 全量测试
docker compose up -d --build                                      # 5) 重建
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7863/healthz   # 6) 健康检查
git push origin master                                            # 7) 推 fork
```

## 部署形态速记

- 容器 `workbuddy2api`，`127.0.0.1:7863`，healthcheck /healthz。
- manager（另一个项目）是 systemd 服务 `workbuddy-web.service`，监听 172.17.0.1:7864，
  **它有自己的热补丁与更新流程**：见 `/opt/workbuddy-manager/README-HOTFIX.md`。
