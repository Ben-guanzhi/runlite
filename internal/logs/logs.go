// Package logs 提供每次运行的日志管道：内存环形缓冲 + 全量落盘 + 增量订阅。
package logs

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"runlite/internal/netx"
	"strings"
	"sync"
	"time"
)

type Line struct {
	Seq  int    `json:"seq"`
	TS   string `json:"ts,omitempty"`
	Err  bool   `json:"err,omitempty"`
	Text string `json:"text"`
}

const maxLineBytes = 1 << 20 // 1MB 以上按截断处理，避免单行刷爆内存

type Log struct {
	mu       sync.Mutex
	path     string
	ring     []Line
	head     int
	size     int
	count    int
	f        *os.File
	w        *bufio.Writer
	closed   bool
	lastRead time.Time
	// nowCached 的缓存字段：pump 单 goroutine 顺序调 Append，毫秒级窗口内
	// 多行共享同一时间戳字符串，省掉高频路径上每行一次的 now() 分配。
	cachedTS   string
	cachedTime time.Time
}

func Create(path string, ringSize int) (*Log, error) {
	if ringSize <= 0 {
		ringSize = 2000
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{
		path: path, ring: make([]Line, ringSize), size: ringSize,
		f: f, w: bufio.NewWriterSize(f, 64<<10),
	}, nil
}

func (l *Log) Path() string { return l.path }

func (l *Log) Append(errStream bool, text string) Line {
	text = strings.TrimRight(stripANSI(strings.ReplaceAll(netx.DecodeConsole(text), "\r", "")), " \t")
	if len(text) > maxLineBytes {
		text = text[:maxLineBytes] + "…(行超长已截断)"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	ts := l.nowCached()
	ln := Line{Seq: l.count, TS: ts, Err: errStream, Text: text}
	l.ring[l.head] = ln
	l.head = (l.head + 1) % l.size
	if !l.closed {
		if _, err := fmtFprintln(l.w, ln); err != nil {
			l.closed = true // 磁盘异常时降级为纯内存，不影响被管进程
		}
	}
	return ln
}

func (l *Log) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed && l.w != nil {
		_ = l.w.Flush()
	}
}

// fmtFprintln 落盘格式带时间戳与 stderr 标记；内存里保留原始行，便于 UI 着色。
//
// 用 io.WriteString 分段写，不走 fmt.Fprintln：老写法每行都要做
// prefix += " [stderr] " 和 prefix+ln.Text 两次串拼接，日志行多的时候
// 那两次无谓的分配比写出本身还贵。bufio.Writer 有 WriteString 方法，
// io.WriteString 会直接派给它，全程零分配。
func fmtFprintln(w io.Writer, ln Line) (int, error) {
	_, err := io.WriteString(w, ln.TS)
	if err != nil {
		return 0, err
	}
	if ln.Err {
		if _, err := io.WriteString(w, " [stderr] "); err != nil {
			return 0, err
		}
	} else {
		if _, err := io.WriteString(w, " "); err != nil {
			return 0, err
		}
	}
	if _, err := io.WriteString(w, ln.Text); err != nil {
		return 0, err
	}
	_, err = io.WriteString(w, "\n")
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Tail 返回 seq 之后的行；n<=0 表示不限条数（仍受 since 约束）。
func (l *Log) Tail(n int, since int) []Line {
	l.mu.Lock()
	// 内存环形缓冲已释放（ReleaseRing/ReleaseIdle）时，透明退回磁盘读：
	// 全量日志一直在盘上，读文件这条路径 daemon 重启后本来就在用，口径一致。
	if l.ring == nil {
		path := l.path
		l.mu.Unlock()
		out, err := ReadFile(path, n, since)
		if err != nil {
			return nil
		}
		return out
	}
	l.lastRead = time.Now()
	defer l.mu.Unlock()
	start := (l.head - l.count + l.size*2) % l.size
	avail := l.count
	if avail > l.size {
		avail = l.size
	}
	if avail == 0 {
		return nil
	}
	// 单遍过滤：直接只追加 seq>since 的行，不做"先全收再筛"的二次拷贝。
	// 老实现在 GUI 每秒轮询时会白分配一整份 l.size 长度的切片。
	out := make([]Line, 0, avail)
	for i := 0; i < avail; i++ {
		ln := l.ring[(start+i)%l.size]
		if ln.Seq > since {
			out = append(out, ln)
		}
	}
	if len(out) > 0 {
		oldest := out[0].Seq
		if since < oldest-1 {
			// 请求的起点已被环形覆盖，补一条缺口提示，避免静默丢行
			out = append([]Line{{Seq: since + 1, Text: "…(更早 " + itoa(oldest-since-1) + " 行已滚出内存，完整内容见日志文件)"}}, out...)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// defaultFileTail 与内存环形缓冲的默认容量一致：n<=0 时都只给最近这么多行。
const defaultFileTail = 2000

// ReadFile 从磁盘上的日志文件取尾部 n 行。
//
// daemon 重启后内存里的环形缓冲自然没了，但文件一直都在：看日志是这个产品最主要的
// 用途，不能在这种时候把用户推回去自己开文件。
//
// 单遍流式扫描，只保留最近 n 行和已扫过的行数，所以内存有界；又因为每一行追加时都会
// 落盘，文件行号就等于内存版的 Seq，增量拉取（since）在两条路径上口径一致。
func ReadFile(path string, n, since int) ([]Line, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if n <= 0 || n > defaultFileTail {
		n = defaultFileTail
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes+64<<10)
	ring := make([]Line, n)
	head, total := 0, 0
	for sc.Scan() {
		total++
		ring[head] = parseFileLine(total, sc.Text())
		head = (head + 1) % n
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	kept := total
	if kept > n {
		kept = n
	}
	start := (head - kept + n*2) % n
	out := make([]Line, 0, kept+1)
	for i := 0; i < kept; i++ {
		ln := ring[(start+i)%n]
		if ln.Seq > since {
			out = append(out, ln)
		}
	}
	if total > kept && len(out) > 0 {
		out = append([]Line{{Seq: total - kept, Text: "…(更早 " + itoa(total-kept) + " 行未返回，完整内容见日志文件)"}}, out...)
	}
	return out, nil
}

// parseFileLine 还原落盘格式 "时间戳 [stderr] 正文"。时间戳形状对不上就原样返回，
// 这样混进来的旧格式或手工编辑过的文件也不会被拆错。
//
// 先判时间戳形状，再按需 DecodeConsole：老写法一进门就 DecodeConsole(raw)，
// 形状对得上时又覆盖 Text——第一次解码白做（GBK 日志每条行多一次转换）。
func parseFileLine(seq int, raw string) Line {
	sp := strings.IndexByte(raw, ' ')
	if sp != 12 || raw[2] != ':' || raw[5] != ':' || raw[8] != '.' {
		return Line{Seq: seq, Text: netx.DecodeConsole(raw)}
	}
	ln := Line{Seq: seq, TS: raw[:sp]}
	rest := raw[sp+1:]
	if marker := "[stderr] "; strings.HasPrefix(rest, marker) {
		ln.Err = true
		ln.Text = netx.DecodeConsole(rest[len(marker):])
	} else {
		ln.Text = netx.DecodeConsole(rest)
	}
	return ln
}

func (l *Log) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// ReleaseIdle 把"已结束且一段时间没被读过"的日志的环形缓冲清空，让内存退回磁盘读。
// 结束的日志保留在 Bus 里供 UI 拉取，但 2000 行 × 几十个历史运行是 daemon 自己堆内存的
// 大头；日志全量本来就落盘，释放后 Tail 走 ReadFile 那条路，用户看到的内容不变。
func (l *Log) ReleaseIdle(older time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed || l.ring == nil {
		return false
	}
	if !l.lastRead.IsZero() && time.Since(l.lastRead) >= older {
		l.ring = nil
		return true
	}
	return false
}

// ReleaseRing 立即清空环形缓冲（仅在调用方确认该日志已结束、不再需要内存里那份时用）。
func (l *Log) ReleaseRing() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.ring = nil
	}
}

func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed && l.f == nil {
		return
	}
	if l.w != nil {
		_ = l.w.Flush()
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	l.w = nil
	l.closed = true
}

// Bus 按 runID 保存日志，供 CLI/GUI/MCP 增量拉取。
type Bus struct {
	mu    sync.RWMutex
	logs  map[string]*Log
	order []string
	max   int
}

func NewBus(keep int) *Bus {
	if keep <= 0 {
		keep = 20
	}
	return &Bus{logs: map[string]*Log{}, max: keep}
}

func (b *Bus) Put(runID string, l *Log) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs[runID] = l
	b.order = append(b.order, runID)
	for len(b.order) > b.max {
		old := b.order[0]
		b.order = b.order[1:]
		if l, ok := b.logs[old]; ok {
			l.Close()
			delete(b.logs, old)
		}
	}
}

func (b *Bus) Get(runID string) (*Log, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	l, ok := b.logs[runID]
	return l, ok
}

// ReleaseIdle 清空 Bus 里所有"已结束且空闲"日志的环形缓冲，返回释放了多少条。
// 由 daemon 的周期任务调用，是控制 daemon 自身堆内存的主要手段：活跃运行的日志仍
// 留在内存（要高频 Tail），只有结束的、没人再读的才退回磁盘。
func (b *Bus) ReleaseIdle(older time.Duration) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, l := range b.logs {
		if l.ReleaseIdle(older) {
			n++
		}
	}
	return n
}

var ansiPre = []string{"\x1b[", "\x9b["}

// ansiBufPool 复用 stripANSI 的 []byte 缓冲。每条带转义的日志行原先要分配
// 一次 make([]byte, 0, len(s)) + 一次 string(buf)，pool 把前者摊到池里，
// 只剩 string(buf) 这一次分配。pump 是单 goroutine 顺序调，pool 命中率很高。
var ansiBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 256); return &b },
}

// stripANSI 单遍扫过整行，剥掉 ESC[ 或 0x9b[ 开头、以 m/h/l 结尾的转义序列。
//
// 用 []byte 累积输出、start 记下"上一次落笔"的下标：每跳过一段转义，
// 就把它前面尚未落笔的连续文本一次性写完再重置起点。旧实现每遇到一个转义就
// s[:i] + s[j+1:] 把整行重拼一次——JVM 启动日志满屏颜色码，一行文本能触发
// 几十次全串拷贝。buf 从 sync.Pool 取，无转义时连取都省，直接返回原串。
func stripANSI(s string) string {
	start := 0
	for i := 0; i < len(s); {
		escaped := false
		for _, pre := range ansiPre {
			if i+len(pre) > len(s) || s[i:i+len(pre)] != pre {
				continue
			}
			j := i + len(pre)
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9') || s[j] == '?') {
				j++
			}
			if j < len(s) && (s[j] == 'm' || s[j] == 'h' || s[j] == 'l') {
				// 找到第一个转义才取池里的缓冲：大多数行没转义，零分配直接返回。
				bufp := ansiBufPool.Get().(*[]byte)
				buf := (*bufp)[:0]
				if i > start {
					buf = append(buf, s[start:i]...)
				}
				start = j + 1
				i = j + 1
				// 继续扫剩余转义
				for i < len(s) {
					esc2 := false
					for _, pre := range ansiPre {
						if i+len(pre) > len(s) || s[i:i+len(pre)] != pre {
							continue
						}
						j2 := i + len(pre)
						for j2 < len(s) && (s[j2] == ';' || (s[j2] >= '0' && s[j2] <= '9') || s[j2] == '?') {
							j2++
						}
						if j2 < len(s) && (s[j2] == 'm' || s[j2] == 'h' || s[j2] == 'l') {
							if i > start {
								buf = append(buf, s[start:i]...)
							}
							start = j2 + 1
							i = j2 + 1
							esc2 = true
							break
						}
					}
					if !esc2 {
						i++
					}
				}
				if start < len(s) {
					buf = append(buf, s[start:]...)
				}
				out := string(buf)
				*bufp = buf
				ansiBufPool.Put(bufp)
				return out
			}
		}
		if !escaped {
			i++
		}
	}
	// 一次转义都没剥掉：零分配，原样返回。
	return s
}

// now 只保留到毫秒的时间戳。手写 12 字节格式而非调 time.Format：
// Format 内部要 make 一个 []byte（实测 2 alloc/op），AppendFormat 又会因为返回
// 的 []byte 引用栈缓冲导致逃逸（还是 2 alloc/op）。直接按字段写进 [12]byte，
// string(buf[:]) 是唯一一次分配——日志每一行都要调它。
func now() string {
	t := time.Now()
	h, m, s := t.Clock()
	ms := t.Nanosecond() / 1e6
	var buf [12]byte
	buf[0] = byte('0' + h/10)
	buf[1] = byte('0' + h%10)
	buf[2] = ':'
	buf[3] = byte('0' + m/10)
	buf[4] = byte('0' + m%10)
	buf[5] = ':'
	buf[6] = byte('0' + s/10)
	buf[7] = byte('0' + s%10)
	buf[8] = '.'
	buf[9] = byte('0' + ms/100)
	buf[10] = byte('0' + (ms/10)%10)
	buf[11] = byte('0' + ms%10)
	return string(buf[:])
}

// nowCached 在毫秒级窗口内复用上一次格式化的时间戳字符串。pump 是单 goroutine
// 顺序调 Append 且 Append 已持锁，cachedTS/cachedTime 无竞争；500 行/秒时相邻
// 行落在同一毫秒的概率约一半，命中时零分配——把日志热路径上每行一次 now()
// 的分配摊到约两次。跨毫秒才真正调 now() 走 1 alloc。
func (l *Log) nowCached() string {
	t := time.Now()
	if !l.cachedTime.IsZero() && t.Sub(l.cachedTime) < time.Millisecond {
		return l.cachedTS
	}
	ts := now()
	l.cachedTS = ts
	l.cachedTime = t
	return ts
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
