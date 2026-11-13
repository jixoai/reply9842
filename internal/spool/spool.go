// Package spool — 磁盘承载的流式响应缓冲（双区域 + 字节预算淘汰）
//
// 正交意图（3 个，2026-11-13 kzf 需求：「默认分两个磁盘区域，默认是临时区域
// 一个是幂等缓存区域。正常请求就是走临时区域……当发现客户端断开，那么就进入到
// 幂等缓存区域，继续接受 serverResponse。等客户端接上来，如果幂等缓存区域有，
// 那么就继续」「不要拷时间了，而是靠磁盘预算」）：
//  1. 双区域：tmp（活跃）/ idem（断线继续收 + 可复用），各自独立字节预算
//  2. 流式落盘：上游 chunk 边收边写，不占内存；客户端从文件按偏移读
//  3. 状态机：Active → Detached → Complete，迁移时 tmp→idem 原子 rename
//
// 关键修正（2026-11-13 实测 bug 修复）：
//
//	迁移必须由「上游结束」也触发，不能只由「客户端断开」触发。原实现只在
//	Detach 里 rename，而正常请求（客户端全程在线）从来不走 Detach，于是完成的
//	响应永远留在 tmp/：既进不了幂等区、也不被 restartIdem 扫到，重启即丢。
//	线上实证：/var/lib/rfc9842-reply/spool/{tmp,idem} 中 idem 为空，三个已完成
//	响应全躺在 tmp/。
//
// 持久化：每个 .dat 旁有同名 .meta sidecar，保存完整 32 字节 key 与响应头。
// 仅靠文件名（key 前 16 字节 hex）恢复不够——恢复出的 key 尾部补零，与真实
// sha256 永远不等，重启后同 key 请求必然 miss（日志里的 written=1588 复用
// 只发生在同一进程生命周期内）。
package spool

import (
	"container/list"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State 条目状态
type State uint8

const (
	StateActive   State = iota + 1 // 客户端挂载中，文件在 tmp/
	StateDetached                  // 客户端已断，上游仍在收，文件在 idem/
	StateComplete                  // 上游完成，文件在 idem/，可复用
)

func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateDetached:
		return "detached"
	case StateComplete:
		return "complete"
	}
	return "unknown"
}

// sidecar 与 .dat 同名的元信息文件（.meta），用于重启后精确恢复。
type sidecar struct {
	Key     string `json:"key"` // 完整 32 字节 hex
	Status  int    `json:"status"`
	Headers []byte `json:"headers,omitempty"`
	Written int64  `json:"written"`
}

// Entry 一次请求的响应缓冲
type Entry struct {
	Key [32]byte

	mgr  *Manager
	path string
	file *os.File

	mu       sync.Mutex
	state    State
	inIdem   bool // 文件是否已在 idem/（避免重复 rename）
	written  int64
	complete bool
	err      error
	notify   chan struct{} // 每次 Append/Finish 时 close 并替换

	headStatus  int
	headHeaders []byte
	headSet     bool
	createdAt   time.Time
	lastAccess  time.Time

	// events SSE 事件边界索引（offset → event id），用于按 event id 续传
	events []EventMark

	// gen 单调递增的「内容版本号」，WaitMore 用它判断是否有新数据
	gen uint64

	// uncacheable 标记「这条记录不该被复用」（上游失败/非 2xx）
	uncacheable bool

	// elem 在 Manager 的 LRU 列表中的位置
	elem *list.Element
}

// EventMark SSE 事件分隔点
type EventMark struct {
	Offset int64
	ID     string
}

// ReadFrom 从 offset 开始读取已落盘的 body 字节，最多 n 字节。
// 返回实际读取的字节数；若 offset 已到末尾返回 (0, nil)。
// 使用 ReadAt 而非 Read，避免与写入端共享文件偏移。
func (e *Entry) ReadFrom(offset int64, n int) ([]byte, error) {
	e.mu.Lock()
	written := e.written
	f := e.file
	e.mu.Unlock()
	if f == nil {
		return nil, errors.New("spool: entry closed")
	}
	if offset >= written {
		return nil, nil
	}
	avail := written - offset
	if int64(n) > avail {
		n = int(avail)
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return buf[:got], err
	}
	return buf[:got], nil
}

// Gen 当前内容版本号（每次 Append / Finish 递增）
func (e *Entry) Gen() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gen
}

// WaitMore 等待「内容版本号相对 observedGen 有变化」或「已完成」，或 ctx 关闭。
//
// 必须传观察到的 gen 而非依赖 channel 快照——否则会出现「Append 发生在
// 调用 WaitMore 之前」导致永久等待的竞态。
//
// ctx 为 nil 时等价于「无限等待」。服务端会传入定时器通道做心跳，以便在
// 客户端静默断开时及时感知（写失败 → Detach）。
func (e *Entry) WaitMore(ctx <-chan struct{}, observedGen uint64) bool {
	for {
		e.mu.Lock()
		if e.complete || e.gen != observedGen {
			e.mu.Unlock()
			return true
		}
		ch := e.notify
		e.mu.Unlock()

		if ctx == nil {
			<-ch
			continue
		}
		select {
		case <-ch:
			continue
		case <-ctx:
			return false
		}
	}
}

// Written 当前已落盘字节数
func (e *Entry) Written() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.written
}

// IsComplete 上游是否已完成
func (e *Entry) IsComplete() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.complete
}

// Err 上游错误（若有）
func (e *Entry) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// State 当前状态
func (e *Entry) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// Head 响应头（status + 扁平化 headers）
func (e *Entry) Head() (int, []byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.headStatus, e.headHeaders, e.headSet
}

// Append 追加一段 body 到文件末尾，并唤醒所有等待者
func (e *Entry) Append(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	e.mu.Lock()
	if e.file == nil {
		e.mu.Unlock()
		return errors.New("spool: entry closed")
	}
	if _, err := e.file.WriteAt(p, e.written); err != nil {
		e.err = err
		e.finishLocked()
		mgr := e.mgr
		e.mu.Unlock()
		if mgr != nil {
			mgr.promote(e, StateComplete)
		}
		return err
	}
	e.written += int64(len(p))
	e.scanEventsLocked(p, e.written-int64(len(p)))
	e.lastAccess = time.Now()
	e.gen++
	close(e.notify)
	e.notify = make(chan struct{})
	e.mu.Unlock()
	return nil
}

// scanEventsLocked 在新增字节里找 SSE 事件边界与 id 行。
//
// SSE 事件块以空行分隔；id 行形如 "id: xxx"。为避免跨 chunk 误判，
// 只在完整的 "\n\n" 之后记一个 mark，并把该块内的 id 行取出。
func (e *Entry) scanEventsLocked(p []byte, base int64) {
	// 简化实现：按行扫描，遇到空行即认为事件结束
	lineStart := 0
	var pendingID string
	for i := 0; i < len(p); i++ {
		if p[i] != '\n' {
			continue
		}
		line := p[lineStart:i]
		lineStart = i + 1
		if len(line) == 0 {
			// 空行 → 事件结束
			if pendingID != "" {
				e.events = append(e.events, EventMark{Offset: base + int64(lineStart), ID: pendingID})
				pendingID = ""
			}
			continue
		}
		if len(line) > 3 && (line[0] == 'i' && line[1] == 'd' && line[2] == ':') {
			v := line[3:]
			for len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			pendingID = string(v)
		}
	}
}

// Finish 标记完成（上游正常结束或出错），并把条目提升到幂等区。
//
// 这是修复「完成响应滞留 tmp/」的核心：客户端一直在线时没有 Detach，
// 若这里不迁移，幂等区永远是空的。
func (e *Entry) Finish(err error) {
	e.mu.Lock()
	if err != nil {
		e.err = err
	}
	already := e.complete
	e.finishLocked()
	mgr := e.mgr
	e.mu.Unlock()
	if !already && mgr != nil {
		mgr.promote(e, StateComplete)
	}
}

func (e *Entry) finishLocked() {
	if e.complete {
		return
	}
	e.complete = true
	e.gen++
	close(e.notify)
	e.notify = make(chan struct{})
}

// MarkUncacheable 标记该条目不可复用。
//
// 场景：上游连接失败（0 字节 + error）或返回 4xx/5xx。这类结果如果留在
// idem 区，同一个 key 之后会永远命中这个失败响应，客户端再也拿不到正确结果。
func (e *Entry) MarkUncacheable() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.uncacheable = true
}

// Uncacheable 是否已标记为不可复用
func (e *Entry) Uncacheable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.uncacheable
}

// SetHead 记录响应头（仅首次生效），并写入 sidecar 供重启后恢复。
func (e *Entry) SetHead(status int, headers []byte) {
	e.mu.Lock()
	if e.headSet {
		e.mu.Unlock()
		return
	}
	e.headStatus = status
	e.headHeaders = headers
	e.headSet = true
	mgr := e.mgr
	e.mu.Unlock()
	if mgr != nil {
		mgr.persistHead(e)
	}
}

// EventsFrom 返回 offset 之后的第一个事件标记（用于验证 event id 续传）
func (e *Entry) EventsFrom(offset int64) []EventMark {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, m := range e.events {
		if m.Offset >= offset {
			return e.events[i:]
		}
	}
	return nil
}

// EventOffset 按 event id 找续传偏移；找不到返回 -1
func (e *Entry) EventOffset(id string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].ID == id {
			return e.events[i].Offset
		}
	}
	return -1
}

// Close 关闭文件句柄
func (e *Entry) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.file != nil {
		err := e.file.Close()
		e.file = nil
		return err
	}
	return nil
}

// ---- Manager ----

// Manager 管理双区域与预算
type Manager struct {
	dir        string
	tmpBudget  int64
	idemBudget int64

	mu      sync.Mutex
	entries map[[32]byte]*Entry
	tmpLRU  *list.List
	idemLRU *list.List

	closed bool
}

// Options 构造参数
type Options struct {
	Dir        string // 根目录，内部建 tmp/ 与 idem/
	TmpBudget  int64  // 活跃区字节上限
	IdemBudget int64  // 幂等区字节上限
}

// New 创建 Manager
func New(opt Options) (*Manager, error) {
	if opt.Dir == "" {
		return nil, errors.New("spool: dir required")
	}
	if opt.TmpBudget <= 0 {
		opt.TmpBudget = 512 << 20
	}
	if opt.IdemBudget <= 0 {
		opt.IdemBudget = 2 << 30
	}
	for _, sub := range []string{"tmp", "idem"} {
		if err := os.MkdirAll(filepath.Join(opt.Dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("spool: mkdir: %w", err)
		}
	}
	m := &Manager{
		dir: opt.Dir, tmpBudget: opt.TmpBudget, idemBudget: opt.IdemBudget,
		entries: make(map[[32]byte]*Entry),
		tmpLRU:  list.New(), idemLRU: list.New(),
	}
	// 恢复幂等区：进程重启后，之前 Detach/Complete 的条目必须可复用。
	m.restoreIdem()
	// tmp/ 是「活跃请求」区，活跃请求不会跨进程存活——上一个进程留下的
	// 文件必然是孤儿（客户端连接早已断、上游 pump 已死）。不清理会永久
	// 占用磁盘且永不被引用。这是设计语义，不是数据丢失。
	m.resetTmp()
	m.mu.Lock()
	m.evictLocked() // 恢复后立刻按预算收敛（旧进程可能留下超额文件）
	m.mu.Unlock()
	return m, nil
}

// resetTmp 清空活跃区（仅启动时调用）。
func (m *Manager) resetTmp() {
	dir := filepath.Join(m.dir, "tmp")
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		os.Remove(filepath.Join(dir, de.Name()))
	}
}

// restoreIdem 扫描 idem/ 目录，按 sidecar 恢复完整 key 与响应头。
//
// 没有 sidecar 的 .dat 是旧版本遗留（只有 key 前 16 字节），既无法还原出
// 正确 key（尾部补零后永远 miss）也无法还原响应头，属于不可用残骸：删除，
// 避免永久占用预算。
func (m *Manager) restoreIdem() {
	dir := filepath.Join(m.dir, "idem")
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if de.IsDir() || filepath.Ext(de.Name()) != ".dat" {
			continue
		}
		path := filepath.Join(dir, de.Name())
		metaPath := strings.TrimSuffix(path, ".dat") + ".meta"

		var sc sidecar
		raw, rerr := os.ReadFile(metaPath)
		if rerr != nil || json.Unmarshal(raw, &sc) != nil {
			os.Remove(path)
			os.Remove(metaPath)
			continue
		}
		rawKey, kerr := hex.DecodeString(sc.Key)
		if kerr != nil || len(rawKey) != 32 {
			os.Remove(path)
			os.Remove(metaPath)
			continue
		}

		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			continue
		}

		var key [32]byte
		copy(key[:], rawKey)

		e := &Entry{
			Key: key, mgr: m, path: path, file: f,
			state: StateComplete, inIdem: true, // 重启后不知上游状态，保守标记为 Complete
			written: st.Size(), complete: true,
			headStatus: sc.Status, headHeaders: sc.Headers, headSet: true,
			notify:    make(chan struct{}),
			createdAt: st.ModTime(), lastAccess: st.ModTime(),
		}
		e.scanEventsLockedFile() // 重建 SSE 事件索引，续传才能按 event id 定位
		m.entries[key] = e
		e.elem = m.idemLRU.PushFront(e)
	}
}

// scanEventsLockedFile 从文件内容重建 SSE 事件索引（仅恢复路径使用）。
func (e *Entry) scanEventsLockedFile() {
	if e.file == nil || e.written <= 0 {
		return
	}
	buf := make([]byte, e.written)
	n, err := e.file.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return
	}
	e.scanEventsLocked(buf[:n], 0)
}

// Acquire 取得（或创建）一个条目。
//
// existing=true 表示该 key 已有条目（可能已 complete，也可能仍在接收），
// 调用方应走「重放 + 挂载」路径；existing=false 表示新建，调用方需启动上游 pump。
func (m *Manager) Acquire(key [32]byte) (*Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, errors.New("spool: manager closed")
	}
	if e, ok := m.entries[key]; ok {
		e.mu.Lock()
		e.lastAccess = time.Now()
		e.mu.Unlock()
		// 提升到 LRU 前端
		if e.elem != nil {
			if e.inIdem {
				m.idemLRU.MoveToFront(e.elem)
			} else {
				m.tmpLRU.MoveToFront(e.elem)
			}
		}
		return e, true, nil
	}

	path := filepath.Join(m.dir, "tmp", fmt.Sprintf("%x.dat", key[:16]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("spool: create: %w", err)
	}
	e := &Entry{
		Key: key, mgr: m, path: path, file: f,
		state: StateActive, notify: make(chan struct{}),
		createdAt: time.Now(), lastAccess: time.Now(),
	}
	m.entries[key] = e
	e.elem = m.tmpLRU.PushFront(e)
	return e, false, nil
}

// Detach 客户端断开：状态转 Detached，并把文件从 tmp/ 移到 idem/。
//
// 上游 pump 不受影响，继续 Append。若上游已结束（Complete），只做迁移，
// 状态保持 Complete（已完成的内容不会因为客户端走开而“降级”）。
func (m *Manager) Detach(e *Entry) error {
	return m.promote(e, StateDetached)
}

// promote 把条目提升到幂等区：tmp/ → idem/ 原子 rename，状态单调递进。
//
// 锁序固定为 m.mu → e.mu（与 Acquire/Stats/evict 一致），Entry 侧方法绝不
// 反过来持 e.mu 再取 m.mu，避免死锁。
func (m *Manager) promote(e *Entry, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	// 条目可能已被淘汰
	if cur, ok := m.entries[e.Key]; !ok || cur != e {
		return nil
	}

	e.mu.Lock()
	cur := e.state
	if cur == StateComplete {
		to = StateComplete // 不允许降级
	}
	already := e.inIdem
	oldPath := e.path
	closed := e.file == nil
	e.mu.Unlock()

	if closed {
		return nil
	}
	if already && cur == to {
		return nil // 已在该状态
	}

	newPath := filepath.Join(m.dir, "idem", filepath.Base(oldPath))
	if already {
		newPath = oldPath
	}

	// 先落 head 的 sidecar（rename 前写，保证与 .dat 同名同目录）
	m.writeSidecar(e)

	if !already {
		// 原子 rename（同文件系统）；失败不致命，条目继续在原位工作
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("spool: rename: %w", err)
		}
		e.mu.Lock()
		e.path = newPath
		e.inIdem = true
		e.mu.Unlock()
		// sidecar 跟着搬家
		os.Rename(strings.TrimSuffix(oldPath, ".dat")+".meta",
			strings.TrimSuffix(newPath, ".dat")+".meta")
	}

	e.mu.Lock()
	e.state = to
	e.mu.Unlock()

	// 移动 LRU 列表
	if e.elem != nil {
		m.tmpLRU.Remove(e.elem)
		e.elem = m.idemLRU.PushFront(e)
	}
	m.evictLocked()
	return nil
}

// persistHead 记录响应头到 sidecar（重启后可继续按原状态码/头部重放）。
func (m *Manager) persistHead(e *Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeSidecar(e)
}

// writeSidecar 写出 .meta。调用方必须持有 m.mu。
func (m *Manager) writeSidecar(e *Entry) {
	e.mu.Lock()
	path := e.path
	sc := sidecar{Key: hex.EncodeToString(e.Key[:]), Status: e.headStatus,
		Headers: e.headHeaders, Written: e.written}
	headSet := e.headSet
	e.mu.Unlock()

	// 没有头就没有可重放的内容，不写 sidecar（避免留下“看似可用”的残骸）
	if !headSet || path == "" {
		return
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return
	}
	metaPath := strings.TrimSuffix(path, ".dat") + ".meta"
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	os.Rename(tmp, metaPath) // 原子替换
}

// Evict 主动释放一个条目（客户端确认收完后调用）
func (m *Manager) Evict(key [32]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return
	}
	m.removeLocked(e)
}

func (m *Manager) removeLocked(e *Entry) {
	e.mu.Lock()
	path := e.path
	f := e.file
	inIdem := e.inIdem
	e.file = nil
	e.mu.Unlock()

	delete(m.entries, e.Key)
	if e.elem != nil {
		if inIdem {
			m.idemLRU.Remove(e.elem)
		} else {
			m.tmpLRU.Remove(e.elem)
		}
		e.elem = nil
	}
	if f != nil {
		f.Close()
	}
	if path != "" {
		os.Remove(path)
		os.Remove(strings.TrimSuffix(path, ".dat") + ".meta")
	}
}

// regionBytesLocked 统计某区域的已落盘字节（调用方持有 m.mu）。
//
// 不用增量计数器：Append 在 Entry 锁内、Detach 在 Manager 锁内，增量记账
// 的竞态会让计数漂移甚至变负（实测 Detach 后 tmpBytes=-1000）。按需汇总
// 的代价是 O(entries)，相对磁盘 IO 可忽略，换来的是不会错的预算。
func (m *Manager) regionBytesLocked(inIdem bool) int64 {
	var sum int64
	for _, e := range m.entries {
		e.mu.Lock()
		if e.inIdem == inIdem {
			sum += e.written
		}
		e.mu.Unlock()
	}
	return sum
}

// evictLocked 按预算淘汰（调用方持有 m.mu）
func (m *Manager) evictLocked() {
	// tmp 区：只淘汰 Detached/Complete（Active 不淘汰）
	for {
		cur := m.regionBytesLocked(false)
		if cur <= m.tmpBudget {
			break
		}
		victim := m.pickVictim(m.tmpLRU)
		if victim == nil {
			break
		}
		before := cur
		m.removeLocked(victim)
		if m.regionBytesLocked(false) >= before {
			break // 防御：淘汰没能释放字节，避免死循环
		}
	}
	// idem 区：LRU 淘汰
	for {
		cur := m.regionBytesLocked(true)
		if cur <= m.idemBudget {
			break
		}
		el := m.idemLRU.Back()
		if el == nil {
			break
		}
		before := cur
		m.removeLocked(el.Value.(*Entry))
		if m.regionBytesLocked(true) >= before {
			break
		}
	}
}

// pickVictim 从 LRU 尾部向前找一个非 Active 条目
func (m *Manager) pickVictim(l *list.List) *Entry {
	for el := l.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*Entry)
		if e.State() != StateActive {
			return e
		}
	}
	return nil
}

// Stats 快照
type Stats struct {
	Entries    int   `json:"entries"`
	TmpBytes   int64 `json:"tmp_bytes"`
	TmpBudget  int64 `json:"tmp_budget"`
	IdemBytes  int64 `json:"idem_bytes"`
	IdemBudget int64 `json:"idem_budget"`
	Active     int   `json:"active"`
	Detached   int   `json:"detached"`
	Complete   int   `json:"complete"`
}

// Stats 返回统计
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Stats{
		Entries: len(m.entries), TmpBudget: m.tmpBudget, IdemBudget: m.idemBudget,
		TmpBytes: m.regionBytesLocked(false), IdemBytes: m.regionBytesLocked(true),
	}
	for _, e := range m.entries {
		switch e.State() {
		case StateActive:
			s.Active++
		case StateDetached:
			s.Detached++
		case StateComplete:
			s.Complete++
		}
	}
	return s
}

// Close 关闭全部条目
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, e := range m.entries {
		e.mu.Lock()
		if e.file != nil {
			e.file.Close()
			e.file = nil
		}
		e.mu.Unlock()
	}
	return nil
}
