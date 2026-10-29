package s3store

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 是进程内 S3 兼容实现（§15 允许内存实现），覆盖测试所需子集：
// PutObject / GetObject / HeadObject / DeleteObject / ListObjectsV2（分页）/
// CreateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload。
// 不校验签名与 checksum 头；故障经字段注入，不加业务语义。
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte // uploadID → partNumber → data
	seq     int
	aborts  []string // 收到的 Abort 请求的 uploadID

	// 故障注入
	failPartN     int // UploadPart 的 partNumber 等于该值时返回 500（0 不注入）
	failComplete  int // CompleteMultipartUpload 剩余失败次数（SDK 重试可耗尽）
	lastKeyCalled string
}

// newServer 启动 fakeS3 的 httptest 服务，生命周期绑定测试。
func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		objects: map[string][]byte{},
		uploads: map[string]map[int][]byte{},
	}
}

// xml 结构与 S3 协议响应形态一致，仅覆盖 SDK 解析所需字段。
type xmlContents struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

type xmlListResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	KeyCount              int           `xml:"KeyCount"`
	IsTruncated           bool          `xml:"IsTruncated"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	Contents              []xmlContents `xml:"Contents"`
}

type xmlInitiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

type xmlCompleteResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(rest, "/")
	q := r.URL.Query()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastKeyCalled = key

	switch r.Method {
	case http.MethodPut:
		if q.Get("partNumber") != "" && q.Get("uploadId") != "" {
			f.handleUploadPart(w, r, q.Get("uploadId"), key)
			return
		}
		body := readAllBody(r)
		f.objects[key] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%s"`, key))
		w.WriteHeader(http.StatusOK)

	case http.MethodPost:
		if _, ok := q["uploads"]; ok {
			f.seq++
			id := fmt.Sprintf("up-%d", f.seq)
			f.uploads[id] = map[int][]byte{}
			writeXML(w, http.StatusOK, xmlInitiateResult{Bucket: bucket, Key: key, UploadId: id})
			return
		}
		if id := q.Get("uploadId"); id != "" {
			f.handleComplete(w, bucket, key, id)
			return
		}
		w.WriteHeader(http.StatusBadRequest)

	case http.MethodGet:
		if key == "" {
			f.handleList(w, bucket, q)
			return
		}
		data, ok := f.objects[key]
		if !ok {
			writeXMLError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)

	case http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if id := q.Get("uploadId"); id != "" {
			delete(f.uploads, id)
			f.aborts = append(f.aborts, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeS3) handleUploadPart(w http.ResponseWriter, r *http.Request, uploadID, key string) {
	partN, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.failPartN == partN {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	parts, ok := f.uploads[uploadID]
	if !ok {
		writeXMLError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	parts[partN] = readAllBody(r)
	w.Header().Set("ETag", fmt.Sprintf(`"part-%s-%d"`, key, partN))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) handleComplete(w http.ResponseWriter, bucket, key, uploadID string) {
	if f.failComplete > 0 {
		f.failComplete--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	parts, ok := f.uploads[uploadID]
	if !ok {
		writeXMLError(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	nums := make([]int, 0, len(parts))
	for n := range parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var merged []byte
	for _, n := range nums {
		merged = append(merged, parts[n]...)
	}
	f.objects[key] = merged
	delete(f.uploads, uploadID)
	writeXML(w, http.StatusOK, xmlCompleteResult{
		Location: "http://" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: `"merged"`,
	})
}

func (f *fakeS3) handleList(w http.ResponseWriter, bucket string, q map[string][]string) {
	prefix := getQuery(q, "prefix")
	maxKeys := 1000
	if v := getQuery(q, "max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxKeys = n
		}
	}
	offset := 0
	if v := getQuery(q, "continuation-token"); v != "" {
		offset, _ = strconv.Atoi(v)
	}

	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	end := offset + maxKeys
	truncated := false
	if end < len(keys) {
		truncated = true
	} else {
		end = len(keys)
	}
	out := xmlListResult{Name: bucket, Prefix: prefix, KeyCount: end - offset, IsTruncated: truncated}
	if truncated {
		out.NextContinuationToken = strconv.Itoa(end)
	}
	for _, k := range keys[offset:end] {
		out.Contents = append(out.Contents, xmlContents{
			Key: k, Size: int64(len(f.objects[k])), LastModified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	writeXML(w, http.StatusOK, out)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

func writeXMLError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, xml.Header+"<Error><Code>%s</Code></Error>", code)
}

func readAllBody(r *http.Request) []byte {
	buf := make([]byte, 0, r.ContentLength)
	tmp := make([]byte, 32<<10)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func getQuery(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}
