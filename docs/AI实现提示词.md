# 拾光集 · AI 实现提示词（完整版：前台 + 管理后台 + Go 后端）

> 用法：整份粘贴给编码 AI（如 Claude Code），并将随附的两个高保真原型文件
> 放入项目 `web/` 目录（`index.html`=前台时间轴，`admin.html`=管理后台）。
> 原型即视觉与交互规格；本提示词定义功能、API 与工程要求。

---

你是一名资深 Go 全栈工程师。请为「拾光集」——一个家庭照片时间轴应用——实现
生产可用的完整系统：Go 后端 + 前台时间轴 + 管理后台。**单机部署、单管理员 +
家人浏览**，照片是不可再生资产，正确性与数据安全优先于性能优化。

请严格按本文档实现，不要自行增删功能范围；有歧义时在代码注释中说明取舍。
两个 HTML 原型是 UI 的唯一基准：**保留其全部视觉与交互（含动效、主题切换、
显影动画），只把模拟逻辑替换为真实 API 调用**。

## 一、交付物清单

1. 完整可编译运行的 Go 项目（模块名 `shiguang`），单二进制；
2. 前端两页经 `go:embed` 打入二进制：`/` 前台时间轴（index.html 改造）、
   `/admin` 管理后台（admin.html 改造）；
3. `Dockerfile`（多阶段，非 root）与 `docker-compose.yml`（含 litestream 备份编排）；
4. goose SQL 迁移文件，启动时自动执行；
5. 测试套件（见「十一、测试与验收」）与 `README.md`（配置表、两种模式部署
   步骤、curl 冒烟脚本、备份恢复手册）。

## 二、技术栈（硬约束，不得替换）

- Go 1.22+；路由 `github.com/go-chi/chi/v5`；日志 `log/slog`（JSON）
- SQLite（`modernc.org/sqlite` 纯 Go 驱动），WAL，写连接池上限 1、读连接 8
- 迁移 `github.com/pressly/goose/v3`（embed 进二进制）
- 图片 `github.com/disintegration/imaging`；EXIF `github.com/rwcarlsen/goexif`；
  BlurHash `github.com/buckket/go-blurhash`
- 对象存储 `github.com/aws/aws-sdk-go-v2`（兼容 MinIO/R2/OSS/COS）
- ID 用 ULID（`github.com/oklog/ulid/v2`）
- 前端保持原型的零构建形态（原生 HTML/CSS/JS，不引入框架与打包器）
- 禁止引入：ORM、Redis、消息队列、gRPC、任何前端构建链

## 三、项目结构

```
shiguang/
├── cmd/server/main.go        # 装配、优雅退出（drain worker → close db）
├── internal/
│   ├── httpapi/              # handler + 中间件(auth/ratelimit/reqid/recover/log)
│   ├── service/              # 业务编排（DB 与 blob 一致性责任集中在此层）
│   ├── store/                # SQL 访问（sqlc 或手写 database/sql）
│   ├── blob/                 # blob.go(接口) local.go s3.go fake.go(测试)
│   ├── imgproc/              # 校验/变体/BlurHash + worker pool + 启动恢复
│   └── jobs/                 # GC(孤儿blob+回收站) · reaper(过期上传会话)
├── migrations/               # goose
└── web/                      # index.html + admin.html（embed）
```

依赖方向单向：`httpapi → service → (store | blob | imgproc)`，禁止反向引用。

## 四、数据模型（迁移文件按此实现）

```sql
CREATE TABLE nodes (
    id TEXT PRIMARY KEY,                    -- ULID
    date TEXT NOT NULL,                     -- 'YYYY-MM-DD'
    title TEXT NOT NULL CHECK(length(title)<=120),
    description TEXT NOT NULL DEFAULT '' CHECK(length(description)<=2000),
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    deleted_at TEXT
);
CREATE INDEX idx_nodes_timeline ON nodes(date DESC, id DESC) WHERE deleted_at IS NULL;

CREATE TABLE photos (
    id TEXT PRIMARY KEY,                    -- ULID
    node_id TEXT NOT NULL REFERENCES nodes(id),
    caption TEXT NOT NULL DEFAULT '' CHECK(length(caption)<=200),
    ord INTEGER NOT NULL,                   -- 留缝排序 100,200,300…
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK(status IN ('pending','processing','ready','failed')),
    fail_reason TEXT,
    sha256 TEXT, ext TEXT,
    width INTEGER, height INTEGER,
    blurhash TEXT, dominant TEXT, size_bytes INTEGER,
    taken_at TEXT,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    deleted_at TEXT
);
CREATE INDEX idx_photos_node ON photos(node_id, ord) WHERE deleted_at IS NULL;
CREATE INDEX idx_photos_status ON photos(status) WHERE status IN ('pending','processing');
CREATE UNIQUE INDEX uq_node_sha ON photos(node_id, sha256)
    WHERE deleted_at IS NULL AND sha256 IS NOT NULL;

CREATE TABLE upload_sessions (
    id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id),
    object_key TEXT NOT NULL, expect_size INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'issued'
        CHECK(state IN ('issued','confirmed','expired','aborted')),
    expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX idx_sessions_reap ON upload_sessions(state, expires_at);
```

Blob key 约定（内容寻址，`ab/cd`=sha256 前 4 位分片，两驱动一致）：

```
orig/ab/cd/<sha256>.<ext>       原图，永久保留
var/ab/cd/<sha256>/thumb.webp   336w   var/.../md.webp 1200w   var/.../lg.webp 2048w
staging/<ulid>.<ext>            s3 暂存，confirm 校验后 Rename 转正
```

## 五、存储抽象（必须按此接口实现 local / s3 / fake 三驱动）

```go
package blob

var ErrNotSupported = errors.New("blob: operation not supported")
var ErrBadKey       = errors.New("blob: invalid key")

type Store interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
    Open(ctx context.Context, key string) (io.ReadCloser, error)
    Stat(ctx context.Context, key string) (size int64, err error)
    Delete(ctx context.Context, key string) error
    Rename(ctx context.Context, from, to string) error
    List(ctx context.Context, prefix string, fn func(key string) error) error
    PublicURL(key string) (url string, ok bool)   // local:("/img/"+key,true)；s3:CDN或桶URL
    PresignPut(ctx context.Context, key, contentType string, size int64,
               ttl time.Duration) (string, error) // local 返回 ErrNotSupported
    PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}
```

local 驱动：key 白名单 `^(orig|var|staging)/[a-z0-9/._-]+$` + Clean 后前缀双校验
防穿越；写入 = 同目录临时文件 → io.Copy → **fsync** → os.Rename 原子可见。
s3 驱动：支持自定义 endpoint 与 path-style（MinIO）；PresignPut 锁定
Content-Length 与 Content-Type。

## 六、API 契约（前缀 `/api/v1`；写接口 `Authorization: Bearer <SG_ADMIN_TOKEN>`）

| 方法/路径 | 说明 |
|-----------|------|
| GET /timeline?cursor&limit(1-50,默认10) | 逆序游标分页；cursor=base64("date\|id")，(date,id) 双键 `<` 定位 |
| GET /nodes/{id} · GET /photos/{id} · GET /stats | 详情 / 状态轮询 / 统计（缓存60s） |
| POST /nodes {date,title,description} · PATCH /nodes/{id} · DELETE /nodes/{id} | DELETE=软删并级联软删照片 |
| POST /nodes/{id}/restore | 从回收站恢复节点（含其照片） |
| POST /nodes/{id}/photos | **local** 模式 multipart(file,caption?)，202 返回 photo |
| POST /uploads/presign {node_id,filename,size,content_type} | **s3** 模式①：建 pending photo+session，返回 {upload_url,photo_id} |
| POST /photos/{id}/confirm | s3 模式③：Stat 比对 → 回读复检魔数 → Rename 转正 → 入队 |
| POST /photos/{id}/reprocess | 对 failed 照片重新入队处理（原图已在则直接重跑管线） |
| PATCH /photos/{id} {caption?,ord?} · DELETE /photos/{id} · POST /photos/{id}/restore | 改注 / 软删 / 恢复 |
| PUT /nodes/{id}/photos/order {photo_ids:[…]} | 整组重排（事务内重赋 ord=100,200,…；设封面=移到首位后调用此接口） |
| GET /trash | 回收站列表：{items:[{type:'node'\|'photo', id, name, deleted_at, purge_at, extra…}]} |
| GET /img/{key...} | 仅 local 模式：`Cache-Control: public,max-age=31536000,immutable` + ETag(sha) + Range；只允许 `var/` 前缀 |
| GET /healthz | DB ping + blob 写探测 |

timeline 响应结构（字段名必须一致）：

```jsonc
{ "items":[{ "id":"01J…","date":"2026-05-21","title":"黄山两日",
  "description":"…","photo_count":5,
  "photos":[{ "id":"01J…","caption":"云海翻涌",
    "status":"ready","fail_reason":null,
    "blurhash":"LEHV6nWB…","dominant":"#8FA6B8","width":4032,"height":3024,
    "variants":{"thumb":"…","md":"…","lg":"…"} }] }],
  "next_cursor":"…或 null" }
```

错误统一 `{"code":"…","message":"…"}`：VALIDATION_FAILED(422) UNAUTHORIZED(401)
NOT_FOUND(404) CONFLICT_DUPLICATE(409，body 附已存在 photo，即"秒传")
PAYLOAD_TOO_LARGE(413) UNSUPPORTED_MEDIA(415) UPLOAD_SESSION_EXPIRED(410)
RATE_LIMITED(429+Retry-After) STORAGE_UNAVAILABLE(503)。

查询规则：timeline 两条 SQL（先取一页节点，再 `node_id IN (...)` 批量取照片），
禁止 N+1；pending/processing/failed 的照片也要返回（前端渲染显影中/失败）。

## 七、上传管线与状态机（核心正确性，逐条实现）

状态机：`pending → processing → ready | failed`（失败自动重试 1 次）。
local 模式跳过 pending 直接 processing；s3 模式 pending 挂 upload_session。

worker：进程内固定池（默认 `runtime.NumCPU()`，`SG_WORKERS` 可覆盖），缓冲队列
512；领取用 DB 抢占更新（影响行数=1 才处理）。**队列满时上传接口仍收下原图
（202），绝不因处理慢拒收资产。**

处理步骤（每步幂等，可重入）：
1. 校验：魔数嗅探仅 jpeg/png/webp；`http.MaxBytesReader` 30MB；
   `image.DecodeConfig` 预读，宽×高 >60MP → failed("pixel bomb")；
2. 规范化：EXIF Orientation(1-8) 矫正；DateTimeOriginal→taken_at(UTC)；
   重编码输出（天然剥离全部 EXIF 含 GPS）；
3. 变体：thumb 336w / md 1200w / lg 2048w，webp q80，Lanczos，小图不放大；
   每个变体写 `…/.tmp-*` 临时 key 再 Rename 覆盖；
4. 元数据：BlurHash 4×3、主色（缩至 1×1 取 #RRGGBB）、宽高、字节数；
   单事务回写 + status=ready。

一致性原则：**blob 先行、DB 断后**。三个后台任务必须实现：
- 启动恢复：processing 超过 10 分钟或遗留 pending(local) 的记录重新入队；
- reaper（每 5 分钟）：过期 session → expired、photo=failed("上传未完成")、删 staging；
- GC（每日）：① 软删超过 `SG_TRASH_TTL_DAYS`(默认7) 的物理清理——删 blob 前
  **必须校验 `SELECT COUNT(*) FROM photos WHERE sha256=? AND deleted_at IS NULL`
  为 0**（同图可被多节点共享）；② List 全量与 DB 对账，删 mtime>48h 的孤儿。

优雅退出：Shutdown(30s) → close(jobs) 等 worker 清空 → db.Close()。

## 八、安全要求（全部必须满足）

1. Bearer token 用 `crypto/subtle.ConstantTimeCompare`；token 读自
   `SG_ADMIN_TOKEN` 或 `SG_ADMIN_TOKEN_FILE`；
2. 限流：全局令牌桶 50 rps；上传端点 10 次/分钟；
3. `SG_PUBLIC_READ=false` 时读 API 也要 token；local 变体 URL 加 HMAC 签名
   `?e=<unix>&s=<hex(hmac_sha256(SG_SIGN_SECRET,key+"|"+e))>`；s3 用 PresignGet(10min)；
4. `/img` key 白名单 + 前缀双校验；原图 `orig/` 永不直出；
5. s3 confirm 后服务端回读复检魔数，不符 → failed + 删对象；
6. 文本字段 API 层长度校验、存原文不存 HTML；
7. http.Server 全套超时（ReadHeader/Read/Write/Idle）。

## 九、前台时间轴对接（web/index.html，保持视觉交互不变）

1. 删除静态 `DATA` 数组 → `GET /api/v1/timeline` 首屏 + 滚动到底用 next_cursor
   取下一页；
2. `variants.thumb` 用于列表、`md` 用于灯箱、双指放大 >2x 切 `lg`；
   显影动画由真实 `Image.onload` 驱动，加载前用 `blurhash` 解码画占位
   （内联 ~1KB 的浏览器端 blurhash 解码函数）；
3. `status=pending/processing` 持续显示未显影相纸并每 3s 轮询
   `GET /photos/{id}`；`failed` 盖"曝光失败"章，title 显示 fail_reason；
4. **安全修复（必做）**：原型用模板字符串 + innerHTML 拼接 caption/title/
   description，存在存储型 XSS——统一改为 createElement+textContent 或经
   HTML 转义函数再插值（管理后台原型已内置 `esc()`，可复用同思路）；
5. stats 区改为 `GET /api/v1/stats`；年份分组仍由前端按 date 前 4 位派生。

## 十、管理后台对接（web/admin.html，以原型为交互基准）

1. **登录**：输入 token 后调用 `GET /api/v1/stats`（带 Bearer）验证——401 则
   复用原型的抖动报错；成功进入工作台，token 存 sessionStorage 并给后续所有
   请求带上；401 响应统一拦截 → 弹回登录页。原型中"任意 ≥8 位可进"的演示逻辑删除。
2. **节点管理**：左栏列表来自 `GET /timeline`（管理端可用大 limit 循环取全量）；
   新建/保存/删除分别对接 POST/PATCH/DELETE `/nodes`；保存成功显示原型的
   "✓ 已保存"，失败 Toast 显示 API 错误 message。
3. **上传（显影盘）**：
   - local 模式：`POST /nodes/{id}/photos` multipart，用 XHR `upload.onprogress`
     驱动原型进度条；
   - s3 模式：presign → 浏览器 PUT（同样有 progress）→ confirm；
   - 后端返回 202 后按原型进入"显影中"状态并轮询 `GET /photos/{id}` 直到
     ready/failed；`failed` 显示"曝光失败"章，重试按钮调用
     `POST /photos/{id}/reprocess`（原图缺失场景则重新走上传）；
   - 409 秒传：Toast 提示"已存在相同照片"，直接展示已存在的 photo；
   - 客户端预检（>30MB、非 jpg/png/webp）沿用原型提示文案。
4. **晾片整理**：拖木夹排序松手后调用 `PUT /nodes/{id}/photos/order`（失败回滚
   本地顺序并 Toast）；✎ 改注 → `PATCH /photos/{id}`；★ 设封面 = 本地移首 +
   调用同一 order 接口；✕ 删除 → 确认弹层 → `DELETE /photos/{id}`。
5. **回收站**：视图数据来自 `GET /trash`，倒计时按 `purge_at` 计算；恢复调用
   `POST /photos/{id}/restore` 或 `POST /nodes/{id}/restore`。
6. **主题切换**：纯前端行为（登录页拉绳 + 工作台顶栏"开灯/关灯"按钮，见原型），
   偏好存 localStorage，两个页面共用同一 key（`sg_theme`），前台时间轴读取同
   一偏好保持一致。
7. 管理后台所有动态文本插入必须走原型已有的 `esc()` 转义或 textContent。

## 十一、测试与验收标准（Definition of Done）

**必须提供的测试：**
- imgproc 单元测试：8 方向 EXIF 样图（可代码生成）、灰度 png、截断文件、
  超 60MP 尺寸头伪造文件 → 断言矫正正确/正确拒绝；
- blob 契约测试：同一套件跑 fake 与 local（Put/Open/Stat/Rename/Delete/List/
  防穿越）；s3 测试在检测到 `SG_TEST_S3_ENDPOINT` 时连 MinIO 执行，否则 skip；
- service 集成测试（fake blob + 临时 SQLite）：上传→ready 全流程、重复上传 409
  秒传、跨节点同图删除不误删共享 blob、reaper 清过期会话、启动恢复重入队、
  游标分页跨页不重不漏、trash 列表与恢复；
- httpapi 测试：鉴权、限流 429、multipart 超限 413、错误码与响应结构。

**人工验收清单（写进 README）：**
1. `docker compose up` 后 `/` 能看到时间轴、`/admin` 能登录；
2. 管理台完整走一遍：错误 token 被拒 → 正确 token 进入 → 建节点 → 拖 3 张
   真实 jpg 进显影盘（有进度条）→ 全部 ready → 改注 → 拖拽排序 → 删 1 张 →
   回收站恢复；刷新前台时间轴，以上变化全部可见且顺序正确；
3. 传 .txt 改名 .jpg → 前后台均出现"曝光失败"，重试（reprocess/重传）路径可用；
4. `SG_BLOB_DRIVER=s3` 指向本地 MinIO 重启，重复第 2 步全部通过；
5. 上传中途 `docker kill` 再启动：无半截数据，卡住任务自动恢复；
6. 变体响应头含 `immutable`；`SG_PUBLIC_READ=false` 时未带签名的变体 URL 403；
   GET /healthz 返回 200。

## 十二、配置（启动即校验，缺必填项 fail-fast 并打印示例）

```
SG_ADDR=:8080
SG_DB_DSN=file:data/shiguang.db?_journal=WAL&_busy_timeout=5000
SG_ADMIN_TOKEN / SG_ADMIN_TOKEN_FILE     （必填其一）
SG_PUBLIC_READ=false
SG_BLOB_DRIVER=local|s3                  （默认 local）
SG_BLOB_LOCAL_ROOT=data/blobs
SG_S3_ENDPOINT / SG_S3_BUCKET / SG_S3_REGION / SG_S3_AK / SG_S3_SK / SG_S3_PATH_STYLE / SG_S3_CDN_BASE
SG_SIGN_SECRET                           （PUBLIC_READ=false 时必填）
SG_LIMIT_UPLOAD_MB=30  SG_LIMIT_PIXELS_MP=60
SG_TRASH_TTL_DAYS=7    SG_WORKERS=0(=NumCPU)
```

## 十三、工作方式

- 按里程碑推进并在每步结束时自测：
  ① schema+store+读 API+local 驱动+embed 两页前端（前台接真数据可浏览）→
  ② 上传管线+worker+变体+BlurHash+管理台上传/整理对接 →
  ③ s3 驱动+presign/confirm → ④ GC/reaper/恢复+trash 接口+安全加固 →
  ⑤ 测试补全+Docker+README；
- 每个里程碑 `go build ./... && go vet ./... && go test ./...` 全绿再继续；
- 改造原型 HTML 时保持其 CSS 变量、类名与动效结构，只替换数据与逻辑层，
  保证视觉零回归；
- 代码风格：标准库优先、错误包装上下文（`fmt.Errorf("…: %w", err)`）、
  公共函数写 doc comment；不生成无关样板；
- 完成后输出：运行说明、两种模式 curl 冒烟脚本、以及实现中所有取舍决定清单。
