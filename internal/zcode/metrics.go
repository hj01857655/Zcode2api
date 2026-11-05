package zcode

// 请求级观测：req_id / TTFT / token 四元组 / finish_reason / UA / cost。
// 语义对齐 zcode-switch 网关（Rust）的请求日志：每请求一行 JSONL 落盘
// （gateway.jsonl），日志页与排查都以此为唯一数据源。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UnitPrice 单价（USD / 百万 token）：(input, output, cache_read)。
// 对齐 zcode-switch gateway/server.rs unit_price（z.ai 牌价）。
func UnitPrice(model string) (in, out, cacheRead float64, ok bool) {
	switch model {
	case "GLM-5.3-FlashX":
		return 0.37, 1.25, 0.075, true
	case "GLM-5.3-Flash":
		return 0.15, 0.50, 0.03, true
	default:
		return 0, 0, 0, false
	}
}

// ComputeCost 按牌价折算成本（USD）；未知模型返回 ok=false。
// cache_write 不单独计价（Rust 侧同口径：tcr 参与折算，tcw 记录不折算）。
func ComputeCost(model string, tin, tout, tcr int64) (float64, bool) {
	pin, pout, pcr, ok := UnitPrice(model)
	if !ok {
		return 0, false
	}
	return (float64(tin)*pin + float64(tout)*pout + float64(tcr)*pcr) / 1e6, true
}

// RequestLog 一条请求日志（gateway.jsonl 一行一个 JSON）。
type RequestLog struct {
	TS        string  `json:"ts"`
	ReqID     string  `json:"req_id"`
	Proto     string  `json:"proto"` // chat / responses / anthropic
	Stream    bool    `json:"stream"`
	Model     string  `json:"model"`
	Account   string  `json:"account,omitempty"` // 池内命中账号 label
	Status    int     `json:"status"`            // 下游 HTTP 状态
	UpStatus  int     `json:"up_status,omitempty"`
	Finish    string  `json:"finish,omitempty"` // stop/length/tool_use/error…
	TTFTMs    int64   `json:"ttft_ms,omitempty"`
	TotalMs   int64   `json:"total_ms"`
	Tin       int64   `json:"tin"`
	Tout      int64   `json:"tout"`
	Tcr       int64   `json:"tcr"` // cache_read
	Tcw       int64   `json:"tcw"` // cache_creation
	Cost      float64 `json:"cost,omitempty"`
	CostKnown bool    `json:"cost_known,omitempty"`
	UA        string  `json:"ua,omitempty"`
	Err       string  `json:"err,omitempty"`
}

// LogSink 请求日志汇：内存环形缓冲（admin 页读）+ JSONL 追加落盘。
// 落盘按大小轮转（RotateMB，0 = 关闭）：超限改名 .1（单代，覆盖旧 .1），
// 对齐 zcode-switch 网关 log_file_max_mb 行为，防长期运行日志无限膨胀。
type LogSink struct {
	mu        sync.Mutex
	path      string // 空 = 不落盘
	ring      []RequestLog
	RotateMB  int64
}

// NewLogSink path 为空则只进内存环形缓冲；rotateMB>0 时落盘按大小轮转。
func NewLogSink(path string, rotateMB int64) *LogSink {
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	return &LogSink{path: path, ring: make([]RequestLog, 0, 256), RotateMB: rotateMB}
}

// Record 写一条日志：追加 JSONL + 内存 ring（保留最近 512 条）。
func (s *LogSink) Record(l RequestLog) {
	if l.TS == "" {
		l.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(s.ring, l)
	if len(s.ring) > 512 {
		s.ring = s.ring[len(s.ring)-512:]
	}
	if s.path == "" {
		return
	}
	// 轮转：写入前检查（低频检查足够，日志行 KB 级）；超限 → path.1 覆盖旧代
	if s.RotateMB > 0 {
		if st, err := os.Stat(s.path); err == nil && st.Size() >= s.RotateMB<<20 {
			_ = os.Rename(s.path, s.path+".1")
		}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Recent 返回最近 n 条（新的在前）；n<=0 返回全部 ring。
func (s *LogSink) Recent(n int) []RequestLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.ring) {
		n = len(s.ring)
	}
	out := make([]RequestLog, 0, n)
	for i := len(s.ring) - 1; i >= len(s.ring)-n; i-- {
		out = append(out, s.ring[i])
	}
	return out
}

// Obs 单请求观测器：入口创建，TTFT 在上游首个带内容的 SSE 帧时标记，
// Done 收尾写日志。并发字段仅在同一 handler goroutine 内读写，无需锁。
type Obs struct {
	Log      *LogSink
	Start    time.Time
	ReqID    string
	Proto    string
	Stream   bool
	Model    string
	Account  string
	UA       string
	ttftSet  bool
	TTFTMs   int64
	UpStatus int
}

// NewObs 请求入口调用。
func NewObs(log *LogSink, proto string, stream bool, model, ua string) *Obs {
	return &Obs{
		Log: log, Start: time.Now(),
		ReqID: "req_" + UUIDv4()[:20], Proto: proto, Stream: stream,
		Model: model, UA: ua,
	}
}

// FirstByte 上游首个内容帧（text/thinking/tool 增量）时标记 TTFT，幂等。
func (o *Obs) FirstByte() {
	if o == nil || o.ttftSet {
		return
	}
	o.ttftSet = true
	o.TTFTMs = time.Since(o.Start).Milliseconds()
}

// SetAccount 补记命中账号（池模式在 Client.Messages 内部选号后才可知，
// handler 构造 Obs 时还不知道——请求成功/失败收尾前回填）。幂等。
func (o *Obs) SetAccount(label string) {
	if o == nil || label == "" {
		return
	}
	o.Account = label
}

// Done 请求收尾：写日志（含 cost 折算）。
func (o *Obs) Done(status, upStatus int, finish string, tin, tout, tcr, tcw int64, errText string) {
	if o == nil || o.Log == nil {
		return
	}
	cost, known := ComputeCost(o.Model, tin, tout, tcr)
	o.Log.Record(RequestLog{
		ReqID: o.ReqID, Proto: o.Proto, Stream: o.Stream, Model: o.Model,
		Account: o.Account, Status: status, UpStatus: upStatus, Finish: finish,
		TTFTMs: o.TTFTMs, TotalMs: time.Since(o.Start).Milliseconds(),
		Tin: tin, Tout: tout, Tcr: tcr, Tcw: tcw,
		Cost: cost, CostKnown: known, UA: o.UA, Err: errText,
	})
}
