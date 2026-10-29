// Package s3store 是 S3 兼容对象存储（生产为 RustFS）的访问层。
// 只认键与对象，不认备份业务语义；重试由 aws-sdk-go-v2 内建重试器完成（C12，设计文档 §12：上限 8 次）。
package s3store

import (
	"strings"
	"time"

	"github.com/vlogs-backup/vlogs-backup/internal/errs"
)

// KeyBuilder 统一生成 S3 对象键（C7：键必须含 v1/<cluster>/<node>/<YYYYMMDD>/），
// 禁止其他模块手工拼接键。
type KeyBuilder struct {
	prefix  string // 已归一：去首尾 "/"，空表示无前缀
	cluster string
	node    string
}

// NewKeyBuilder 创建键构造器；cluster/node 非空是 C7 硬约束。
func NewKeyBuilder(prefix, cluster, node string) (*KeyBuilder, error) {
	if cluster == "" {
		return nil, errs.Usage("cluster must not be empty")
	}
	if node == "" {
		return nil, errs.Usage("node must not be empty")
	}
	return &KeyBuilder{
		prefix:  strings.Trim(prefix, "/"),
		cluster: cluster,
		node:    node,
	}, nil
}

// validPartition 校验八位 UTC 日期且为真实存在的日期（C8，与 manifest 契约一致）。
func validPartition(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	_, err := time.Parse("20060102", s)
	return err == nil
}

// UnitPrefix 返回一个备份单元的键前缀：
// <prefix>/v1/<cluster>/<node>/<YYYYMMDD>/（含尾部分隔符，供 List 过滤）。
func (b *KeyBuilder) UnitPrefix(partition string) (string, error) {
	if !validPartition(partition) {
		return "", errs.Usage("partition %q must be a valid UTC date in YYYYMMDD", partition)
	}
	return b.base() + partition + "/", nil
}

// ManifestKey 返回备份单元的 manifest.json 键（唯一提交点，C3）。
func (b *KeyBuilder) ManifestKey(partition string) (string, error) {
	unit, err := b.UnitPrefix(partition)
	if err != nil {
		return "", err
	}
	return unit + "manifest.json", nil
}

// FileKey 返回分区内相对路径对应的对象键；反斜杠归一为 "/"。
func (b *KeyBuilder) FileKey(partition, relPath string) (string, error) {
	unit, err := b.UnitPrefix(partition)
	if err != nil {
		return "", err
	}
	clean, err := normalizeRelPath(relPath)
	if err != nil {
		return "", err
	}
	return unit + "files/" + clean, nil
}

// FileKeyToRel 从完整对象键反解出分区内相对路径；非本单元文件键返回 false。
// 供校验 / 清理路径使用，不做前缀猜测之外的推断。
func (b *KeyBuilder) FileKeyToRel(key, partition string) (string, bool) {
	unit, err := b.UnitPrefix(partition)
	if err != nil {
		return "", false
	}
	const sep = "files/"
	if !strings.HasPrefix(key, unit+sep) {
		return "", false
	}
	rel := strings.TrimPrefix(key, unit+sep)
	if _, err := normalizeRelPath(rel); err == nil {
		return rel, true
	}
	return "", false
}

// Base 返回所有备份单元的共同键前缀：<prefix>/v1/<cluster>/<node>/。
// 供 gc 列举日期前缀使用（设计文档 §7.4）。
func (b *KeyBuilder) Base() string { return b.base() }

// base 返回不含分区段的前缀：<prefix>/v1/<cluster>/<node>/。
func (b *KeyBuilder) base() string {
	parts := make([]string, 0, 4)
	if b.prefix != "" {
		parts = append(parts, b.prefix, "v1")
	} else {
		parts = append(parts, "v1")
	}
	parts = append(parts, b.cluster, b.node)
	return strings.Join(parts, "/") + "/"
}

// normalizeRelPath 校验并归一化分区内相对路径：
// 反斜杠归一为 "/"，拒绝空串、绝对路径、盘符与空段/"."/".." 段。
// 段校验与 manifest.Validate 的路径规则保持一致，键中不出现未归一段。
func normalizeRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return "", errs.Usage("relative path must not be empty")
	}
	if strings.HasPrefix(p, "/") {
		return "", errs.Usage("relative path %q must not be absolute", p)
	}
	if strings.Contains(p, ":") {
		return "", errs.Usage("relative path %q must not contain a drive separator", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errs.Usage("relative path %q contains invalid segments", p)
		}
	}
	return p, nil
}
