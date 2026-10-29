package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// 集成测试门控：仅当提供 RustFS（或任意 S3 兼容）容器端点与凭据时执行，
// 缺省环境一律 t.Skip，保证 `go test ./...` 离线可跑（AGENTS.md §5.4）。
// 必需：VLB_S3_TEST_ENDPOINT / VLB_S3_TEST_BUCKET / VLB_S3_TEST_ACCESS_KEY / VLB_S3_TEST_SECRET_KEY
// 可选：VLB_S3_TEST_REGION（默认 us-east-1）
func integrationOptions(t *testing.T) (Options, bool) {
	t.Helper()
	endpoint := os.Getenv("VLB_S3_TEST_ENDPOINT")
	bucket := os.Getenv("VLB_S3_TEST_BUCKET")
	accessKey := os.Getenv("VLB_S3_TEST_ACCESS_KEY")
	secretKey := os.Getenv("VLB_S3_TEST_SECRET_KEY")
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return Options{}, false
	}
	return Options{
		Endpoint:  endpoint,
		Region:    os.Getenv("VLB_S3_TEST_REGION"),
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		PathStyle: true,
	}, true
}

// TestIntegrationRoundtrip 验证真实 RustFS 上的 Put→Head→Get→List→Delete 全链路。
func TestIntegrationRoundtrip(t *testing.T) {
	opts, ok := integrationOptions(t)
	if !ok {
		t.Skip("integration s3 env not set: VLB_S3_TEST_* required")
	}
	ctx := context.Background()
	s, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := "vlogs-backup-integration/v1/c/n/20261027/files/a.bin"
	payload := []byte("integration payload against rustfs")
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	if err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, ok, err := s.Head(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Head: ok = %v, err = %v", ok, err)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(payload))
	}
	buf := &sliceBuf{b: make([]byte, len(payload))}
	if err := s.Get(ctx, key, buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(buf.b, payload) {
		t.Fatal("content mismatch")
	}
	metas, err := s.List(ctx, "vlogs-backup-integration/v1/c/n/20261027/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != key {
		t.Fatalf("metas = %+v", metas)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := s.Head(ctx, key); err != nil || ok {
		t.Fatalf("Head after delete: ok = %v, err = %v", ok, err)
	}
}

// TestIntegrationMultipart 验证真实 RustFS 上的分段上传与合并一致性。
// RustFS 分段最小大小约束与 fake 不同，这里用 8MiB 标准分片上传 ~9MiB 数据。
func TestIntegrationMultipart(t *testing.T) {
	opts, ok := integrationOptions(t)
	if !ok {
		t.Skip("integration s3 env not set: VLB_S3_TEST_* required")
	}
	opts.MultipartThreshold = 8 << 20
	opts.MultipartPartSize = 8 << 20
	ctx := context.Background()
	s, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	key := "vlogs-backup-integration/v1/c/n/20261027/files/big.bin"
	const size = 9 << 20
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	if err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, ok, err := s.Head(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Head: ok = %v, err = %v", ok, err)
	}
	if meta.Size != size {
		t.Fatalf("size = %d, want %d", meta.Size, size)
	}
	buf := &sliceBuf{b: make([]byte, size)}
	if err := s.Get(ctx, key, buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(buf.b, payload) {
		t.Fatal("multipart content mismatch against real s3")
	}
}

// TestIntegrationGetNotFound 验证真实 RustFS 的 404 分类（退出码 4）。
func TestIntegrationGetNotFound(t *testing.T) {
	opts, ok := integrationOptions(t)
	if !ok {
		t.Skip("integration s3 env not set: VLB_S3_TEST_* required")
	}
	ctx := context.Background()
	s, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	err = s.Get(ctx, "vlogs-backup-integration/missing-key", &sliceBuf{})
	if err == nil || errs.ExitCode(err) != errs.ExitNotFound {
		t.Fatalf("want exit code %d, got %v", errs.ExitNotFound, err)
	}
}
