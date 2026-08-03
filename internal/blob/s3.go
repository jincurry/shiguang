package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config 是 s3 驱动配置，兼容 MinIO / R2 / OSS / COS 等 S3 协议实现。
type S3Config struct {
	Endpoint  string // 留空用 AWS 官方
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool   // MinIO 需要 true
	CDNBase   string // 非空时 PublicURL 返回 CDNBase+"/"+key
}

// S3 是 aws-sdk-go-v2 实现的对象存储驱动。
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	cfg     S3Config
}

// NewS3 创建 s3 驱动并校验必填配置。
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob s3: bucket required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // MinIO 等自建服务对 region 不敏感
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("blob s3: load config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})
	return &S3{
		client:  client,
		presign: s3.NewPresignClient(client),
		cfg:     cfg,
	}, nil
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound")
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if !ValidKey(key) {
		return ErrBadKey
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("blob s3 put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, ErrBadKey
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("blob s3 open %s: %w", key, err)
	}
	return out.Body, nil
}

func (s *S3) Stat(ctx context.Context, key string) (int64, error) {
	if !ValidKey(key) {
		return 0, ErrBadKey
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return 0, fmt.Errorf("blob s3 stat %s: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrBadKey
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blob s3 delete %s: %w", key, err)
	}
	return nil
}

// Rename 通过服务端 CopyObject + DeleteObject 实现（S3 无原生 rename）。
func (s *S3) Rename(ctx context.Context, from, to string) error {
	if !ValidKey(from) || !ValidKey(to) {
		return ErrBadKey
	}
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.cfg.Bucket),
		Key:        aws.String(to),
		CopySource: aws.String(s.cfg.Bucket + "/" + from),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, from)
		}
		return fmt.Errorf("blob s3 rename %s -> %s: copy: %w", from, to, err)
	}
	if err := s.Delete(ctx, from); err != nil {
		return fmt.Errorf("blob s3 rename %s -> %s: delete src: %w", from, to, err)
	}
	return nil
}

func (s *S3) List(ctx context.Context, prefix string, fn func(key string) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("blob s3 list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if err := fn(aws.ToString(obj.Key)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) PublicURL(key string) (string, bool) {
	if s.cfg.CDNBase != "" {
		return strings.TrimSuffix(s.cfg.CDNBase, "/") + "/" + key, true
	}
	return "", false
}

func (s *S3) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrBadKey
	}
	// 锁定 Content-Length 与 Content-Type：二者进入签名，客户端改动即 403。
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob s3 presign put %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrBadKey
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob s3 presign get %s: %w", key, err)
	}
	return req.URL, nil
}
