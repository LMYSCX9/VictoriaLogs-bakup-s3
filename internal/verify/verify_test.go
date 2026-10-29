package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/backupmeta"
	"github.com/vlogs-backup/vlogs-backup/internal/errs"
	"github.com/vlogs-backup/vlogs-backup/internal/s3store"
)

// fakeStore 是内存对象存储（§5.4：禁止真实访问外网），Get 顺序写入。
type fakeStore struct {
	objs   map[string][]byte
	headEr map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}, headEr: map[string]error{}}
}

func (f *fakeStore) Head(_ context.Context, key string) (s3store.ObjectMeta, bool, error) {
	if err := f.headEr[key]; err != nil {
		return s3store.ObjectMeta{}, false, err
	}
	b, ok := f.objs[key]
	return s3store.ObjectMeta{Key: key, Size: int64(len(b)), LastModified: time.Now().UTC()}, ok, nil
}

func (f *fakeStore) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objs[key] = b
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string, w io.WriterAt) error {
	b, ok := f.objs[key]
	if !ok {
		return errors.New("not found")
	}
	off := int64(0)
	for len(b) > 0 {
		n := len(b)
		if n > 512 {
			n = 512
		}
		if _, err := w.WriteAt(b[:n], off); err != nil {
			return err
		}
		off += int64(n)
		b = b[n:]
	}
	return nil
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]s3store.ObjectMeta, error) {
	var out []s3store.ObjectMeta
	for k, v := range f.objs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, s3store.ObjectMeta{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	delete(f.objs, key)
	return nil
}

type testEnv struct {
	t     *testing.T
	store *fakeStore
	keys  *s3store.KeyBuilder
	job   *Job
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st := newFakeStore()
	keys, err := s3store.NewKeyBuilder("", "c1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewJob(st, keys, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, store: st, keys: keys, job: job}
}

// putUnit 提交合法 manifest 与对应文件对象。
func (env *testEnv) putUnit(partition string, files map[string]string) *backupmeta.Manifest {
	env.t.Helper()
	m := &backupmeta.Manifest{
		FormatVersion: backupmeta.FormatVersion,
		Cluster:       "c1",
		Node:          "n1",
		Partition:     partition,
		CreatedAt:     time.Now().UTC(),
		SnapshotAt:    time.Now().UTC(),
		VLVersion:     "v1.21.0",
		ToolVersion:   "test",
	}
	for path, content := range files {
		sum := sha256.Sum256([]byte(content))
		m.Entries = append(m.Entries, backupmeta.Entry{
			Path:   path,
			Size:   int64(len(content)),
			SHA256: hex.EncodeToString(sum[:]),
		})
		m.FileCount++
		m.TotalBytes += int64(len(content))
		key, err := env.keys.FileKey(partition, path)
		if err != nil {
			env.t.Fatal(err)
		}
		env.store.objs[key] = []byte(content)
	}
	data, err := json.Marshal(m)
	if err != nil {
		env.t.Fatal(err)
	}
	mkey, err := env.keys.ManifestKey(partition)
	if err != nil {
		env.t.Fatal(err)
	}
	env.store.objs[mkey] = data
	return m
}

// 正常单元：全部通过。
func TestVerifyOK(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello", "indexdb/idx": "world"})

	res, err := env.job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.OK() {
		t.Fatalf("expected OK, missing=%v mismatched=%v", res.Missing, res.Mismatched)
	}
	if res.Files != 2 || res.Bytes != 10 {
		t.Errorf("files=%d bytes=%d, want 2/10", res.Files, res.Bytes)
	}
	if res.Deep {
		t.Error("shallow run must not set deep")
	}
}

// 深度校验：全量下载并比对 sha256，全部通过。
func TestVerifyDeepOK(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello"})

	job, err := NewJob(env.store, env.keys, Options{Deep: true, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	res, err := job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.OK() || !res.Deep {
		t.Fatalf("expected deep OK, missing=%v mismatched=%v deep=%v", res.Missing, res.Mismatched, res.Deep)
	}
}

// C3：无 manifest 的前缀禁止使用（退出码 4）。
func TestVerifyNoManifest(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.job.Run(context.Background(), "20261027")
	if errs.ExitCode(err) != errs.ExitNotFound {
		t.Fatalf("exit code: got %v (%v), want 4", err, errs.ExitCode(err))
	}
}

// manifest 损坏 → 退出码 5。
func TestVerifyBrokenManifest(t *testing.T) {
	env := newTestEnv(t)
	mkey, err := env.keys.ManifestKey("20261027")
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[mkey] = []byte("{broken")

	_, err = env.job.Run(context.Background(), "20261027")
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
}

// manifest 版本过新 → 退出码 5。
func TestVerifyVersionTooNew(t *testing.T) {
	env := newTestEnv(t)
	m := env.putUnit("20261027", map[string]string{"data/part.0": "x"})
	m.FormatVersion = backupmeta.FormatVersion + 1
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	mkey, _ := env.keys.ManifestKey("20261027")
	env.store.objs[mkey] = data

	_, err = env.job.Run(context.Background(), "20261027")
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
	if !errors.Is(err, backupmeta.ErrVersionTooNew) {
		t.Errorf("error should wrap ErrVersionTooNew: %v", err)
	}
}

// 对象缺失 → Missing 记录相对路径。
func TestVerifyMissingObject(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello", "data/part.1": "world"})
	key, _ := env.keys.FileKey("20261027", "data/part.1")
	delete(env.store.objs, key)

	res, err := env.job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OK() {
		t.Fatal("expected mismatch")
	}
	if len(res.Missing) != 1 || res.Missing[0] != "data/part.1" {
		t.Errorf("missing = %v, want [data/part.1]", res.Missing)
	}
	if len(res.Mismatched) != 0 {
		t.Errorf("mismatched = %v, want empty", res.Mismatched)
	}
}

// 对象被截断 → size 不一致。
func TestVerifySizeMismatch(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello"})
	key, _ := env.keys.FileKey("20261027", "data/part.0")
	env.store.objs[key] = []byte("hell")

	res, err := env.job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Mismatched) != 1 || res.Mismatched[0] != "data/part.0" {
		t.Errorf("mismatched = %v, want [data/part.0]", res.Mismatched)
	}
}

// 内容被篡改但长度一致：浅校验通过，深校验命中 sha256 分支。
func TestVerifyDeepSHA256Mismatch(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello"})
	key, _ := env.keys.FileKey("20261027", "data/part.0")
	env.store.objs[key] = []byte("helle")

	res, err := env.job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("shallow run: %v", err)
	}
	if !res.OK() {
		t.Fatalf("shallow run must pass, got missing=%v mismatched=%v", res.Missing, res.Mismatched)
	}

	job, err := NewJob(env.store, env.keys, Options{Deep: true, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	res, err = job.Run(context.Background(), "20261027")
	if err != nil {
		t.Fatalf("deep run: %v", err)
	}
	if len(res.Mismatched) != 1 || res.Mismatched[0] != "data/part.0" {
		t.Errorf("deep mismatched = %v, want [data/part.0]", res.Mismatched)
	}
}

// HEAD 网络错误 → 退出码 3。
func TestVerifyHeadError(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello"})
	key, _ := env.keys.FileKey("20261027", "data/part.0")
	env.store.headEr[key] = errors.New("boom")

	_, err := env.job.Run(context.Background(), "20261027")
	if errs.ExitCode(err) != errs.ExitNetwork {
		t.Fatalf("exit code: got %v (%v), want 3", err, errs.ExitCode(err))
	}
}

// List：只展示含已提交 manifest 的分区，按分区升序，统计来自 manifest。
func TestListUnits(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "hello", "data/part.1": "world"}) // 10 bytes
	env.putUnit("20261026", map[string]string{"data/part.0": "abc"})                           // 3 bytes
	// 残缺前缀：只有数据对象、无 manifest → 不展示（C3 / R-MF-01）。
	dkey, err := env.keys.FileKey("20261025", "data/part.0")
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[dkey] = []byte("ghost")

	units, err := env.job.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("units = %d, want 2", len(units))
	}
	if units[0].Partition != "20261026" || units[1].Partition != "20261027" {
		t.Errorf("order = %s,%s, want 20261026,20261027", units[0].Partition, units[1].Partition)
	}
	if units[0].Files != 1 || units[0].Bytes != 3 {
		t.Errorf("unit[0] files=%d bytes=%d, want 1/3", units[0].Files, units[0].Bytes)
	}
	if units[1].Files != 2 || units[1].Bytes != 10 {
		t.Errorf("unit[1] files=%d bytes=%d, want 2/10", units[1].Files, units[1].Bytes)
	}
}

// List：空桶返回空切片而非 nil（--json 渲染为 []）。
func TestListEmpty(t *testing.T) {
	env := newTestEnv(t)
	units, err := env.job.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if units == nil || len(units) != 0 {
		t.Fatalf("units = %v, want non-nil empty slice", units)
	}
}

// List：单个 manifest 损坏 → 整体失败，退出码 5。
func TestListBrokenManifest(t *testing.T) {
	env := newTestEnv(t)
	env.putUnit("20261027", map[string]string{"data/part.0": "x"})
	mkey, err := env.keys.ManifestKey("20261027")
	if err != nil {
		t.Fatal(err)
	}
	env.store.objs[mkey] = []byte("{broken")

	_, err = env.job.List(context.Background())
	if errs.ExitCode(err) != errs.ExitValidation {
		t.Fatalf("exit code: got %v (%v), want 5", err, errs.ExitCode(err))
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))
