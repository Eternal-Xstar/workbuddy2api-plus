// Package logrotate 按大小轮转日志文件（进程内，无需外部工具）。
//
// 为什么需要：此前 `logs\wb2api.log` 只在**重启时**被 PowerShell 脚本挪成 .prev，
// 长期运行会一路膨胀（一天几 MB，跑一个月就是几百 MB），而排障时最想看的"刚才那段"
// 反而埋在文件末尾。
//
// 行为：
//   - 单文件超过 MaxBytes 就把当前文件滚成 `name.1`，旧的 `.1` 变 `.2`……最多保留
//     Keep 份，更旧的直接删；
//   - 轮转发生在**写入前**，所以单次写不会跨越两个文件；
//   - 全程持锁，多 goroutine（net/http 每个请求一个）并发写安全。
//
// 与 request_archive 的区别：那是请求元数据 JSONL（有独立上限与清理逻辑），
// 本包只管进程日志这一份文本。
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultMaxBytes 单文件默认上限（20MB）。
const DefaultMaxBytes int64 = 20 << 20

// DefaultKeep 默认保留份数。
const DefaultKeep = 3

// Writer 是一个会自我轮转的 io.Writer。
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// New 打开（或创建）path，返回可并发写入的轮转 writer。
// maxBytes<=0 用 DefaultMaxBytes；keep<=0 用 DefaultKeep。
func New(path string, maxBytes int64, keep int) (*Writer, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	w := &Writer{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 打开当前文件并把已有大小记下来（重启后接着算，不会因为不知道大小而不轮转）。
func (w *Writer) open() error {
	if dir := filepath.Dir(w.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

// Write 实现 io.Writer；超限时先轮转再写。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// 轮转失败不丢日志：继续往当前文件写，只是暂时不轮转。
			_ = err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked 关闭当前文件 → 依次改名 → 重开空文件。调用方持锁。
func (w *Writer) rotateLocked() error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}
	// .N-1 → .N（从最旧的开始，覆盖尾部）
	for i := w.keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.Remove(dst)
		_ = os.Rename(src, dst)
	}
	if _, err := os.Stat(w.path); err == nil {
		_ = os.Remove(w.path + "." + itoa(w.keep))
		_ = os.Rename(w.path, w.path+".1")
	}
	return w.open()
}

// Close 关闭底层文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// itoa 小工具，避免为一个数字引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
