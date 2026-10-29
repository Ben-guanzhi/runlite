//go:build windows

package netx

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

const isWindows = true

// hideWindow 让辅助命令（netstat/taskkill）不闪控制台窗口。
var hideWindow = &syscall.SysProcAttr{HideWindow: true}

// KillTree 结束 root 及其整棵子进程树。
// force=false 先发普通结束请求（对无窗口进程通常仍会被拒），force=true 走 /F。
// 注意：Windows 上从分离进程发出的子进程没有控制台，Ctrl+Break 这条路走不通，
// 因此 Spring Boot 的优雅关闭（@PreDestroy、连接池释放）在这里做不到，只能强杀 +
// 事后核对端口是否释放。这是本平台的既有限制，不把它包装成"已优雅停止"。
func KillTree(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command("taskkill", args...)
	cmd.SysProcAttr = hideWindow
	return cmd.Run()
}

var (
	modKernel32         = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess     = modKernel32.NewProc("OpenProcess")
	procCloseHandle     = modKernel32.NewProc("CloseHandle")
	procGetProcessTimes = modKernel32.NewProc("GetProcessTimes")
	procGetExitCode     = modKernel32.NewProc("GetExitCodeProcess")
	procMBToWide        = modKernel32.NewProc("MultiByteToWideChar")
	procWideToMB        = modKernel32.NewProc("WideCharToMultiByte")

	procCreateToolhelp32Snapshot = modKernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First           = modKernel32.NewProc("Process32FirstW")
	procProcess32Next            = modKernel32.NewProc("Process32NextW")
)

const (
	th32CS_SNAPPROCESS = 0x00000002
)

const (
	processQueryLimitedInformation = 0x1000
	processSynchronize             = 0x00100000
	stillActive                    = 259
	errorFileNotFound              = syscall.Errno(2)
	errorAccessDenied              = syscall.Errno(5)
	errorInvalidParameter          = syscall.Errno(87)
	cpACP                          = 0     // 系统 ANSI 代码页：中文 Windows 上就是 GBK
	cpUTF8                         = 65001 // 与 MultiByteToWideChar/WideCharToMultiByte 约定的固定值
)

type fileTime struct {
	low  uint32
	high uint32
}

// procStat 一次打开、一次问完：创建时间 + 是否还在。
//
// 不用 tasklist：它输出的是本地代码页（中文 Windows 是 GBK）文本，而 Go 按 UTF-8
// 比较，"没有匹配的运行任务"永远匹配不上，于是已结束的进程全被读成"还活着"。
// 也不用 WaitForSingleObject：本机实测用 PROCESS_QUERY_LIMITED_INFORMATION 打开的
// 句柄上它恒返回 WAIT_FAILED，活进程和死进程都掉进"未知"分支，等于永远报活。
// GetProcessTimes/GetExitCodeProcess 在同一句柄上实测可用，就用它们。
func procStat(pid int) (created uint64, alive bool, opened bool) {
	if pid <= 0 {
		return 0, false, false
	}
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation|processSynchronize, 0, uintptr(pid))
	if h == 0 {
		if no, ok := err.(syscall.Errno); ok {
			switch no {
			case errorAccessDenied:
				return 0, true, false // 存在但没权限查询，绝不报"没了"
			case errorFileNotFound, errorInvalidParameter:
				return 0, false, false // 明确回答：这个 PID 没有进程对象
			}
		}
		return 0, true, false // 其他错误：答案未知，宁可漏报
	}
	defer procCloseHandle.Call(h)
	var creation, exitK, kUser, user fileTime
	var code uint32
	ret, _, _ := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exitK)),
		uintptr(unsafe.Pointer(&kUser)), uintptr(unsafe.Pointer(&user)))
	if ret != 0 {
		created = uint64(creation.high)<<32 | uint64(creation.low)
	}
	// 退出码查不到时按"还在"处理：误判成已退出会让 runlite 忘掉一个真在跑的进程。
	alive = true
	if r, _, _ := procGetExitCode.Call(h, uintptr(unsafe.Pointer(&code))); r != 0 {
		alive = code == stillActive
	}
	return created, alive, true
}

// PidAlive 判断 PID 是否还在。
func PidAlive(pid int) bool {
	_, alive, _ := procStat(pid)
	return alive
}

// ProcIdent 返回该 PID 当前进程的创建时间（FILETIME：自 1601 年起算的 100ns 计数）。
func ProcIdent(pid int) (uint64, bool) {
	created, _, opened := procStat(pid)
	return created, opened && created != 0
}

// PidSame 回答"这个 PID 现在装的还是不是当初那个进程"。
//
// PID 会被系统回收复用，单看 PID 会认错人：那样 stop 一个孤儿时，可能去结束一个
// 恰好捡到同一个号码的无关进程。创建时间标识的是进程对象本身，才拿来当身份用。
// created 为 0（老记录里没存过）时退化成只看存活，并在调用方的说法里说明这份不确定。
func PidSame(pid int, created uint64) bool {
	if created == 0 {
		return PidAlive(pid)
	}
	now, alive, _ := procStat(pid)
	return alive && now == created
}

func listenersUnix() ([]Listener, error) { return nil, nil }

// decodeBufs 复用中间缓冲区：DecodeConsole 在 GBK 路径上要
// make([]uint16, n) + make([]byte, size) + string() 三次分配。mutex 保护的
// 包级缓冲把前两次摊平，只剩最后的 string() 一次——pump 单 goroutine 顺序调，
// 锁竞争几乎为零。
//
// 关键：不用 sync.Pool（GC 会随时清池，bench 实测仍 5 allocs/op），
// 也不靠 defer 归还（闭包捕获变量导致额外分配）。
var (
	decodeMu   sync.Mutex
	decodeWide []uint16
	decodeUtf8 []byte
)

// DecodeConsole 把不是合法 UTF-8 的文本按系统 ANSI 代码页转成 UTF-8。
//
// 被管进程的输出一律先进管道：JDK 8 只认控制台代码页，mvn.cmd、gradlew.bat 这些
// 包装脚本发的中文提示也照着 ANSI 代码页写，本机实测就是 GBK。给子进程设
// -Dstderr.encoding 只能管住 JDK 9+，压不住其余来源，所以读到的字节还得能还原。
// 已经是合法 UTF-8 的（含纯 ASCII）原样返回，不做二次转换。
func DecodeConsole(s string) string {
	if len(s) == 0 || utf8.ValidString(s) {
		return s
	}
	// 直接用 string 的底层指针传给 syscall，省掉 []byte(s) 的一次拷贝。
	// MultiByteToWideChar 只读输入缓冲区，不改写。
	p := unsafe.Pointer(unsafe.StringData(s))
	n, _, _ := procMBToWide.Call(cpACP, 0, uintptr(p), uintptr(len(s)), 0, 0)
	if n == 0 {
		return s
	}

	decodeMu.Lock()
	if cap(decodeWide) < int(n) {
		decodeWide = make([]uint16, n)
	}
	w := decodeWide[:n]
	if r, _, _ := procMBToWide.Call(cpACP, 0, uintptr(p), uintptr(len(s)),
		uintptr(unsafe.Pointer(&w[0])), n); r == 0 {
		decodeMu.Unlock()
		return s
	}

	// 两遍调用：第一遍拿精确字节数（含终止符），第二遍填缓冲。
	size, _, _ := procWideToMB.Call(cpUTF8, 0, uintptr(unsafe.Pointer(&w[0])), n, 0, 0, 0, 0)
	if size == 0 {
		decodeMu.Unlock()
		return s
	}

	if cap(decodeUtf8) < int(size) {
		decodeUtf8 = make([]byte, size)
	}
	out := decodeUtf8[:size]
	k, _, _ := procWideToMB.Call(cpUTF8, 0, uintptr(unsafe.Pointer(&w[0])), n,
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), 0, 0)
	if k == 0 {
		decodeMu.Unlock()
		return s
	}

	result := string(out[:k])
	decodeMu.Unlock()
	return result
}

// procEntry32 是 PROCESSENTRY32W。字段顺序按本机实测：th32ModuleID 在 cntThreads 之前，
// 整条 568 字节；dwSize 报给内核的大小对不上时 Process32FirstW 直接回 ERROR_BAD_LENGTH
// （照 MSDN 页面那个顺序写会一个进程都枚举不到，本机 C 参考实现核对过）。
type procEntry32 struct {
	size        uint32
	usage       uint32
	pid         uint32
	defaultHeap uintptr
	moduleID    uint32
	threads     uint32
	parentPID   uint32
	priority    int32
	flags       uint32
	exeFile     [260]uint16
}

// TreePIDs 返回 root 及它全部子孙进程的 id（含 root 自己）。
// 找"启动后新出现的监听端口"必须按归属过滤：不看进程树，别人家程序此刻开一个端口
// 就会被记成"这个项目在监听"（本机实测：`mvn package` 被记到 5283，那根本不是它的）。
func TreePIDs(root int) map[int]bool {
	out := map[int]bool{root: true}
	if root <= 0 {
		return out
	}
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32CS_SNAPPROCESS, 0)
	if snap == uintptr(syscall.InvalidHandle) {
		return out
	}
	defer procCloseHandle.Call(snap)
	var pe procEntry32
	pe.size = uint32(unsafe.Sizeof(pe))
	children := map[uint32][]uint32{}
	ok, _, _ := procProcess32First.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for ok != 0 {
		children[pe.parentPID] = append(children[pe.parentPID], pe.pid)
		ok, _, _ = procProcess32Next.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	queue := []uint32{uint32(root)}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ch := range children[cur] {
			if ch == 0 || out[int(ch)] {
				continue
			}
			out[int(ch)] = true
			queue = append(queue, ch)
		}
	}
	return out
}
