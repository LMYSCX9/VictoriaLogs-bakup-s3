package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// 包内兜底默认值：编排层/配置层应显式传参覆盖（Q2：阈值不写死为业务契约，
// 这里仅防止 Options 零值导致除零或全量走 multipart）。
const (
	defaultMultipartThreshold = int64(64) << 20 // 64MiB：超过即分段上传
	defaultMultipartPartSize  = int64(8) << 20  // 8MiB：单分片大小
	defaultMaxAttempts        = 8               // §12：S3 5xx/SlowDown 上限 8 次
	defaultListPageSize       = int32(1000)
)

// ObjectStore 是编排层依赖的对象存储接口（设计文档 §8.1），
// 所有实现必须自带重试，编排层只感知最终分类错误（C12）。
type ObjectStore interface {
	Head(ctx context.Context, key string) (ObjectMeta, bool, error)
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string, w io.WriterAt) error
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	Delete(ctx context.Context, key string) error
}

// ObjectMeta 是对象的元数据视图。
type ObjectMeta struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Options 是 Store 的构造参数；字段与配置层一一对应，凭据不落日志（C9）。
type Options struct {
	Endpoint           string        // S3 兼容端点（RustFS），必填
	Region             string        // 允许空，空则占位 us-east-1（§8.4）
	Bucket             string        // 必填
	AccessKey          string        // 必填
	SecretKey          string        // 必填
	PathStyle          bool          // RustFS 默认 path-style
	MultipartThreshold int64         // ≤0 时用包内兜底默认
	MultipartPartSize  int64         // ≤0 时用包内兜底默认
	MaxAttempts        int           // SDK 重试上限，≤0 时为 8
	ListPageSize       int32         // List 分页大小，≤0 时为 1000
	Timeout            time.Duration // 单请求超时；0 表示不限（整体由作业超时约束）
	Logger             *slog.Logger
}

// Store 是 ObjectStore 的 aws-sdk-go-v2 实现。
type Store struct {
	client             *s3.Client
	bucket             string
	multipartThreshold int64
	multipartPartSize  int64
	listPageSize       int32
	logger             *slog.Logger
}

// 编译期断言：Store 满足 ObjectStore 契约。
var _ ObjectStore = (*Store)(nil)

// NewStore 按 Options 构造 S3 客户端：静态凭据 + 固定端点 + path-style（RustFS 同款），
// 不使用 aws-sdk-go-v2/config 的环境配置加载器（凭据显式注入，C9）。
func NewStore(opts Options) (*Store, error) {
	if opts.Endpoint == "" {
		return nil, errs.Usage("s3 endpoint must not be empty")
	}
	if opts.Bucket == "" {
		return nil, errs.Usage("s3 bucket must not be empty")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, errs.Usage("s3 access key and secret key must not be empty")
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	threshold := opts.MultipartThreshold
	if threshold <= 0 {
		threshold = defaultMultipartThreshold
	}
	partSize := opts.MultipartPartSize
	if partSize <= 0 {
		partSize = defaultMultipartPartSize
	}
	pageSize := opts.ListPageSize
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}

	provider := credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, "")
	client := s3.New(s3.Options{
		BaseEndpoint:     aws.String(opts.Endpoint),
		Region:           region,
		Credentials:      aws.NewCredentialsCache(provider),
		UsePathStyle:     opts.PathStyle,
		RetryMaxAttempts: maxAttempts,
		HTTPClient:       &http.Client{Timeout: opts.Timeout},
	})
	return &Store{
		client:             client,
		bucket:             opts.Bucket,
		multipartThreshold: threshold,
		multipartPartSize:  partSize,
		listPageSize:       pageSize,
		logger:             opts.Logger,
	}, nil
}

// Head 返回对象元数据；对象不存在返回 (zero, false, nil)。
func (s *Store) Head(ctx context.Context, key string) (ObjectMeta, bool, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectMeta{}, false, nil
		}
		return ObjectMeta{}, false, classify("head "+key, err)
	}
	meta := ObjectMeta{Key: key}
	if out.ContentLength != nil {
		meta.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		meta.LastModified = *out.LastModified
	}
	return meta, true, nil
}

// HeadBucket 校验桶存在且凭据可访问（setup 连通性验证用）。
func (s *Store) HeadBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err == nil {
		return nil
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusNotFound:
			return errs.NotFound(err, "head bucket %s: bucket not found", s.bucket)
		case http.StatusUnauthorized, http.StatusForbidden:
			return errs.Precondition(err, "head bucket %s: access denied", s.bucket)
		}
	}
	return classify("head bucket "+s.bucket, err)
}

// Put 上传对象：size 未超分段阈值走 PutObject，超过则手写 multipart
// （分片大小可配，Q2）；任何失败中止上传并清理已上传分片。
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size <= s.multipartThreshold {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          r,
			ContentLength: aws.Int64(size),
		})
		if err != nil {
			return classify("put "+key, err)
		}
		return nil
	}
	if err := s.putMultipart(ctx, key, r, size); err != nil {
		return err
	}
	return nil
}

// putMultipart 手写分段上传：CreateMultipartUpload → UploadPart × N → Complete；
// 实际读取字节必须与声明 size 一致，不一致视为源数据异常，中止并清理；
// 失败时 Abort（本身可重入，中止失败仅记日志，不影响主错误）。
func (s *Store) putMultipart(ctx context.Context, key string, r io.Reader, size int64) error {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return classify("create multipart "+key, err)
	}
	abort := func() {
		_, aerr := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			UploadId: created.UploadId,
		})
		if aerr != nil {
			s.log().WarnContext(ctx, "abort multipart upload failed",
				"key", key, "err", aerr)
		}
	}

	buf := make([]byte, s.multipartPartSize)
	var parts []s3types.CompletedPart
	var total int64
	for partNumber := int32(1); ; partNumber++ {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			total += int64(n)
			out, uerr := s.client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        aws.String(s.bucket),
				Key:           aws.String(key),
				UploadId:      created.UploadId,
				PartNumber:    aws.Int32(partNumber),
				Body:          bytes.NewReader(buf[:n]),
				ContentLength: aws.Int64(int64(n)),
			})
			if uerr != nil {
				abort()
				return classify(fmt.Sprintf("upload part %d %s", partNumber, key), uerr)
			}
			parts = append(parts, s3types.CompletedPart{
				PartNumber: aws.Int32(partNumber),
				ETag:       out.ETag,
			})
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			abort()
			return errs.Untyped(rerr, "read source for multipart %s", key)
		}
	}
	if len(parts) == 0 {
		abort()
		return errs.Untyped(io.ErrUnexpectedEOF, "source is shorter than declared for multipart %s", key)
	}
	if total != size {
		abort()
		return errs.Untyped(io.ErrUnexpectedEOF, "source size %d mismatches declared %d for multipart %s", total, size, key)
	}

	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()
		return classify("complete multipart "+key, err)
	}
	return nil
}

// Get 下载对象并顺序写入 WriterAt（接口契约见 §8.1；
// 恢复侧可基于 WriterAt 自行并发分段，本层保持顺序写）。
func (s *Store) Get(ctx context.Context, key string, w io.WriterAt) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return classify("get "+key, err)
	}
	defer out.Body.Close()
	if _, err := io.Copy(&offsetWriter{w: w}, out.Body); err != nil {
		return classify("read "+key, err)
	}
	return nil
}

// List 按前缀分页列举对象元数据（ListObjectsV2）。
func (s *Store) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	var metas []ObjectMeta
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
			MaxKeys:           aws.Int32(s.listPageSize),
		})
		if err != nil {
			return nil, classify("list "+prefix, err)
		}
		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			meta := ObjectMeta{Key: *obj.Key}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.LastModified = *obj.LastModified
			}
			metas = append(metas, meta)
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return metas, nil
		}
		token = out.NextContinuationToken
	}
}

// Delete 删除单个对象。
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return classify("delete "+key, err)
	}
	return nil
}

// offsetWriter 把顺序 Write 平移到 WriterAt 的连续偏移上。
type offsetWriter struct {
	w   io.WriterAt
	off int64
}

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.w.WriteAt(p, o.off)
	o.off += int64(n)
	return n, err
}

// isNotFound 判断 SDK 错误是否为对象/前缀不存在。
func isNotFound(err error) bool {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

// classify 把 SDK 错误映射为对外分类错误（§11.2）：
// 不存在 → 退出码 4；5xx 与传输层错误 → 退出码 3（SDK 已重试耗尽）；
// 其余 4xx → 退出码 1。
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return errs.NotFound(err, "%s: object not found", op)
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		if code := re.HTTPStatusCode(); code >= 500 {
			return errs.Network(err, "%s: s3 server returned %d", op, code)
		}
		return errs.Untyped(err, "%s: unexpected status %d", op, re.HTTPStatusCode())
	}
	return errs.Network(err, "%s: request failed", op)
}

func (s *Store) log() *slog.Logger {
	if s.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.logger
}
