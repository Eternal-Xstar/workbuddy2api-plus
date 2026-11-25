package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// 超过上限自动轮转：当前文件清空，旧内容进 .1，份数不超过 keep。
func TestRotateBySize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	w, err := New(p, 120, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := strings.Repeat("a", 50) + "\n"
	for i := 0; i < 12; i++ { // 共 612 字节，必然触发多次轮转
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	cur := readAll(t, p)
	if int64(len(cur)) > 120 {
		t.Fatalf("当前文件 %d 字节，超过上限 120", len(cur))
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf(".1 不存在: %v", err)
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("keep=2 不该产生 .3")
	}
}

// 重启场景：已有文件已经大于上限，首次写入就要轮转（不能因为"文件不是自己建的"就不轮）。
func TestRotateOnOpenOversize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(p, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, p); got != "hello\n" {
		t.Fatalf("当前文件内容 = %q", got)
	}
	old := readAll(t, p+".1")
	if len(old) != 500 {
		t.Fatalf(".1 内容长度 = %d，期望 500", len(old))
	}
}

// 目录不存在时自动创建（全新部署 logs/ 还没建）。
func TestCreatesDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "app.log")
	w, err := New(p, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p), "ok") {
		t.Fatal("写入失败")
	}
}

// 并发写不能丢行、不能 panic（net/http 每请求一个 goroutine）。
func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	w, err := New(p, 4096, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if _, err := w.Write([]byte("line-payload-1234567890\n")); err != nil {
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// 只验"没崩、文件还在、当前文件不超限"。
	if fi, err := os.Stat(p); err != nil || fi.Size() > 4096 {
		t.Fatalf("当前文件状态异常: %v size=%d", err, fi.Size())
	}
}
