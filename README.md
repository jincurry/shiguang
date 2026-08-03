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
| `SG_HTTP_PORT` | `8080` | Docker 部署时映射到宿主机的访问端口 |
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
| `SG_LIMIT_GLOBAL_RPS` | `50` | 全局限流（次/秒） |
| `SG_LIMIT_UPLOAD_RPM` | `120` | 上传端点限流（次/分钟）。批量导入几千张时可调到 `600` |

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

## 批量导入照片（sgctl）

后台的显影盘适合日常加几十张（支持整个文件夹拖入，带并发队列和总进度条）。
成百上千张的老照片入库用 `sgctl`——单文件纯 Go 二进制，**放在任何能访问服务的
机器上都能跑**，不需要和服务同机。

```bash
# 构建（或用 make dist 交叉编译出各平台版本到 dist/）
make sgctl                    # → bin/sgctl
make dist                     # → dist/sgctl-{darwin,linux,windows}-{amd64,arm64}

# 先看一眼会怎么分组，不上传任何东西
sgctl import ~/Photos --dry-run

# 真正导入
export SG_ADMIN_TOKEN=your-token
sgctl import ~/Photos --server https://photos.example.com
```

输出示例：

```
已连接 https://photos.example.com（存储模式：local）
上传中 31/31  已传 31  已存在 0  失败 0

完成，用时 12.4s
  节点：新建 5 个，复用 0 个
  照片：上传 31 张，已存在跳过 0 张，失败 0 张
```

**分组方式**（`--group`，默认 `auto`）：

| 模式 | 行为 |
|------|------|
| `auto` | 子目录里的照片按目录名归组，直接躺在根目录的按 EXIF 拍摄日期归组。最贴合"部分整理过"的照片库 |
| `date` | 一律按 EXIF 拍摄日期（无 EXIF 时用文件修改时间） |
| `folder` | 一律按第一层目录名，根目录散图归入「未分类」 |
| `single` | 全部放进一个节点（配 `--title` 指定标题），之后在后台慢慢分 |

节点日期取组内最早的拍摄时间，组内照片按拍摄时间升序排列。

**其他要点**：

- **可中断、可重跑**。照片按 sha256 内容寻址去重，中断后重跑同一条命令会自动
  跳过已导入的（计入"已存在"而非失败），只补没传完的部分。Ctrl-C 安全退出。
- **同名节点会复用**，不会因为重跑造出一堆重复节点（按 `日期+标题` 匹配）。
- **按魔数而非扩展名判定类型**：改名的 `.txt` 在扫描阶段就被剔除，不浪费上传；
  目录里混着的文档、视频静默跳过；`.thumbnails`、`@eaDir`、`$RECYCLE.BIN` 等
  系统目录整个跳过。
- **429 自动退避重试**（尊重 `Retry-After`），所以并发调高也不会传失败。
  导入几千张时建议服务端把 `SG_LIMIT_UPLOAD_RPM` 调到 `600`，客户端
  `--concurrency 8`。
- s3 模式下自动走 presign 直传，照片不经过应用服务器。

### 指定日期与文案（清单文件，可选）

自动推导出的节点日期、标题和图注不一定合心意。在照片目录里放一个
`shiguang.txt` 就能覆盖它们——**没有这个文件时，一切按自动推导走，行为与
不用清单时完全一致**。

```bash
sgctl import ~/Photos --emit-manifest   # 按当前推导结果生成模板（不覆盖已有）
# 用文本编辑器改完，再正常导入
sgctl import ~/Photos --server https://photos.example.com
```

清单格式刻意做得极简，值写到行尾即可，不需要引号和转义：

```
# 以 # 开头的行是注释；删掉某一行 = 该项回到自动推导
date = 2019-10-06
title = 小妹的婚礼
description = 全家去了三亚，海边办的仪式

# 每张照片的图注：文件名 = 图注
DSC1000.jpg = 接亲那天早上
DSC1001.jpg = 海边仪式
```

生效规则（只有两条）：

- **图注**按文件名匹配清单所在目录内的照片，任何 `--group` 模式下都生效；
  没写的照片仍回落到文件名。文件名大小写不敏感。
- **`date` / `title` / `description`** 只在该目录恰好对应一个节点时生效
  （`auto`/`folder` 模式的第一层子目录、`single` 模式的根目录）。否则会明确
  提示"未生效"，而不是悄悄套到某个节点上。

其他细节：日期非 `YYYY-MM-DD`、标题超 120 字、图注超 200 字都会给出警告并
忽略或截断；清单里写了目录下不存在的文件名会被报出来（多半是拼错）；支持
Windows 记事本存的 UTF-8 BOM。**限制**：文件名本身含 `=` 时无法在清单中表示
（按首个 `=` 切分键值），这种文件请先改名——工具会明确报出来而不是静默存错。

**日期可信度提示**：`--dry-run` 会用 `⚠` 标出「日期来自文件修改时间而非 EXIF
拍摄时间」的节点，用 `✎` 标出由清单指定日期的节点。扫描件、被聊天软件转发过的照片通常没有 EXIF，这类节点的
日期多半不是真实拍摄日，导入后需要在后台核对。建议先看 dry-run 再决定是
调整文件夹结构，还是导入后手工改日期。

导入后想重新归类，管理后台每张照片的悬浮操作里有 `⇄`（移到其他节点），
点开选目标节点即可；对应 API 是 `PATCH /photos/{id}` 带 `node_id`。

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

# 5. 改注 / 移动到其他节点 / 排序 / 软删 / 恢复
curl -s -X PATCH  $BASE/api/v1/photos/$PHOTO -H "$AUTH" -H 'Content-Type: application/json' -d '{"caption":"新图注"}'
curl -s -X PATCH  $BASE/api/v1/photos/$PHOTO -H "$AUTH" -H 'Content-Type: application/json' -d "{\"node_id\":\"$OTHER_NODE\"}"
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
blob 防误删、跨节点移动照片（含目标重复 409）、reaper、启动恢复、游标分页
不重不漏、trash 恢复、鉴权、429 限流与批量首波不被限流、413/415、超长文件名
图注截断、响应结构、导入扫描的魔数过滤与四种分组模式、清单解析与套用
（含无清单时行为不变、模板不覆盖用户改动、拼错文件名告警）。

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
14. **限流实现**：内存令牌桶（全局 50 rps；上传 120 次/分钟），两者均可经
    `SG_LIMIT_GLOBAL_RPS` / `SG_LIMIT_UPLOAD_RPM` 调整，单机部署无需分布式限流。
    上传默认值从最初的 10 次/分钟提高到 120——上传 handler 只做落盘 + 入队，
    真正的 CPU 消耗由 worker 池自行限流，过低的上传限流只会卡死批量导入。
15. **批量上传的并发**：后台显影盘用并发上限 3 的队列（而非一次全部发出），
    429 一律指数退避重试而不是判失败；CLI 默认并发 4、可用 `--concurrency` 调。
16. **显影轮询按节点而非按照片**：`GET /nodes/{id}` 一次带回该节点全部照片
    状态，所以一个节点里传 200 张也只是每 3 秒 1 个请求。
17. **导入的类型判定**：客户端按魔数嗅探而非扩展名，改名的 `.txt` 在扫描阶段
    就剔除，不浪费一次上传往返；EXIF 只读文件头 64KB（EXIF 段在开头），
    不把整张原图读进内存。
18. **导入的节点复用**：按 `日期+标题` 匹配已有节点，避免重跑造出重复节点；
    照片去重靠内容寻址（同节点同 sha 返回 409），两者共同保证重跑幂等。
19. **清单文件格式**：自造的行式格式而非 TOML/YAML——避免为一个可选功能引入
    解析依赖，且 `文件名 = 中文图注` 无需引号转义，比 JSON 好手写得多。
    代价是文件名含 `=` 无法表示，故做成显式报错而非静默切错。
20. **清单的作用域规则**：节点级字段只在目录唯一对应一个节点时生效，否则宁可
    不生效并告警——多份清单争抢同一节点会产生取决于遍历顺序的结果，
    对不可再生资产而言，可预测比"尽量生效"更重要。

## 项目结构

```
shiguang/
├── cmd/server/           # 装配、配置校验、优雅退出（drain worker → close db）
├── cmd/sgctl/            # 命令行工具：批量导入
├── internal/
│   ├── importer/         # 扫描 / EXIF 分组 / API 客户端 / 并发导入
│   ├── httpapi/          # 路由 + 中间件（auth/ratelimit/reqid/recover/log）+ /img
│   ├── service/          # 业务编排；DB 与 blob 一致性责任集中在此
│   ├── store/            # SQLite 访问（读写双池，写连接上限 1）
│   ├── blob/             # Store 接口 + local / s3 / fake 三驱动
│   ├── imgproc/          # 校验/EXIF 矫正/webp 变体/BlurHash + worker 池
│   └── jobs/             # 启动恢复 · reaper(5min) · GC(每日)
├── migrations/           # goose SQL（embed，启动自动执行）
└── web/                  # index.html + admin.html（embed，零构建）
```
