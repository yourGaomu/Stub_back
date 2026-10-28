package objectstore

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/yourGaomu/short_links/back/internal/config"
)

type Store struct {
	internal *minio.Client
	public   *minio.Client
	core     *minio.Core
	cancel   context.CancelFunc
}

func (s *Store) Ping(ctx context.Context) error {
	// 1. MinIO 连通性只检查内网服务端点，不触碰公网签名地址。
	if s == nil {
		return fmt.Errorf("MinIO 未配置")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s.internal.IsOffline() {
		return fmt.Errorf("MinIO 内网服务不可达")
	}
	return nil
}

func (s *Store) Close() {
	// 1. 停止后台健康探针，避免 App 关闭后遗留 goroutine。
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

func New(cfg config.Config) (*Store, error) {
	// 1. MinIO 配置可整体缺省用于纯 URL 部署，但不接受半套凭据。
	configured := cfg.MinIOInternalEndpoint != "" || cfg.MinIOPublicEndpoint != "" || cfg.MinIOAccessKey != "" || cfg.MinIOSecretKey != ""
	if !configured {
		return nil, nil
	}
	if cfg.MinIOInternalEndpoint == "" || cfg.MinIOPublicEndpoint == "" || cfg.MinIOAccessKey == "" || cfg.MinIOSecretKey == "" {
		return nil, fmt.Errorf("MinIO 内网地址、公网签名地址及访问凭据必须成套配置")
	}

	// 2. 分离内网对象操作地址与公网预签名地址，避免签名后改写 Host。
	internalHost, internalSecure, err := endpoint(cfg.MinIOInternalEndpoint, cfg.MinIOUseSSL)
	if err != nil {
		return nil, fmt.Errorf("MINIO_INTERNAL_ENDPOINT 无效: %w", err)
	}
	publicHost, publicSecure, err := endpoint(cfg.MinIOPublicEndpoint, true)
	if err != nil {
		return nil, fmt.Errorf("MINIO_PUBLIC_ENDPOINT 无效: %w", err)
	}
	credentials := credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, "")
	internalClient, err := minio.New(internalHost, &minio.Options{Creds: credentials, Secure: internalSecure})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO 内网客户端失败: %w", err)
	}
	publicClient, err := minio.New(publicHost, &minio.Options{Creds: credentials, Secure: publicSecure})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO 公网签名客户端失败: %w", err)
	}
	// 3. 使用 SDK 健康探针检测内网 endpoint；该探针不需要列举桶的权限。
	cancel, err := internalClient.HealthCheck(10 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("启动 MinIO 健康检查失败: %w", err)
	}
	return &Store{internal: internalClient, public: publicClient, core: &minio.Core{Client: internalClient}, cancel: cancel}, nil
}

// 1. Multipart 上传只通过内网 MinIO 客户端执行，调用方始终只连接 Go API。
func (s *Store) BeginMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	return s.core.NewMultipartUpload(ctx, bucket, key, minio.PutObjectOptions{ContentType: contentType})
}

func (s *Store) PutMultipartPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	part, err := s.core.PutObjectPart(ctx, bucket, key, uploadID, partNumber, reader, size, minio.PutObjectPartOptions{})
	return part.ETag, err
}

func (s *Store) ListMultipartParts(ctx context.Context, bucket, key, uploadID string) ([]minio.ObjectPart, error) {
	result, err := s.core.ListObjectParts(ctx, bucket, key, uploadID, 0, 10000)
	return result.ObjectParts, err
}

func (s *Store) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []minio.CompletePart, contentType string) error {
	_, err := s.core.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *Store) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	err := s.core.AbortMultipartUpload(ctx, bucket, key, uploadID)
	if err == nil {
		return nil
	}
	if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
		return nil
	}
	return err
}

func (s *Store) AbortMultipartForObject(ctx context.Context, bucket, key string) error {
	// 1. 回收 initializing 会话时，处理 MinIO 已创建但 upload ID 尚未写回数据库的崩溃窗口。
	result, err := s.core.ListMultipartUploads(ctx, bucket, key, "", "", "", 1000)
	if err != nil {
		return err
	}
	for _, upload := range result.Uploads {
		if upload.Key == key {
			if err := s.core.AbortMultipartUpload(ctx, bucket, key, upload.UploadID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ObjectSize(ctx context.Context, bucket, key string) (int64, error) {
	info, err := s.internal.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	return info.Size, err
}

func (s *Store) Put(ctx context.Context, bucket, key, contentType string, reader io.Reader, size int64) error {
	// 1. 只通过内网客户端上传对象；桶必须由管理员预先创建。
	_, err := s.internal.PutObject(ctx, bucket, key, reader, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *Store) BucketExists(ctx context.Context, bucket string) (bool, error) {
	// 1. 业务登记只检查指定桶，不授予后端列举全部桶的权限。
	if s == nil {
		return false, fmt.Errorf("MinIO 未配置")
	}
	return s.internal.BucketExists(ctx, bucket)
}

func (s *Store) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	// 1. 回收站恢复前确认对象仍存在，避免只恢复数据库引用。
	if s == nil {
		return false, fmt.Errorf("MinIO 未配置")
	}
	_, err := s.internal.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	response := minio.ToErrorResponse(err)
	if response.Code == "NoSuchKey" || response.Code == "NoSuchObject" || response.Code == "NotFound" {
		return false, nil
	}
	return false, err
}

func (s *Store) Remove(ctx context.Context, bucket, key string) error {
	// 1. 物理删除同样走内网，公网对象入口不承担管理权限。
	return s.internal.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

func (s *Store) PresignedGet(ctx context.Context, bucket, key string, expires time.Duration, contentType, fileName string) (string, error) {
	// 1. 仅生成公网 Host 上的短期签名 URL，不发出 MinIO 网络请求。
	// 2. 保留浏览器对图片、视频等素材的内联预览，并让 APK 按安装包下载。
	disposition := "inline"
	if strings.EqualFold(filepath.Ext(fileName), ".apk") {
		contentType = "application/vnd.android.package-archive"
		disposition = "attachment"
	}
	responseHeaders := url.Values{}
	if contentType != "" {
		responseHeaders.Set("response-content-type", contentType)
	}
	if fileName != "" {
		responseHeaders.Set("response-content-disposition", mime.FormatMediaType(disposition, map[string]string{"filename": fileName}))
	}
	value, err := s.public.PresignedGetObject(ctx, bucket, key, expires, responseHeaders)
	if err != nil {
		return "", err
	}
	return value.String(), nil
}

func endpoint(raw string, defaultSecure bool) (string, bool, error) {
	// 1. 同时接受带 scheme 的公网 URL 和仅含 host:port 的容器内网地址。
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		return raw, defaultSecure, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path != "" {
		return "", false, fmt.Errorf("地址必须是 host:port 或无路径 URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false, fmt.Errorf("地址 scheme 只能是 http 或 https")
	}
	return parsed.Host, parsed.Scheme == "https", nil
}
