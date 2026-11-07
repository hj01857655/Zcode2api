package zcode

// LogSink 落盘轮转测试：持久句柄 + 记账字节数的轮转行为。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogSinkRotateOnSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.jsonl")
	sink := NewLogSink(path, 1) // 1MB 上限
	defer sink.Close()

	// 一条 1MB+ 的日志，写完立刻超限
	big := strings.Repeat("x", 1<<20)
	sink.Record(RequestLog{ReqID: "r1", UA: big})
	if sink.written < 1<<20 {
		t.Fatalf("written 记账错误: %d", sink.written)
	}
	// 第二条触发轮转：主文件改名 .1，新主文件只含第二条
	sink.Record(RequestLog{ReqID: "r2", UA: "small"})
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("轮转后 .1 不存在: %v", err)
	}
	b1, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b1), "\n"); n != 1 {
		t.Fatalf(".1 应只含第一条日志，got %d 行", n)
	}
	if !strings.Contains(string(b1), `"req_id":"r1"`) {
		t.Fatal(".1 内容应是 r1")
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b2), "\n"); n != 1 {
		t.Fatalf("新主文件应只含第二条日志，got %d 行", n)
	}
	if !strings.Contains(string(b2), `"req_id":"r2"`) {
		t.Fatal("主文件内容应是 r2")
	}
	if sink.written != int64(len(b2)) {
		t.Fatalf("轮转后 written 应重记新文件字节数: %d vs %d", sink.written, len(b2))
	}
	if got := sink.Recent(0); len(got) != 2 {
		t.Fatalf("ring 应有 2 条，got %d", len(got))
	}
}

func TestLogSinkPersistsHandleAcrossRecords(t *testing.T) {
	// 持久句柄：连续写不重开文件（无 .1 生成、字节数累计正确）
	path := filepath.Join(t.TempDir(), "gateway.jsonl")
	sink := NewLogSink(path, 0) // 0 = 关闭轮转
	defer sink.Close()
	if sink.f != nil {
		t.Fatal("句柄应惰性打开")
	}
	sink.Record(RequestLog{ReqID: "r1"})
	if sink.f == nil {
		t.Fatal("首条写入后句柄应保持打开")
	}
	sink.Record(RequestLog{ReqID: "r2"})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Fatalf("应 2 行 JSONL，got %d", n)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("关闭轮转时不应产生 .1")
	}
}

func TestLogSinkStartupAccounting(t *testing.T) {
	// 启动时从既有文件 Stat 初始化记账：已写满的旧文件 + 新 sink → 首条即轮转
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.jsonl")
	big := strings.Repeat("y", 1<<20)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := NewLogSink(path, 1)
	defer sink.Close()
	if sink.written != 1<<20 {
		t.Fatalf("启动记账应读出既有文件大小: %d", sink.written)
	}
	sink.Record(RequestLog{ReqID: "r1"})
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("既有文件超限，首条写入应触发轮转: %v", err)
	}
}
