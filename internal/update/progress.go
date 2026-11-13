package update

// Stage 是更新流程的阶段。GUI 用它决定显示什么文案与进度条形态：
// 下载与校验有可量化的百分比，检查与替换只有「进行中」。
type Stage string

const (
	StageCheck    Stage = "check"    // 查询 GitHub Release
	StageDownload Stage = "download" // 下载产物（可报告字节数）
	StageVerify   Stage = "verify"   // 校验 sha256 / 可执行性
	StageApply    Stage = "apply"    // 备份 + 原子替换
	StageRestart  Stage = "restart"  // 启动新进程
)

// Progress 是一次进度回调的载荷。
//
// 刻意只有一个结构体而不是多个回调：GUI 的进度条只有一个位置，
// 阶段变化时它就地改文案即可，多回调会让调用方自己去拼状态机。
type Progress struct {
	Stage Stage
	// Mirror 是本次实际使用的镜像标签（hostname 或 "github.com（直连）"）。
	// 仅在 StageDownload 有意义，让用户能看出「为什么这么慢/为什么通了」。
	Mirror string
	// Downloaded / Total 是字节数；Total 为 0 表示服务端没给长度
	// （chunked 传输、或镜像把 Content-Length 去掉了）。
	Downloaded int64
	Total      int64
	// Percent 是 0-100 的整数百分比；Total==0 时为 -1（未知）。
	//
	// 用整数而不是浮点：界面只显示整数，浮点会在 99.6% 与 100% 之间反复抖动。
	Percent int
}

// ProgressFunc 是进度回调。允许为 nil。
//
// 回调在下载的 goroutine 里同步调用，调用方若要做 UI 更新需自行投递到主线程；
// 本包不引入任何 UI 依赖，这是刻意的边界。
type ProgressFunc func(Progress)

// report 安全地投递一次进度（nil 回调时什么都不做）。
func (u *Updater) report(p Progress) {
	if u.opts.Progress == nil {
		return
	}
	u.opts.Progress(p)
}

// percent 计算整数百分比，Total<=0 时返回 -1。
func percent(got, total int64) int {
	if total <= 0 {
		return -1
	}
	p := int(got * 100 / total)
	// 防御：服务端可能给出比实际更小的 Content-Length（极少数代理会截断），
	// 让界面停在 100% 比显示 120% 更合理；真正的错误由大小校验负责。
	if p > 100 {
		p = 100
	}
	if p < 0 {
		p = 0
	}
	return p
}
