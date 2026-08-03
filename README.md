# 拾光集 · 家庭照片时间轴

单二进制 Go 应用：前台暗房时间轴（`/`）+ 管理后台（`/admin`）+ REST API。
单机部署、单管理员 + 家人浏览；照片是不可再生资产，正确性与数据安全优先。

- 纯 Go 编译（`CGO_ENABLED=0`）：SQLite 用 `modernc.org/sqlite`，webp 编码用
  `gen2brain/webp`（wazero 运行 wasm 版 libwebp）
- 两种对象存储：`local`（本地磁盘）/ `s3`（MinIO / R2 / OSS / COS）
- 前端零构建：两页原生 HTML 经 `go:embed` 打入二进制

## 快速开始

```bash
# 源码运行
SG_ADMIN_TOKEN=my-secret-token go run ./cmd/server
# 打开 http://localhost:8080 （前台）与 http://localhost:8080/admin （后台）

# Docker
cp .env.example .env    # 编辑 SG_ADMIN_TOKEN 等
docker compose up -d
```

## 配置表（环境变量）

| 变量 | 默认 | 说明 |
|------|------|------|
| `SG_ADDR` | `:8080` | 监听地址 |
| `SG_DB_DSN` | `file:data/shiguang.db` | SQLite 路径（`file:` DSN；WAL/busy_timeout 等 PRAGMA 由程序自动追加） |
| `SG_ADMIN_TOKEN` / `SG_ADMIN_TOKEN_FILE` | — | **必填其一**。管理口令（写接口 Bearer token） |
| `SG_PUBLIC_READ` | `false` | `true`=家人免登录浏览；`false`=读接口也要 token，变体 URL 带 HMAC 签名 |
| `SG_SIGN_SECRET` | — | `SG_PUBLIC_READ=false` 时必填，签名 local 变体 URL |
| `SG_BLOB_DRIVER` | `local` | `local` \| `s3` |
| `SG_BLOB_LOCAL_ROOT` | `data/blobs` | local 模式存储根目录 |
| `SG_S3_ENDPOINT` | — | s3 端点（MinIO 如 `http://minio:9000`；留空=AWS） |
| `SG_S3_BUCKET` / `SG_S3_REGION` / `SG_S3_AK` / `SG_S3_SK` | — | s3 模式必填（region 默认 `us-east-1`） |
| `SG_S3_PATH_STYLE` | `false` | MinIO 需 `true` |
| `SG_S3_CDN_BASE` | — | 有 CDN 时变体走 `CDN_BASE/<key>`，否则 PresignGet(10min) |
| `SG_LIMIT_UPLOAD_MB` | `30` | 单张上限 |
| `SG_LIMIT_PIXELS_MP` | `60` | 像素上限（pixel bomb 防护） |
| `SG_TRASH_TTL_DAYS` | `7` | 回收站保留天数 |
| `SG_WORKERS` | `0` | 处理 worker 数，0=NumCPU |

启动即校验：缺必填项 fail-fast 并打印配置示例。

## 部署

### local 模式（默认，最简单）

```bash
cp .env.example .env         # 设 SG_ADMIN_TOKEN；SG_BLOB_DRIVER=local
docker compose up -d         # app + litestream（DB 备份）
```

照片与数据库都在 `data` 卷：`/data/shiguang.db` + `/data/blobs/`。

### s3 模式（MinIO 演示）

```bash
# .env 里：
#   SG_BLOB_DRIVER=s3
#   SG_S3_ENDPOINT=http://minio:9000  SG_S3_BUCKET=shiguang
#   SG_S3_AK=minioadmin  SG_S3_SK=minioadmin  SG_S3_PATH_STYLE=true
docker compose --profile s3 up -d
# 首次需建桶：
docker compose exec minio mc alias set local http://localhost:9000 minioadmin minioadmin
docker compose exec minio mc mb local/shiguang
```

s3 模式上传走 presign 直传：浏览器 PUT 到对象存储，不过应用带宽。

## curl 冒烟脚本

```bash
BASE=http://localhost:8080
TOKEN=my-secret-token
AUTH="Authorization: Bearer $TOKEN"

# 0. 健康检查
curl -sf $BASE/healthz

# 1. 建节点
NODE=$(curl -sf -X POST $BASE/api/v1/nodes -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"date":"2026-05-21","title":"黄山两日","description":"云海来得比预报早"}' | jq -r .id)

# 2a. local 模式上传（multipart，202）
PHOTO=$(curl -sf -X POST $BASE/api/v1/nodes/$NODE/photos -H "$AUTH" \
  -F file=@photo.jpg -F caption=云海翻涌 | jq -r .id)

# 2b. s3 模式上传（presign → PUT → confirm）
PRES=$(curl -sf -X POST $BASE/api/v1/uploads/presign -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"node_id\":\"$NODE\",\"filename\":\"photo.jpg\",\"size\":$(stat -c%s photo.jpg),\"content_type\":\"image/jpeg\"}")
curl -sf -X PUT "$(echo $PRES | jq -r .upload_url)" \
  -H 'Content-Type: image/jpeg' --data-binary @photo.jpg
PHOTO=$(echo $PRES | jq -r .photo_id)
curl -sf -X POST $BASE/api/v1/photos/$PHOTO/confirm -H "$AUTH"

# 3. 轮询直到 ready
watch -n1 "curl -s $BASE/api/v1/photos/$PHOTO -H '$AUTH' | jq .status"

# 4. 时间轴 / 统计 / 回收站
curl -s "$BASE/api/v1/timeline?limit=10" -H "$AUTH" | jq .
curl -s  $BASE/api/v1/stats -H "$AUTH" | jq .
curl -s  $BASE/api/v1/trash -H "$AUTH" | jq .

# 5. 改注 / 排序 / 软删 / 恢复
curl -s -X PATCH  $BASE/api/v1/photos/$PHOTO -H "$AUTH" -H 'Content-Type: application/json' -d '{"caption":"新图注"}'
curl -s -X PUT    $BASE/api/v1/nodes/$NODE/photos/order -H "$AUTH" -H 'Content-Type: application/json' -d "{\"photo_ids\":[\"$PHOTO\"]}"
curl -s -X DELETE $BASE/api/v1/photos/$PHOTO -H "$AUTH"
curl -s -X POST   $BASE/api/v1/photos/$PHOTO/restore -H "$AUTH"

# 6. 秒传验证（再传同文件应 409 + 已存在照片）
curl -s -o /dev/null -w '%{http_code}\n' -X POST $BASE/api/v1/nodes/$NODE/photos -H "$AUTH" -F file=@photo.jpg
```

## 备份恢复手册

**要备份的两样东西：**

1. **SQLite 主库**（元数据）— litestream 实时复制，秒级 RPO：

   ```bash
   # 恢复：停 app → 从副本还原 → 重启
   docker compose stop app
   docker compose run --rm litestream restore -config /etc/litestream.yml -o /data/shiguang.db /data/shiguang.db
   docker compose start app
   ```

2. **照片 blob**（不可再生资产）：
   - local 模式：定期 rsync/restic 备份 `data` 卷的 `blobs/` 目录：
     `rsync -a --delete <volume>/blobs/ backup-host:/backups/shiguang-blobs/`
   - s3 模式：开启桶版本控制 + 跨区域复制，或 `mc mirror` 到第二桶。

   blob 是内容寻址（`orig/ab/cd/<sha256>.<ext>`），备份天然增量、可去重、无覆盖风险。

**恢复演练建议**：每季度在干净机器上 restore DB + 同步 blobs 目录 → 启动 →
打开前台确认照片完整。孤儿对账 GC 会自动清掉多余对象，缺失对象会在照片打开时暴露。

**崩溃一致性**：上传中途 kill 进程不会产生半截数据——local 写入走
临时文件 + fsync + rename；DB 记录晚于 blob 落盘；启动时自动把卡在
processing/pending 的照片重新入队，过期上传会话由 reaper 每 5 分钟清理。

## 人工验收清单

1. `docker compose up` 后 `/` 能看到时间轴、`/admin` 能登录；
2. 管理台完整走一遍：错误 token 被拒（抖动报错）→ 正确 token 进入 → 建节点 →
   拖 3 张真实 jpg 进显影盘（有进度条）→ 全部 ready → 改注 → 拖拽排序 →
   删 1 张 → 回收站恢复；刷新前台时间轴，以上变化全部可见且顺序正确；
3. 传 .txt 改名 .jpg → 前后台均出现「曝光失败」，重试（reprocess/重传）路径可用；
4. `SG_BLOB_DRIVER=s3` 指向本地 MinIO 重启，重复第 2 步全部通过；
5. 上传中途 `docker kill` 再启动：无半截数据，卡住任务自动恢复（观察日志
   `recovered stuck photos`）；
6. 变体响应头含 `immutable`；`SG_PUBLIC_READ=false` 时未带签名的变体 URL 403；
   `GET /healthz` 返回 200。

## 测试

```bash
go build ./... && go vet ./... && go test ./...

# s3 契约测试需要 MinIO：
docker run -d -p 9000:9000 minio/minio server /data
SG_TEST_S3_ENDPOINT=http://localhost:9000 SG_TEST_S3_BUCKET=shiguang-test go test ./internal/blob/
```

覆盖：EXIF 8 方向矫正、灰度 png、截断文件、60MP 伪造头、blob 契约
（fake/local/s3 同套件 + 防穿越）、上传→ready 全流程、409 秒传、跨节点共享
blob 防误删、reaper、启动恢复、游标分页不重不漏、trash 恢复、鉴权、429 限流、
413/415、响应结构。

## 实现取舍清单

按提示词「有歧义时在代码注释中说明取舍」汇总：

1. **webp 编码**：硬约束纯 Go（modernc sqlite 暗示 CGO_ENABLED=0），而 webp
   有损编码无纯 Go 实现，选 `gen2brain/webp`（wazero 跑 wasm libwebp，q80 有损，
   保持单二进制静态编译）。
2. **DSN 形式**：`modernc.org/sqlite` 用 `_pragma=key(value)` 传参，与提示词示例
   的 mattn 风格（`_journal=WAL`）不同；程序统一自动追加所需 PRAGMA
   （WAL/busy_timeout/foreign_keys/synchronous），配置只需给 `file:` 路径。
3. **登录验证语义**：管理台用 `GET /stats` 带 Bearer 验证口令，但公开读模式下
   stats 本可匿名——因此读接口约定为「带了 token 就必须有效，否则 401」，
   错误口令在公开模式下也会被拒，登录验证始终有效。
4. **GC 删 blob 的引用检查**：比提示词更严格——`COUNT(*)` 不限 `deleted_at IS NULL`，
   只要还有任何行（含未到期回收站条目）引用该 sha 就保留 blob，否则恢复会断链。
5. **孤儿对账的 mtime**：blob 接口无 mtime；local 驱动额外提供 `MTime`（>48h 判龄），
   s3 驱动用进程内两阶段标记（相邻两次 GC 均为孤儿且间隔 >48h 才删），
   重启丢标记只会推迟删除，方向安全。
6. **变体原子写**：提示词要求「临时 key 再 Rename 覆盖」——local 驱动 `Put` 内部
   即临时文件 + fsync + rename，s3 `Put` 本身原子，故变体直接 `Put`，语义等价。
7. **worker 领取**：DB 抢占更新（`status IN (pending,processing)` 影响行数=1）+
   进程内 in-flight 去重防同照片并发；单机单进程下足够。
8. **恢复时机**：启动恢复不设 10 分钟门槛（崩溃遗留的 processing 必然是孤儿），
   周期重扫（并入 reaper 5 分钟节拍）用 10 分钟门槛。
9. **节点恢复语义**：级联软删用同一 `deleted_at` 时间戳标记「随节点删除」的照片；
   恢复节点只带回这批照片，更早单独删除的照片仍留在回收站。
10. **stats 扩展字段**：响应加了 `upload_mode: local|s3`，管理台据此选择上传路径
    （multipart 或 presign），避免探测式调用。
11. **秒传的 s3 路径**：confirm 时发现同节点同 sha 已存在 → 删暂存对象、硬删
    pending 行、409 返回已存在照片（与 local 模式语义一致）。
12. **原图 EXIF**：原图原样保存（含 GPS，`orig/` 永不直出）；变体重编码天然剥离
    全部元数据，对外只出变体。
13. **前台私有读**：`SG_PUBLIC_READ=false` 时前台时间轴需要 token，页面显示
    「相册未公开，需要访问授权」——单管理员场景下私有浏览走管理台。
14. **限流实现**：内存令牌桶（全局 50 rps burst 100；上传 10 次/分钟 burst 10），
    单机部署无需分布式限流。

## 项目结构

```
shiguang/
├── cmd/server/           # 装配、配置校验、优雅退出（drain worker → close db）
├── internal/
│   ├── httpapi/          # 路由 + 中间件（auth/ratelimit/reqid/recover/log）+ /img
│   ├── service/          # 业务编排；DB 与 blob 一致性责任集中在此
│   ├── store/            # SQLite 访问（读写双池，写连接上限 1）
│   ├── blob/             # Store 接口 + local / s3 / fake 三驱动
│   ├── imgproc/          # 校验/EXIF 矫正/webp 变体/BlurHash + worker 池
│   └── jobs/             # 启动恢复 · reaper(5min) · GC(每日)
├── migrations/           # goose SQL（embed，启动自动执行）
└── web/                  # index.html + admin.html（embed，零构建）
```
