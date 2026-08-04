// 拾光集服务入口：装配 config → store → blob → service → worker 池 → jobs → http，
// 优雅退出顺序：http drain → jobs 停止 → worker 清空 → db close。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"shiguang/internal/blob"
	"shiguang/internal/httpapi"
	"shiguang/internal/imgproc"
	"shiguang/internal/jobs"
	"shiguang/internal/service"
	"shiguang/internal/store"
	"shiguang/migrations"
	"shiguang/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := loadConfig()
	if err != nil {
		log.Error("配置校验失败", "err", err)
		os.Exit(1)
	}

	// DB 文件目录不存在时自动创建
	if p, ok := dbFilePath(cfg.DBDSN); ok {
		os.MkdirAll(filepath.Dir(p), 0o750)
	}

	st, err := store.Open(cfg.DBDSN, migrations.FS)
	if err != nil {
		log.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}

	var bl blob.Store
	var localBlob *blob.Local
	if cfg.BlobDriver == "s3" {
		bl, err = blob.NewS3(context.Background(), blob.S3Config{
			Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket, Region: cfg.S3Region,
			AccessKey: cfg.S3AK, SecretKey: cfg.S3SK,
			PathStyle: cfg.S3PathStyle, CDNBase: cfg.S3CDNBase,
		})
	} else {
		localBlob, err = blob.NewLocal(cfg.LocalRoot)
		bl = localBlob
	}
	if err != nil {
		log.Error("初始化 blob 存储失败", "err", err)
		os.Exit(1)
	}

	svc := service.New(service.Config{
		BlobDriver:    cfg.BlobDriver,
		PublicRead:    cfg.PublicRead,
		SignSecret:    cfg.SignSecret,
		UploadLimitMB: cfg.UploadLimitMB,
		PixelLimitMP:  cfg.PixelLimitMP,
		TrashTTLDays:  cfg.TrashTTLDays,
	}, st, bl, log)

	pool := imgproc.NewPool(cfg.Workers, svc.ProcessPhoto, log)
	svc.AttachPool(pool)

	runner := jobs.New(svc, log)
	runner.Start()

	srv := httpapi.New(svc, log, httpapi.Options{
		AdminToken: cfg.AdminToken,
		PublicRead: cfg.PublicRead,
		SignSecret: cfg.SignSecret,
		LocalBlob:  localBlob,
		IndexHTML:  web.IndexHTML,
		AdminHTML:  web.AdminHTML,
		FaviconSVG: web.FaviconSVG,
		GlobalRPS:  float64(cfg.GlobalRPS),
		UploadRPM:  float64(cfg.UploadRPM),
	})

	to := defaultTimeouts()
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: to.ReadHeader,
		ReadTimeout:       to.Read,
		WriteTimeout:      to.Write,
		IdleTimeout:       to.Idle,
	}

	go func() {
		log.Info("拾光集启动", "addr", cfg.Addr, "driver", cfg.BlobDriver,
			"public_read", cfg.PublicRead, "workers", cfg.Workers)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Info("开始优雅退出")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	runner.Stop()
	pool.Close() // 等 worker 清空队列
	if err := st.Close(); err != nil {
		log.Error("db close", "err", err)
	}
	log.Info("已退出")
}

// dbFilePath 从 file: DSN 提取文件路径（用于建目录）；:memory: 等返回 false。
func dbFilePath(dsn string) (string, bool) {
	s := dsn
	if len(s) >= 5 && s[:5] == "file:" {
		s = s[5:]
	}
	if i := indexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == ":memory:" {
		return "", false
	}
	return s, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
