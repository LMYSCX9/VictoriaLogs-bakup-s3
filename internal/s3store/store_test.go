package s3store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// sliceBuf 是测试用 io.WriterAt，模拟恢复侧的临时文件写入。
type sliceBuf struct{ b []byte }

func (s *sliceBuf) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(s.b)) {
		return 0, errors.New("write out of range")
	}
	copy(s.b[off:], p)
	return len(p), nil
}

func TestNewKeyBuilder(t *testing.T) {
	t.Parallel()
	if _, err := NewKeyBuilder("logs", "", "node0"); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("empty cluster: want exit %d, got %v", errs.ExitUsage, err)
	}
	if _, err := NewKeyBuilder("logs", "prod", ""); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("empty node: want exit %d, got %v", errs.ExitUsage, err)
	}
	b, err := NewKeyBuilder("/logs/", "prod", "node0")
	if err != nil {
		t.Fatalf("NewKeyBuilder: %v", err)
	}
	if b.prefix != "logs" {
		t.Fatalf("prefix = %q, want %q", b.prefix, "logs")
	}
}

func TestKeyBuilderKeys(t *testing.T) {
	t.Parallel()
	b, err := NewKeyBuilder("logs", "prod-logs", "vlstorage-0")
	if err != nil {
		t.Fatalf("NewKeyBuilder: %v", err)
	}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "manifest_key",
			got: func() string {
				k, err := b.ManifestKey("20261027")
				if err != nil {
					t.Fatalf("ManifestKey: %v", err)
				}
				return k
			}(),
			want: "logs/v1/prod-logs/vlstorage-0/20261027/manifest.json",
		},
		{
			name: "file_key",
			got: func() string {
				k, err := b.FileKey("20261027", "indexdb/a.bin")
				if err != nil {
					t.Fatalf("FileKey: %v", err)
				}
				return k
			}(),
			want: "logs/v1/prod-logs/vlstorage-0/20261027/files/indexdb/a.bin",
		},
		{
			name: "file_key_backslash_normalized",
			got: func() string {
				k, err := b.FileKey("20261027", `parts\b\c.bin`)
				if err != nil {
					t.Fatalf("FileKey: %v", err)
				}
				return k
			}(),
			want: "logs/v1/prod-logs/vlstorage-0/20261027/files/parts/b/c.bin",
		},
		{
			name: "unit_prefix",
			got: func() string {
				k, err := b.UnitPrefix("20261027")
				if err != nil {
					t.Fatalf("UnitPrefix: %v", err)
				}
				return k
			}(),
			want: "logs/v1/prod-logs/vlstorage-0/20261027/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}

	// 无前缀时以 v1 开头。
	b0, err := NewKeyBuilder("", "c", "n")
	if err != nil {
		t.Fatalf("NewKeyBuilder: %v", err)
	}
	k, err := b0.ManifestKey("20261027")
	if err != nil {
		t.Fatalf("ManifestKey: %v", err)
	}
	if k != "v1/c/n/20261027/manifest.json" {
		t.Fatalf("key = %q", k)
	}

	// 非法输入必须被拒绝（C7/C8 防御）。
	bad := []struct {
		name     string
		partFn   func() (string, error)
		wantCode int
	}{
		{name: "partition_not_date", partFn: func() (string, error) { return b.FileKey("2026102", "a") }, wantCode: errs.ExitUsage},
		{name: "partition_invalid_day", partFn: func() (string, error) { return b.FileKey("20261301", "a") }, wantCode: errs.ExitUsage},
		{name: "rel_empty", partFn: func() (string, error) { return b.FileKey("20261027", "") }, wantCode: errs.ExitUsage},
		{name: "rel_absolute", partFn: func() (string, error) { return b.FileKey("20261027", "/etc/passwd") }, wantCode: errs.ExitUsage},
		{name: "rel_dotdot", partFn: func() (string, error) { return b.FileKey("20261027", "../x") }, wantCode: errs.ExitUsage},
		{name: "rel_drive", partFn: func() (string, error) { return b.FileKey("20261027", "C:/x") }, wantCode: errs.ExitUsage},
		{name: "rel_empty_segment", partFn: func() (string, error) { return b.FileKey("20261027", "a//b") }, wantCode: errs.ExitUsage},
		{name: "rel_dot_segment", partFn: func() (string, error) { return b.FileKey("20261027", "a/./b") }, wantCode: errs.ExitUsage},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.partFn()
			if err == nil || errs.ExitCode(err) != tc.wantCode {
				t.Fatalf("want exit code %d, got %v", tc.wantCode, err)
			}
		})
	}
}

func TestFileKeyToRel(t *testing.T) {
	t.Parallel()
	b, err := NewKeyBuilder("", "c", "n")
	if err != nil {
		t.Fatalf("NewKeyBuilder: %v", err)
	}
	key, err := b.FileKey("20261027", "parts/a.bin")
	if err != nil {
		t.Fatalf("FileKey: %v", err)
	}
	rel, ok := b.FileKeyToRel(key, "20261027")
	if !ok || rel != "parts/a.bin" {
		t.Fatalf("rel = %q, ok = %v", rel, ok)
	}
	if _, ok := b.FileKeyToRel("v1/c/n/20261027/manifest.json", "20261027"); ok {
		t.Fatal("manifest key must not be a file key")
	}
	if _, ok := b.FileKeyToRel("v1/c/n/20261028/files/a", "20261027"); ok {
		t.Fatal("other partition key must not match")
	}
}

// newFakeStore 构造 Store + fakeS3；默认单次尝试（SDK 不重试），故障测试即时生效。
func newFakeStore(t *testing.T, mutate func(*fakeS3), optFns ...func(*Options)) (*Store, *fakeS3) {
	t.Helper()
	f := newFakeS3()
	if mutate != nil {
		mutate(f)
	}
	srv := newServer(t, f)
	opts := Options{
		Endpoint:  srv.URL,
		Bucket:    "test-bucket",
		AccessKey: "ak",
		SecretKey: "sk",
		PathStyle: true,
	}
	for _, fn := range optFns {
		fn(&opts)
	}
	s, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, f
}

func TestNewStoreValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts Options
	}{
		{name: "no_endpoint", opts: Options{Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		{name: "no_bucket", opts: Options{Endpoint: "http://e", AccessKey: "a", SecretKey: "s"}},
		{name: "no_credentials", opts: Options{Endpoint: "http://e", Bucket: "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStore(tc.opts)
			if err == nil || errs.ExitCode(err) != errs.ExitUsage {
				t.Fatalf("want exit code %d, got %v", errs.ExitUsage, err)
			}
		})
	}
}

func TestStoreRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newFakeStore(t, nil)
	const key = "v1/c/n/20261027/files/a.bin"
	payload := []byte("hello rustfs")

	if err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, ok, err := s.Head(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Head: ok = %v, err = %v", ok, err)
	}
	if meta.Size != int64(len(payload)) || meta.Key != key {
		t.Fatalf("meta = %+v", meta)
	}

	buf := &sliceBuf{b: make([]byte, len(payload))}
	if err := s.Get(ctx, key, buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(buf.b, payload) {
		t.Fatalf("content = %q, want %q", buf.b, payload)
	}

	metas, err := s.List(ctx, "v1/c/n/20261027/")
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

func TestStoreEmptyObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newFakeStore(t, nil)
	const key = "v1/c/n/20261027/files/empty"
	if err := s.Put(ctx, key, bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	meta, ok, err := s.Head(ctx, key)
	if err != nil || !ok || meta.Size != 0 {
		t.Fatalf("Head: ok = %v, size = %d, err = %v", ok, meta.Size, err)
	}
	buf := &sliceBuf{b: make([]byte, 1)}
	if err := s.Get(ctx, key, buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.b[0] != 0 {
		t.Fatalf("content = %v, want zero", buf.b)
	}
}

func TestStoreMultipart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newFakeStore(t, nil, func(o *Options) {
		o.MultipartThreshold = 1024
		o.MultipartPartSize = 512
	})
	const key = "v1/c/n/20261027/files/big.bin"
	payload := make([]byte, 2500)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

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
		t.Fatal("multipart content mismatch")
	}
}

func TestStoreMultipartAbortOnPartFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := newFakeStore(t, func(f *fakeS3) { f.failPartN = 2 },
		func(o *Options) {
			o.MultipartThreshold = 256
			o.MultipartPartSize = 128
			o.MaxAttempts = 1
		})
	const key = "v1/c/n/20261027/files/fail.bin"
	payload := make([]byte, 300)

	err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)))
	if err == nil || errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, err)
	}
	if len(f.aborts) != 1 {
		t.Fatalf("aborts = %v, want exactly one", f.aborts)
	}
	if _, ok := f.objects[key]; ok {
		t.Fatal("object must not exist after abort")
	}
}

func TestStoreMultipartCompleteFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := newFakeStore(t, func(f *fakeS3) { f.failComplete = 5 },
		func(o *Options) {
			o.MultipartThreshold = 256
			o.MultipartPartSize = 128
			o.MaxAttempts = 1
		})
	const key = "v1/c/n/20261027/files/fail2.bin"
	payload := make([]byte, 300)

	err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)))
	if err == nil || errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, err)
	}
	if len(f.aborts) != 1 {
		t.Fatalf("aborts = %v, want exactly one", f.aborts)
	}
}

func TestStoreMultipartShortRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := newFakeStore(t, nil, func(o *Options) {
		o.MultipartThreshold = 256
		o.MultipartPartSize = 128
	})
	const key = "v1/c/n/20261027/files/short.bin"
	// 声明 300 字节，实际只有 5 字节：必须拒绝，不得提交残缺对象。
	err := s.Put(ctx, key, strings.NewReader("short"), 300)
	if err == nil || errs.ExitCode(err) != errs.ExitGeneral {
		t.Fatalf("want exit code %d, got %v", errs.ExitGeneral, err)
	}
	if _, ok := f.objects[key]; ok {
		t.Fatal("object must not exist after short read")
	}
}

func TestStoreGetNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newFakeStore(t, nil)
	err := s.Get(ctx, "v1/c/n/20261027/files/missing", &sliceBuf{})
	if err == nil || errs.ExitCode(err) != errs.ExitNotFound {
		t.Fatalf("want exit code %d, got %v", errs.ExitNotFound, err)
	}
}

func TestStoreListPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := newFakeStore(t, nil, func(o *Options) { o.ListPageSize = 2 })
	const unit = "v1/c/n/20261027/files/"
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		f.objects[unit+k] = []byte(k)
	}
	metas, err := s.List(ctx, unit)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 5 {
		t.Fatalf("got %d metas, want 5", len(metas))
	}
	for i, want := range []string{"a", "b", "c", "d", "e"} {
		if metas[i].Key != unit+want {
			t.Fatalf("metas[%d] = %q, want %q", i, metas[i].Key, unit+want)
		}
	}
}

func TestStoreListPrefixIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := newFakeStore(t, nil)
	f.objects["v1/c/n/20261027/files/a"] = []byte("a")
	f.objects["v1/c/n/20261028/files/a"] = []byte("a")
	f.objects["v1/c/n/20261027/manifest.json"] = []byte("{}")
	metas, err := s.List(ctx, "v1/c/n/20261027/files/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != "v1/c/n/20261027/files/a" {
		t.Fatalf("metas = %+v", metas)
	}
}
