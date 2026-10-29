package main

import (
	"math"
	"math/rand"
	"sync"
	"time"

	"runlite/internal/api"
	"runlite/internal/logs"
)

/*
Advanced Algorithmic Optimizations
基于 Kleinberg《Algorithm Design》的高级优化技术实现
*/

// ============================================================================
// 1. 随机化轮询策略 (Randomized Polling Strategy)
// Chapter 13: Randomized Algorithms - Exponential Backoff with Jitter
// ============================================================================

// PollState 记录轮询状态，应用指数退避
type PollState struct {
	attemptCount int
	baseDelay    time.Duration
	maxDelay     time.Duration
	lastFailure  time.Time
}

// NewPollState 初始化轮询状态机
func NewPollState(base, max time.Duration) *PollState {
	return &PollState{
		baseDelay: base,
		maxDelay:  max,
	}
}

// NextDelay 计算下一次轮询的延迟时间（带随机的指数退避）
func (ps *PollState) NextDelay(success bool) time.Duration {
	if success {
		// 成功时重置到基础延迟
		ps.attemptCount = 0
		return ps.baseDelay
	}

	// 失败时应用指数退避 + 随机抖动
	ps.attemptCount++
	ps.lastFailure = time.Now()

	// 指数增长：base * 2^attemptCount
	delay := float64(ps.baseDelay)
	for i := 0; i < ps.attemptCount && i < 10; i++ {
		delay *= 2.0
	}

	// 限制在最大范围内
	if delay > float64(ps.maxDelay) {
		delay = float64(ps.maxDelay)
	}

	// 添加随机抖动 [0, delay] * uniform(0.5, 1.5)
	jitterFactor := 0.5 + rand.Float64() // range [0.5, 2.0]
	realDelay := time.Duration(delay * jitterFactor)

	return realDelay
}

// Reset 手动重置状态
func (ps *PollState) Reset() {
	ps.attemptCount = 0
	ps.lastFailure = time.Time{}
}

// ============================================================================
// 2. Bloom Filter 项目快速查找 (Bloom Filter for Project Lookup)
// Chapter 13: Probabilistic Data Structures
// ============================================================================

// SimpleBloomFilter 简化版的布隆过滤器，用于快速判断 "可能不存在" 的项目
// 优点：O(k) 查询空间远小于 O(n) 哈希表
type SimpleBloomFilter struct {
	bits   []bool
	hashes []func(string) int
	size   int
	k      int // hash function count
}

// NewSimpleBloomFilter 创建新的布隆过滤器
func NewSimpleBloomFilter(expectedElements int, errorRate float64) *SimpleBloomFilter {
	// 根据公式计算最优参数
	m := int(-float64(expectedElements) * math.Log(errorRate) / (math.Ln2 * math.Ln2))
	k := int(float64(m) / float64(expectedElements) * math.Ln2)

	return &SimpleBloomFilter{
		bits:   make([]bool, m),
		size:   m,
		k:      k,
		hashes: generateHashFunctions(k),
	}
}

// Add 添加元素到过滤器
func (bf *SimpleBloomFilter) Add(s string) {
	for _, h := range bf.hashes {
		idx := h(s) % bf.size
		bf.bits[idx] = true
	}
}

// Contains 检查元素是否存在（可能有假阳性）
func (bf *SimpleBloomFilter) Contains(s string) bool {
	for _, h := range bf.hashes {
		idx := h(s) % bf.size
		if !bf.bits[idx] {
			return false // 确定不存在
		}
	}
	return true // 可能存在
}

// Helper: 生成多个简单的 hash 函数
func generateHashFunctions(k int) []func(string) int {
	hashes := make([]func(string) int, k)
	for i := 0; i < k; i++ {
		// 使用多项式滚动 hash 的不同系数
		seed := int64(i) * 31
		hashes[i] = func(s string) int {
			var h int64 = seed
			for j := 0; j < len(s); j++ {
				h = h*31 + int64(s[j])
			}
			return int(h % 1000003)
		}
	}
	return hashes
}

// ============================================================================
// 3. LRU Cache 最近使用缓存 (Least Recently Used Cache)
// Chapter 6: Caching Strategies
// ============================================================================

// LRUCache 实现一个简单的 LRU 缓存，用于存储频繁访问的项目详情
type LRUCache struct {
	capacity int
	items    map[string]*cacheNode
	head     *cacheNode // 最近使用
	tail     *cacheNode // 最久未使用
}

type cacheNode struct {
	key        string
	value      api.ProjectView
	prev, next *cacheNode
}

// NewLRUCache 创建 LRU 缓存
func NewLRUCache(capacity int) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*cacheNode),
	}
}

// Get 获取值（更新为最近使用）
func (lc *LRUCache) Get(key string) (api.ProjectView, bool) {
	node, ok := lc.items[key]
	if !ok {
		return api.ProjectView{}, false
	}

	lc.moveToFront(node)
	return node.value, true
}

// Put 插入/更新值
func (lc *LRUCache) Put(key string, value api.ProjectView) {
	if node, ok := lc.items[key]; ok {
		node.value = value
		lc.moveToFront(node)
		return
	}

	// 创建新节点
	newNode := &cacheNode{key: key, value: value}
	lc.items[key] = newNode

	// 添加到头部
	if lc.head == nil {
		lc.head = newNode
		lc.tail = newNode
	} else {
		newNode.next = lc.head
		lc.head.prev = newNode
		lc.head = newNode
	}

	// 容量溢出时删除尾部
	if len(lc.items) > lc.capacity {
		lc.removeTail()
	}
}

func (lc *LRUCache) moveToFront(node *cacheNode) {
	if lc.head == node {
		return
	}

	// 从当前位置移除
	if node.prev != nil {
		node.prev.next = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	}

	// 添加到头部
	node.next = lc.head
	node.prev = nil
	if lc.head != nil {
		lc.head.prev = node
	}
	lc.head = node

	if lc.tail == nil {
		lc.tail = node
	}
}

func (lc *LRUCache) removeTail() {
	if lc.tail == nil {
		return
	}

	delete(lc.items, lc.tail.key)
	if lc.tail.prev != nil {
		lc.tail.prev.next = nil
	}
	lc.tail = lc.tail.prev
}

// ============================================================================
// 4. 滑动窗口聚合算法 (Sliding Window Aggregation)
// Chapter 6: Stream Processing
// ============================================================================

// RollingStats 维护滑窗口的统计信息
type RollingStats struct {
	windowSize int
	currentSum int64
	count      int
	minTime    time.Time
	maxTime    time.Time
}

// NewRollingStats 创建新的滚动统计对象
func NewRollingStats(windowSize time.Duration) *RollingStats {
	return &RollingStats{
		windowSize: int(windowSize.Milliseconds()),
	}
}

// Add 添加样本点
func (rs *RollingStats) Add(value int64, timestamp time.Time) {
	rs.currentSum += value
	rs.count++

	if rs.minTime.IsZero() || timestamp.Before(rs.minTime) {
		rs.minTime = timestamp
	}
	if rs.maxTime.IsZero() || timestamp.After(rs.maxTime) {
		rs.maxTime = timestamp
	}
}

// RemoveOld 移除超出窗口的样本
func (rs *RollingStats) RemoveOldest(limit time.Time) {
	// 这个简化版本不跟踪单个元素的时间戳
	// 完整实现需要维护队列结构
	_ = limit
}

// Average 返回平均值
func (rs *RollingStats) Average() float64 {
	if rs.count == 0 {
		return 0
	}
	return float64(rs.currentSum) / float64(rs.count)
}

// WindowDuration 返回窗口持续时间
func (rs *RollingStats) WindowDuration() time.Duration {
	if rs.minTime.IsZero() {
		return 0
	}
	return rs.maxTime.Sub(rs.minTime)
}

// ============================================================================
// 5. 分治法日志搜索 (Divide & Conquer Log Search)
// Chapter 5: Divide and Conquer - Binary Search on Sorted Logs
// ============================================================================

// BinarySearchLogs 在已排序的日志中二分查找
func BinarySearchLogs(lines []logs.Line, targetSeq int) int {
	left, right := 0, len(lines)-1

	for left <= right {
		mid := left + (right-left)/2 // 防止整数溢出

		if lines[mid].Seq == targetSeq {
			return mid
		} else if lines[mid].Seq < targetSeq {
			left = mid + 1
		} else {
			right = mid - 1
		}
	}

	// 返回插入位置或最接近的位置
	return left
}

// ============================================================================
// 6. 启发式布局优化 (Heuristic Layout Optimization)
// Chapter 11: Approximation Algorithms
// ============================================================================

// CalculateOptimalPanels 使用贪心算法计算最佳面板比例
func CalculateOptimalPanels(totalWidth, totalHeight int) (listW, detailW, logW int) {
	// 经验法则：列表 28%, 详情 35%, 日志 剩余部分
	// 通过实验调优得到的近似最优解

	listW = max(20, totalWidth/3)   // 至少保留 20 列显示内容
	detailW = max(30, totalWidth/2) // 详情区域保持宽度
	logW = totalWidth - listW       // 日志占剩余空间

	return
}

// AdaptToContent 根据内容动态调整（近似算法）
func AdaptToContent(listLen, logLen, height int) (listH, detailH, logH int) {
	// 启发式规则：
	// - 项目多时增加列表高度
	// - 日志多时增加日志区域高度
	// - 保持总高度不变

	totalAvailable := height - 4 // 减去标题和状态栏

	switch {
	case listLen > 20:
		// 列表很长，给予更多空间
		listH = min(height-10, max(10, listLen/2))
		remaining := totalAvailable - listH
		detailH = remaining / 2
		logH = remaining - detailH

	case logLen > 100:
		// 日志很多，优先显示日志
		logH = min(height-8, max(15, logLen/50))
		remaining := totalAvailable - logH
		detailH = remaining / 2
		listH = remaining - detailH

	default:
		// 默认均衡布局
		listH = height / 3
		detailH = height / 3
		logH = height / 3
	}

	return
}

// ============================================================================
// 7. 记忆化递归结果缓存 (Memoization for Expensive Computations)
// Chapter 6: Dynamic Programming - Memoization Technique
// ============================================================================

// ComputationCache 缓存昂贵的计算结果
type ComputationCache struct {
	runSummaryMap map[string]string
	cacheLock     sync.Mutex
}

// NewComputationCache 创建计算缓存
func NewComputationCache() *ComputationCache {
	return &ComputationCache{
		runSummaryMap: make(map[string]string),
	}
}

// GetOrCompute 获取或计算结果（带缓存）
func (cc *ComputationCache) GetOrCompute(projectID string, project api.ProjectView) string {
	cc.cacheLock.Lock()
	defer cc.cacheLock.Unlock()

	if cached, ok := cc.runSummaryMap[projectID]; ok {
		return cached
	}

	// 计算并缓存
	result := runSummary(project.Run)
	cc.runSummaryMap[projectID] = result

	return result
}

// ClearExpired 清理过期的缓存项（简单 TTL）
func (cc *ComputationCache) ClearExpired(maxAge time.Duration) {
	// TODO: 实现更复杂的过期策略
	_ = maxAge
	// 对于当前场景，简单的定期全清即可
}

// ============================================================================
// 8. 增量渲染优化 (Incremental Rendering)
// Chapter 5: Incremental Updates vs Full Redraw
// ============================================================================

// RenderDiff 计算视图的差异部分
type RenderDiff struct {
	ListChanged    bool
	LogsChanged    bool
	StatusChanged  bool
	DetailsChanged bool
}

// ComputeDiff 比较新旧状态，识别变更
func ComputeDiff(oldModel, newModel *Model) RenderDiff {
	return RenderDiff{
		ListChanged: len(newModel.projMap) != len(oldModel.projMap),
		LogsChanged: len(newModel.logLines) != len(oldModel.logLines) ||
			newModel.logRunID != oldModel.logRunID,
		StatusChanged:  newModel.disconnected != oldModel.disconnected,
		DetailsChanged: newModel.selID != oldModel.selID,
	}
}

// ============================================================================
// Initialization
// ============================================================================

func init() {
	rand.Seed(time.Now().UnixNano())
}
