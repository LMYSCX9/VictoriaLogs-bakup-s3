package vl

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// recorder 记录 httptest 服务端收到的请求（客户端同步调用，无并发竞争）。
type recorder struct {
	method string
	path   string
	query  map[string][]string
	header http.Header
}

func (r *recorder) capture(w http.ResponseWriter, req *http.Request) {
	r.method = req.Method
	r.path = req.URL.Path
	r.query = map[string][]string(req.URL.Query())
	r.header = req.Header.Clone()
}

// newTestClient 基于httptest构造客户端；handler 负责响应，recorder 记录请求。
func newTestClient(t *testing.T, status int, body string, opts ...Option) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(w, r)
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, NoAuth{}, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, rec
}

func TestNewAuth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mode     string
		username string
		password string
		token    string
		wantType any
		wantCode int
	}{
		{name: "none", mode: "none", wantType: NoAuth{}, wantCode: errs.ExitOK},
		{name: "basic", mode: "basic", username: "u", password: "p", wantType: BasicAuth{Username: "u", Password: "p"}, wantCode: errs.ExitOK},
		{name: "basic_missing_user", mode: "basic", password: "p", wantCode: errs.ExitUsage},
		{name: "basic_missing_password", mode: "basic", username: "u", wantCode: errs.ExitUsage},
		{name: "bearer", mode: "bearer", token: "tok", wantType: BearerAuth{Token: "tok"}, wantCode: errs.ExitOK},
		{name: "bearer_missing_token", mode: "bearer", wantCode: errs.ExitUsage},
		{name: "unknown_mode", mode: "digest", wantCode: errs.ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewAuth(tc.mode, tc.username, tc.password, tc.token)
			if tc.wantCode == errs.ExitOK {
				if err != nil {
					t.Fatalf("NewAuth: %v", err)
				}
				if got != tc.wantType {
					t.Fatalf("got %#v, want %#v", got, tc.wantType)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if code := errs.ExitCode(err); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

func TestAuthHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		auth     AuthInjector
		wantHead string
	}{
		{name: "none", auth: NoAuth{}, wantHead: ""},
		{name: "basic", auth: BasicAuth{Username: "u", Password: "p"}, wantHead: "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))},
		{name: "bearer", auth: BearerAuth{Token: "tok"}, wantHead: "Bearer tok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotHead atomic.Pointer[string]
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h := r.Header.Get("Authorization")
				gotHead.Store(&h)
				_, _ = w.Write([]byte("[]"))
			}))
			defer srv.Close()
			c, err := NewClient(srv.URL, tc.auth)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if err := c.ForceFlush(context.Background()); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			if got := *gotHead.Load(); got != tc.wantHead {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantHead)
			}
		})
	}
}

func TestEndpoints(t *testing.T) {
	t.Parallel()
	c, rec := newTestClient(t, http.StatusOK, "[]")
	ctx := context.Background()

	if err := c.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/force_flush", nil)

	if _, err := c.SnapshotCreate(ctx, "202610"); err != nil {
		t.Fatalf("SnapshotCreate: %v", err)
	}
	// 双参数兼容：VL ≥ v1.44.0 认 partition_prefix，v1.31~v1.43 只认 name（设计文档 §9）。
	checkReq(t, rec, http.MethodPost, "/internal/partition/snapshot/create", map[string][]string{
		"partition_prefix": {"202610"},
		"name":             {"202610"},
	})

	if err := c.SnapshotDelete(ctx, "/data/snapshots/20261027"); err != nil {
		t.Fatalf("SnapshotDelete: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/snapshot/delete", map[string][]string{"path": {"/data/snapshots/20261027"}})

	if _, err := c.SnapshotList(ctx); err != nil {
		t.Fatalf("SnapshotList: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/snapshot/list", nil)

	if _, err := c.PartitionList(ctx); err != nil {
		t.Fatalf("PartitionList: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/list", nil)

	if err := c.Detach(ctx, "20261027"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/detach", map[string][]string{"name": {"20261027"}})

	if err := c.Attach(ctx, "20261027"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/attach", map[string][]string{"name": {"20261027"}})
}

func checkReq(t *testing.T, rec *recorder, method, path string, query map[string][]string) {
	t.Helper()
	if rec.method != method {
		t.Fatalf("method = %s, want %s", rec.method, method)
	}
	if rec.path != path {
		t.Fatalf("path = %s, want %s", rec.path, path)
	}
	if len(rec.query) != len(query) {
		t.Fatalf("query = %v, want %v", rec.query, query)
	}
	for k, want := range query {
		got, ok := rec.query[k]
		if !ok || len(got) != 1 || got[0] != want[0] {
			t.Fatalf("query[%s] = %v, want %v", k, got, want)
		}
	}
}

func TestParseStringArray(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.StatusOK, `["/snap/20261027","/snap/20261028"]`)
	got, err := c.PartitionList(context.Background())
	if err != nil {
		t.Fatalf("PartitionList: %v", err)
	}
	if len(got) != 2 || got[0] != "/snap/20261027" || got[1] != "/snap/20261028" {
		t.Fatalf("got %v", got)
	}

	c, _ = newTestClient(t, http.StatusOK, "[]")
	got, err = c.PartitionList(context.Background())
	if err != nil {
		t.Fatalf("PartitionList: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}

	c, _ = newTestClient(t, http.StatusOK, `{"names":[]}`)
	_, err = c.PartitionList(context.Background())
	if err == nil || errs.ExitCode(err) != errs.ExitGeneral {
		t.Fatalf("want exit code %d, got %v", errs.ExitGeneral, err)
	}
}

func TestSnapshotDelete204(t *testing.T) {
	t.Parallel()
	// VL 源码：snapshot/delete 成功返回 204 No Content。
	c, rec := newTestClient(t, http.StatusNoContent, "")
	if err := c.SnapshotDelete(context.Background(), "/snap/20261027"); err != nil {
		t.Fatalf("SnapshotDelete: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/internal/partition/snapshot/delete", map[string][]string{"path": {"/snap/20261027"}})
}

func TestNotFoundNoRetry(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, NoAuth{}, WithRetry(5, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.PartitionList(context.Background())
	if err == nil || errs.ExitCode(err) != errs.ExitNotFound {
		t.Fatalf("want exit code %d, got %v", errs.ExitNotFound, err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (404 must not retry)", attempts.Load())
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	t.Run("ok", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusOK, "[]")
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("Probe: %v", err)
		}
	})
	t.Run("snapshot_list_404", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusNotFound, "")
		err := c.Probe(context.Background())
		if err == nil || errs.ExitCode(err) != errs.ExitPrecondition {
			t.Fatalf("want exit code %d, got %v", errs.ExitPrecondition, err)
		}
		if !strings.Contains(err.Error(), "vl version too old") || !strings.Contains(err.Error(), "snapshot/list") {
			t.Fatalf("message = %q", err.Error())
		}
	})
	t.Run("partition_list_404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/internal/partition/list" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte("[]"))
		}))
		defer srv.Close()
		c, err := NewClient(srv.URL, NoAuth{}, WithRetry(1, time.Millisecond))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		err = c.Probe(context.Background())
		if err == nil || errs.ExitCode(err) != errs.ExitPrecondition {
			t.Fatalf("want exit code %d, got %v", errs.ExitPrecondition, err)
		}
		if !strings.Contains(err.Error(), "partition/list") {
			t.Fatalf("message = %q", err.Error())
		}
	})
	t.Run("server_error_passthrough", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusInternalServerError, "", WithRetry(1, time.Millisecond))
		err := c.Probe(context.Background())
		if err == nil || errs.ExitCode(err) != errs.ExitNetwork {
			t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, err)
		}
	})
}

func TestRetry5xxThenSuccess(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, NoAuth{}, WithRetry(5, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.PartitionList(context.Background())
	if err != nil {
		t.Fatalf("PartitionList: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestRetry5xxExhausted(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, NoAuth{}, WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, cerr := c.PartitionList(context.Background())
	if cerr == nil || errs.ExitCode(cerr) != errs.ExitNetwork {
		t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, cerr)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	if !strings.Contains(cerr.Error(), "vl server returned 500") {
		t.Fatalf("message = %q", cerr.Error())
	}
}

func Test4xxNoRetry(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, NoAuth{}, WithRetry(5, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, cerr := c.PartitionList(context.Background())
	if cerr == nil || errs.ExitCode(cerr) != errs.ExitGeneral {
		t.Fatalf("want exit code %d, got %v", errs.ExitGeneral, cerr)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx must not retry)", attempts.Load())
	}
}

// flakyTransport 前 failFirst 次注入传输层错误，之后转发到真实目标。
type flakyTransport struct {
	failFirst atomic.Int32
	base      http.RoundTripper
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.failFirst.Add(-1) >= 0 {
		return nil, errors.New("connection reset by peer")
	}
	return f.base.RoundTrip(req)
}

func TestNetworkErrorRetry(t *testing.T) {
	t.Parallel()
	t.Run("then_success", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusOK, "[]", WithRetry(3, time.Millisecond),
			WithHTTPClient(&http.Client{Transport: &flakyTransport{base: http.DefaultTransport}}))
		if _, err := c.PartitionList(context.Background()); err != nil {
			t.Fatalf("PartitionList: %v", err)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		tr := &flakyTransport{base: http.DefaultTransport}
		tr.failFirst.Store(99)
		c, _ := newTestClient(t, http.StatusOK, "[]", WithRetry(3, time.Millisecond),
			WithHTTPClient(&http.Client{Transport: tr}))
		_, err := c.PartitionList(context.Background())
		if err == nil || errs.ExitCode(err) != errs.ExitNetwork {
			t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, err)
		}
		if !strings.Contains(err.Error(), "request failed") {
			t.Fatalf("message = %q", err.Error())
		}
	})
}

func TestAttachErrorBodySurfaced(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`cannot open partition part/20261027: missing parts`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, NoAuth{}, WithRetry(1, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cerr := c.Attach(context.Background(), "20261027")
	if cerr == nil || errs.ExitCode(cerr) != errs.ExitNetwork {
		t.Fatalf("want exit code %d, got %v", errs.ExitNetwork, cerr)
	}
	if !strings.Contains(cerr.Error(), "missing parts") {
		t.Fatalf("error body not surfaced: %q", cerr.Error())
	}
}

func TestCanceled(t *testing.T) {
	t.Parallel()
	t.Run("before_attempt", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusOK, "[]")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := c.ForceFlush(ctx)
		if err == nil || errs.ExitCode(err) != errs.ExitCanceled {
			t.Fatalf("want exit code %d, got %v", errs.ExitCanceled, err)
		}
	})
	t.Run("during_retry_wait", func(t *testing.T) {
		c, _ := newTestClient(t, http.StatusInternalServerError, "", WithRetry(5, 50*time.Millisecond))
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()
		err := c.ForceFlush(ctx)
		if err == nil || errs.ExitCode(err) != errs.ExitCanceled {
			t.Fatalf("want exit code %d, got %v", errs.ExitCanceled, err)
		}
	})
}

func TestNewClientURLValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
		want int
	}{
		{name: "ok_trailing_slash", url: "http://vl:9428/", want: errs.ExitOK},
		{name: "ok_path_prefix", url: "https://vmauth.example/vl/", want: errs.ExitOK},
		{name: "bad_scheme", url: "ftp://vl:9428", want: errs.ExitUsage},
		{name: "no_host", url: "http:///path", want: errs.ExitUsage},
		{name: "parse_error", url: "http://[::1", want: errs.ExitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(tc.url, NoAuth{})
			if tc.want == errs.ExitOK {
				if err != nil {
					t.Fatalf("NewClient: %v", err)
				}
				if strings.HasSuffix(c.baseURL, "/") {
					t.Fatalf("baseURL = %q, trailing slash not trimmed", c.baseURL)
				}
				return
			}
			if err == nil || errs.ExitCode(err) != tc.want {
				t.Fatalf("want exit code %d, got %v", tc.want, err)
			}
		})
	}
}

func TestBaseURLPathPrefix(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(w, r)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/vl", NoAuth{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.PartitionList(context.Background()); err != nil {
		t.Fatalf("PartitionList: %v", err)
	}
	checkReq(t, rec, http.MethodPost, "/vl/internal/partition/list", nil)
}

func TestBackoffDelay(t *testing.T) {
	t.Parallel()
	const initial = 100 * time.Millisecond
	// attempt=1：无指数增长，jitter 在 [d/2, d)。
	for range 20 {
		if d := backoffDelay(initial, 1); d < initial/2 || d >= initial {
			t.Fatalf("attempt 1: d = %v, want [%v, %v)", d, initial/2, initial)
		}
	}
	// attempt=3：指数到 4d，jitter 在 [2d, 4d)。
	for range 20 {
		if d := backoffDelay(initial, 3); d < 2*initial || d >= 4*initial {
			t.Fatalf("attempt 3: d = %v, want [%v, %v)", d, 2*initial, 4*initial)
		}
	}
	// 溢出保护：倍增溢出为负时回退 1h。
	if d := backoffDelay(time.Duration(1)<<62, 2); d != time.Hour {
		t.Fatalf("overflow: d = %v, want 1h", d)
	}
	// 极小初值：half 为 0 时原样返回。
	if d := backoffDelay(time.Nanosecond, 1); d != time.Nanosecond {
		t.Fatalf("tiny: d = %v, want 1ns", d)
	}
}
