package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// config 汇总全部环境变量；启动即校验，缺必填项 fail-fast。
type config struct {
	Addr       string
	DBDSN      string
	AdminToken string
	PublicRead bool
	BlobDriver string // local | s3
	LocalRoot  string

	S3Endpoint  string
	S3Bucket    string
	S3Region    string
	S3AK        string
	S3SK        string
	S3PathStyle bool
	S3CDNBase   string

	SignSecret    string
	UploadLimitMB int64
	PixelLimitMP  int64
	TrashTTLDays  int
	Workers       int
	GlobalRPS     int64
	UploadRPM     int64
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func envInt(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，得到 %q", key, v)
	}
	return n, nil
}

const configExample = `配置示例：
  SG_ADDR=:8080
  SG_DB_DSN=file:data/shiguang.db
  SG_ADMIN_TOKEN=change-me-to-a-long-random-token   （或 SG_ADMIN_TOKEN_FILE=/run/secrets/token）
  SG_PUBLIC_READ=true
  SG_BLOB_DRIVER=local
  SG_BLOB_LOCAL_ROOT=data/blobs
  SG_SIGN_SECRET=another-random-secret               （SG_PUBLIC_READ=false 时必填）
  SG_LIMIT_UPLOAD_RPM=120                            （批量导入可调高，见 README）
  # s3 模式追加：
  SG_S3_ENDPOINT=http://minio:9000  SG_S3_BUCKET=shiguang  SG_S3_REGION=us-east-1
  SG_S3_AK=...  SG_S3_SK=...  SG_S3_PATH_STYLE=true  SG_S3_CDN_BASE=`

// loadConfig 读取并校验配置；出错时返回带示例的错误信息。
func loadConfig() (*config, error) {
	cfg := &config{
		Addr:       envOr("SG_ADDR", ":8080"),
		DBDSN:      envOr("SG_DB_DSN", "file:data/shiguang.db"),
		PublicRead: envBool("SG_PUBLIC_READ", false),
		BlobDriver: envOr("SG_BLOB_DRIVER", "local"),
		LocalRoot:  envOr("SG_BLOB_LOCAL_ROOT", "data/blobs"),

		S3Endpoint:  os.Getenv("SG_S3_ENDPOINT"),
		S3Bucket:    os.Getenv("SG_S3_BUCKET"),
		S3Region:    os.Getenv("SG_S3_REGION"),
		S3AK:        os.Getenv("SG_S3_AK"),
		S3SK:        os.Getenv("SG_S3_SK"),
		S3PathStyle: envBool("SG_S3_PATH_STYLE", false),
		S3CDNBase:   os.Getenv("SG_S3_CDN_BASE"),

		SignSecret: os.Getenv("SG_SIGN_SECRET"),
	}

	// token：SG_ADMIN_TOKEN 与 SG_ADMIN_TOKEN_FILE 必填其一
	cfg.AdminToken = os.Getenv("SG_ADMIN_TOKEN")
	if f := os.Getenv("SG_ADMIN_TOKEN_FILE"); cfg.AdminToken == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("读取 SG_ADMIN_TOKEN_FILE 失败: %w\n%s", err, configExample)
		}
		cfg.AdminToken = strings.TrimSpace(string(b))
	}
	if cfg.AdminToken == "" {
		return nil, fmt.Errorf("必须设置 SG_ADMIN_TOKEN 或 SG_ADMIN_TOKEN_FILE\n%s", configExample)
	}

	if cfg.BlobDriver != "local" && cfg.BlobDriver != "s3" {
		return nil, fmt.Errorf("SG_BLOB_DRIVER 只能是 local 或 s3，得到 %q\n%s", cfg.BlobDriver, configExample)
	}
	if cfg.BlobDriver == "s3" {
		var missing []string
		for k, v := range map[string]string{
			"SG_S3_BUCKET": cfg.S3Bucket, "SG_S3_AK": cfg.S3AK, "SG_S3_SK": cfg.S3SK,
		} {
			if v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("s3 模式缺少必填项: %s\n%s", strings.Join(missing, ", "), configExample)
		}
	}
	if !cfg.PublicRead && cfg.SignSecret == "" {
		return nil, fmt.Errorf("SG_PUBLIC_READ=false 时必须设置 SG_SIGN_SECRET\n%s", configExample)
	}

	var err error
	if cfg.UploadLimitMB, err = envInt("SG_LIMIT_UPLOAD_MB", 30); err != nil {
		return nil, err
	}
	if cfg.PixelLimitMP, err = envInt("SG_LIMIT_PIXELS_MP", 60); err != nil {
		return nil, err
	}
	ttl, err := envInt("SG_TRASH_TTL_DAYS", 7)
	if err != nil {
		return nil, err
	}
	cfg.TrashTTLDays = int(ttl)
	workers, err := envInt("SG_WORKERS", 0)
	if err != nil {
		return nil, err
	}
	cfg.Workers = int(workers)
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.GlobalRPS, err = envInt("SG_LIMIT_GLOBAL_RPS", 50); err != nil {
		return nil, err
	}
	if cfg.UploadRPM, err = envInt("SG_LIMIT_UPLOAD_RPM", 120); err != nil {
		return nil, err
	}
	if cfg.GlobalRPS <= 0 || cfg.UploadRPM <= 0 {
		return nil, fmt.Errorf("SG_LIMIT_GLOBAL_RPS 与 SG_LIMIT_UPLOAD_RPM 必须为正整数\n%s", configExample)
	}
	return cfg, nil
}

// serverTimeouts 是 http.Server 全套超时。
type serverTimeouts struct {
	ReadHeader, Read, Write, Idle time.Duration
}

func defaultTimeouts() serverTimeouts {
	return serverTimeouts{
		ReadHeader: 10 * time.Second,
		Read:       5 * time.Minute, // 30MB 上传给足余量
		Write:      5 * time.Minute,
		Idle:       2 * time.Minute,
	}
}
