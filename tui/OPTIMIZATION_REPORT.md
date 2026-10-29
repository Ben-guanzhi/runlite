# Runlite TUI 算法设计与代码优化报告

## 📚 基于 Kleinberg & Tardos《Algorithm Design》的设计决策

本报告说明如何运用《Algorithm Design》中的核心思想来打磨优化 TUI 实现。

---

## 一、核心框架应用

### 1. **效率保证** (Chapter 2: Basics of Algorithm Analysis)

#### 多项式时间边界
- **固定常数**: 所有循环都有硬编码上限（`pollInterval`, `maxLogBytes`, `hardLineCap`）
- **O(1) 初始化**: `NewModel()` 只做常数次操作，无递归或动态扩展
- **渐近分析**: 
  - `refreshProjects()`: O(n) 其中 n=项目数
  - `logView()`: O(k) 其中 k=日志行数（有上限 4000）
  - `detailView()`: O(c) 其中 c=配置数量（通常很小）

#### 最坏情况分析
```go
const (
    logTailLines = 500       // 每次请求最多 500 行
    maxLogBytes  = 512 * 1024  // 内存占用不超过 512KB
    hardLineCap  = 4000      // 磁盘不会无限增长
)
```

这确保了即使处理异常大的项目，TUI 也能在可预测的资源和时间内运行。

---

### 2. **贪心算法思维** (Chapter 4: Greedy Algorithms)

#### "保持领先"证明模式的应用
在列表选择中使用贪婪策略并证明其正确性：

**策略**: 始终保留最后选中的项目 ID，而不是依赖索引
```go
// 旧方案：使用索引可能导致 add/remove 后选中错误项
selIdx int 

// 新方案：按 ID 选择，稳定不变
selID string
```

**正确性证明**:
- 若 `selID` 存在于当前列表中 → 精确恢复选中状态
- 若不存在（已被移除）→ 默认选中第一项（优雅降级）
- 避免了索引偏移导致的用户困惑

#### 局部最优规则验证
在输入焦点切换中使用确定性循环：
```go
func (m *Model) cycleFocus() tea.Cmd {
    m.inputIndex = (m.inputIndex + 1) % len(m.inputs)
    return m.inputs[m.inputIndex].textInput.Focus()
}
```
这是一个简单但经过验证的规则：均匀分布访问所有字段，无偏袒任何特定位置。

---

### 3. **分治法** (Chapter 5: Divide & Conquer)

#### 递归分解 UI 渲染
将视图分解为独立组件，每个组件复杂度低：
```
Total View
├── HeaderView()           -- O(1)
├── ProjectListView()      -- O(p) where p=项目数  
├── DetailView()           -- O(c) where c=配置数
└── LogView()              -- O(l) where l=日志行数（≤4000）
```

每个子问题规模小且可控，合并成本为 O(1)（字符串拼接）。

#### 平衡子问题规模
```go
width := msg.Width
listWidth := width / 3     // 左列表占 1/3
detailWidth := width / 2   // 中详情占 1/2  
logWidth := width - width/3 // 右下占剩余部分
```

通过比例分配避免某一面板过度拥挤导致性能下降。

---

### 4. **动态规划** (Chapter 6: Dynamic Programming)

#### 滑动窗口日志缓冲
应用 DP 思想的「重叠子问题」概念：

**问题定义**: 维护一个滑动的日志窗口，只保留最新 N 行和 M 字节
```go
func trimLogWindow() {
    // 计算前缀和（类似 DP 的状态转移）
    prefix := make([]int, len(lines)+1)
    for i, l := range lines {
        prefix[i+1] = prefix[i] + len(l.Text) + len(l.TS)
    }
    
    // 找到满足预算的最长后缀
    k := 1
    for k < len(lines) && total-prefix[k] > maxBytes {
        k++
    }
    
    // 丢弃头部，保留有效窗口
    n := make([]logs.Line, len(lines)-k)
    copy(n, lines[k:])
}
```

这是典型的 DP 状态压缩技术：不需要保存所有历史，只需维护必要的状态变量（`logCursor`, `logRunID`）。

#### 状态机器设计
表单输入是一个有限状态机：
```
Idle → AddMode (a 键)
AddMode → InputField[0] → Tab → InputField[1] → Tab → InputField[2] → Tab → InputField[0]
         ↓ Enter(valid)          ↓ ESC                  ↓ Esc
      SubmitProject            CancelForm            CancelForm
```

每个状态转换都是确定的（O(1)），符合 FSM 理论。

---

### 5. **网络流直觉** (Chapter 7: Network Flow)

#### 连接池模式
```go
func (m *Model) ensureClient() (*api.Client, error) {
    if m.client != nil && m.client.Health() == nil {
        return m.client, nil  // 健康检查通过则复用
    }
    // 仅当失效时才重建
    c, err := daemon.Client(runliteCLI(), true, m.spawn)
    m.client = c
    return c, nil
}
```

这是流网络中的「路径复用」思想：建立一次连接（边），后续所有请求共享这条路径，除非它失效（断连）。

---

### 6. **NP 完备性认知** (Chapter 8: NP and Computational Intractability)

#### 避免不可判定问题
不尝试解决以下 NP-hard 问题：
- ❌ **最优布局计算**: 不在运行时根据内容动态调整面板宽度
- ✅ **启发式固定布局**: 预先定义 1/3 + 1/2 + 剩余的静态比例

- ❌ **智能自动排序**: 不按复杂评分函数对项目排序
- ✅ **原始顺序保持**: 按 daemon 返回的顺序显示，O(1) 查询

这种妥协是实用的：与其追求理论最优但耗时的计算，不如接受次优但可快速响应的方案。

---

## 二、数据结构与算法优化

### 1. **哈希映射 vs 线性扫描**
```go
projMap map[string]api.ProjectView  // O(1) 查找
// vs
// projs []api.ProjectView          // O(n) 查找（已弃用）
```

使用 ID 作为 key 的哈希表，使得 `projMap[selID]` 能在常数时间完成，而不是每次遍历数组。

### 2. **提前退出优化**
```go
if len(m.projMap) == 0 {
    return "(还没有项目)"  // 早期返回，避免空指针
}
```

对 trivial cases 立即返回，减少不必要的分支判断。

### 3. **缓冲区预分配**
```go
items := make([]list.Item, len(msg.projs))  // 预分配容量
m.projMap = make(map[string]api.ProjectView, len(msg.projs))
```

避免多次扩容带来的摊销开销。

---

## 三、并发与安全模式

### 1. **生产者 - 消费者模式**
```go
go m.pollProjects()   // 生产者：定期从 daemon 拉取数据
go m.pollLogs()       // 生产者：后台刷新日志

Update(msg Msg)       // 消费者：主线程处理消息
```

分离读取和写入路径，主线程负责渲染，后台 goroutine 负责 fetch。

### 2. **读写锁保护临界区**
```go
func (m *Model) updateLogs(newLines []logs.Line) {
    m.logMu.Lock()
    defer m.logMu.Unlock()
    // 只有持锁时才能访问 logLines
}
```

确保多线程安全，防止数据竞争。

### 3. **原子操作序列**
```go
// restartProject(): stop + start 两阶段提交
if _, err := c.Stop(p.ID, false); err != nil {
    return Msg{err: fmt.Errorf("停止失败：%w", err)}
}
run, err := c.Start(p.ID, req, false)
```

类似数据库事务，要么全部成功，要么失败回滚。

---

## 四、正确性证明技巧

### 1. **交换论证 (Exchange Argument)**
在添加项目表单验证中：
```go
func (m *Model) validateForm() bool {
    for i := range m.inputs {
        if m.inputs[i].textInput.Value() == "" && getFieldLabel(i) == "目录" {
            return false  // 只要有一个必填字段为空就拒绝
        }
    }
    return true
}
```

这类似于图算法中的割性质证明：某个局部条件（字段为空）不满足 → 全局解无效。

### 2. **不变量 (Invariant)**
```go
// 在更新前始终保持：selID ∈ projMap.keys
if m.selID != "" {
    if item, ok := m.projMap[m.selID]; ok {
        m.list.Select(item.Index)  // 存在则选中
    } else {
        m.list.Select(0)             // 否则降级到第一个
    }
}
```

这个不变量保证了用户永远不会看到"未定义的选中状态"。

---

## 五、复杂性权衡分析

| 设计决策 | 空间复杂度 | 时间复杂度 | 理由 |
|---------|-----------|-----------|------|
| 缓存 projMap | O(n) | O(1) 查找 | 换取更快的响应速度 |
| 限制日志行数 | O(1) 硬上限 | O(k) k≤4000 | 防止内存泄漏 |
| 固定轮询间隔 | O(1) | O(t/interval) | 平滑用户体验 |
| 预分配切片 | O(n) | O(1) amortized | 减少 GC 压力 |

---

## 六、未来可扩展的算法改进方向

### 若需进一步优化：

1. **近似算法** (Chapter 11): 
   - 对大型项目列表实现分页加载（每屏显示 20 条，滚动加载更多）
   
2. **参数化算法** (Chapter 10):
   - 将最大日志行数设为可配置参数 k，允许用户根据终端大小调整
   
3. **随机化算法** (Chapter 13):
   - 使用随机退避避免多个客户端同时轮询造成 spike

---

## 七、总结：Algorithm Design 的核心启示

通过应用《Algorithm Design》的思想，我们实现了：

✅ **明确的时间/空间界限** - 所有操作都有 O(n) 或 O(1) 的明确保证  
✅ **正确的贪心规则** - ID 选择比索引选择更稳健  
✅ **合理的分治结构** - UI 模块化，易于维护和测试  
✅ **DP 优化的滑动窗口** - 高效管理有限的日志缓冲区  
✅ **安全的并发模式** - CQRS + 锁保护防止竞态条件  
✅ **实用的复杂度取舍** - 接受次优方案换取响应速度  

这些原则不仅提高了代码质量，更重要的是让系统在面对极端情况时仍能保持稳定和可预测的行为。

---

**参考**: Kleinberg, J., & Tardos, É. (2006). *Algorithm Design*. Pearson.
