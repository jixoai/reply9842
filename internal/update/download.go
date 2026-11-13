// 下载 + 校验。
//
// 与 npm/lib/run.js 的契约逐条等价（除了**有意**更严的 sha256 强制校验，见 doc.go）：
//
//	镜像回退   按列表顺序逐个尝试，任一成功即用
//	拼接       <prefix><完整 url>，前缀由 mirror.go 规范化
//	断点续传   `.part` + `Range: bytes=<n>-`，206 追加、200 从头写
//	原子落盘   先写 `.part`，全部校验通过后 rename 到目标名
//	大小校验   最终字节数必须等于 Content-Length / Content-Range 的 total
//
// npm 侧目前**没有**断点续传（它一次把 body 收进内存再写 .part）。Go 侧加上
// 续传是因为 GUI 的下载在用户点「立即更新」后可能被中途关窗/断网打断，
// 8-17MB 在国内链路上重下一次可能要一分钟。续传不改变任何可观测契约：
// 最终产物相同、校验相同，只是失败后能接着走。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrChecksum 表示 sha256 与 SHA256SUMS.txt 不符。
//
// 单独定义是为了让调用方能给出「下载的文件损坏，已丢弃」这种准确文案，
// 而不是笼统的「更新失败」——两者的用户动作完全不同（前者重试或换镜像）。
var ErrChecksum = errors.New("update: 下载文件 sha256 校验失败")

// ErrSizeMismatch 表示收到的字节数与声明不符（典型原因：传输被截断）。
//
// 历史上这个项目就因为「scp 被 timeout 杀掉、只传了 2.5MB/9.9MB」而部署了一个
// 半截的 ELF，运行时报 SEGV（见 scripts/deploy.sh 顶部）。所以这不是理论风险。
var ErrSizeMismatch = errors.New("update: 下载大小与声明不符")

// 进度回调的节流粒度。
const (
	progressBytesStep = 256 << 10 // 每 256KB
	progressTimeStep  = 100 * time.Millisecond
)

// Download 下载指定版本的产物并校验。
//
// 返回的路径是**已完成 sha256 校验**的文件，可直接交给 Apply。文件放在
// 缓存目录（按版本隔离），不覆盖任何正在使用的东西——安全边界的划分是
// 「Download 只做取件，Apply 才动现有二进制」。
//
// progressFn 非 nil 时覆盖 Options.Progress（便于调用方为不同按钮/窗口传不同的回调）。
func (u *Updater) Download(ctx context.Context, info Info, progressFn ProgressFunc) (string, error) {
	if u.mirrorErr != nil {
		return "", u.mirrorErr
	}
	artifact := info.Artifact
	if artifact == "" {
		var err error
		artifact, err = CurrentArtifact(u.opts.Kind)
		if err != nil {
			return "", err
		}
	}
	tag := normalizeTag(info.Latest)
	if tag == "" {
		return "", fmt.Errorf("update: Info.Latest 为空，无法确定下载版本")
	}

	report := progressFn
	if report == nil {
		report = u.opts.Progress
	}
	emit := func(p Progress) {
		if report != nil {
			report(p)
		}
	}

	dir, err := u.cacheDir()
	if err != nil {
		return "", err
	}
	// 按版本分目录：换版本不串包，回滚时旧版本的文件还在（与 npm 侧同构）。
	dir = filepath.Join(dir, strings.TrimPrefix(tag, "v"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("update: 创建缓存目录: %w", err)
	}
	dest := filepath.Join(dir, artifact)
	part := dest + ".part"

	// 已有完整且校验通过的文件 → 直接复用（用户重复点「更新」零网络开销）。
	// 校验放在这里而不是只信文件名，是因为 .part/rename 的中断可能留下
	// 一个大小正确但内容损坏的文件（磁盘满、镜像返回了另一个版本）。
	if st, statErr := os.Stat(dest); statErr == nil && st.Size() > 0 {
		// 复用已有文件时不知道该文件来自哪个镜像，传空串：等价于"全部镜像
		// 按原顺序"，与没有偏好时的行为一致。
		if err := u.verifyChecksum(ctx, dest, artifact, tag, "", emit); err == nil {
			return dest, nil
		}
		// 校验不过就当它不存在：删掉，重新下载。
		_ = os.Remove(dest)
	}
	// 旧版本的 .part 可能属于「上一次运行、镜像返回了别的内容」。这里不删它
	// ——续传的语义就是把已有字节当同一次下载的前半段。若真不对，会走
	// errRefetch（416 / 206 起点不符）或 sha256 失败，两条路都会删掉它。

	rawURL := u.downloadURLFor(tag, artifact)
	var lastErr error
	for _, mirror := range u.mirrors {
		label := MirrorLabel(mirror)
		emit(Progress{Stage: StageDownload, Mirror: label, Total: info.ArtifactSize, Percent: -1})

		// 每个镜像有一次「免罚重来」的机会：.part 与服务端对不上（416 长度不符、
		// 206 起点不符）时，这个 .part 对本镜像不可用，删掉重发一次无 Range 请求。
		//
		// 为什么必须这样做而不是直接报错：坏 .part 是**持久**的，每次重试都读到
		// 同一份坏字节、每次都在同一个分支失败，用户会永远卡在这里且无从下手
		// （他并不知道要去缓存目录手工删一个隐藏文件）。
		refetched := false

		for {
			err := u.fetchToFile(ctx, mirror, rawURL, part, info.ArtifactSize, emit)
			if errors.Is(err, errRefetch) {
				if refetched {
					// 重来一次还不行 → 说明不是 .part 的锅，换下一个镜像。
					lastErr = err
					break
				}
				refetched = true
				// 删掉 .part 后 have=0，下一次请求自然不带 Range。
				_ = os.Remove(part)
				continue
			}
			if err != nil {
				// ctx 取消/超时：保留 .part 供下次续传，但立刻停止尝试其余镜像
				// （用户主动取消，不该继续消耗流量）。
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return "", err
				}
				lastErr = err
				break
			}

			// 字节到齐了，接着核 sha256。
			emit(Progress{Stage: StageVerify, Mirror: label, Percent: -1})
			if verr := u.verifyChecksum(ctx, part, artifact, tag, mirror, emit); verr != nil {
				// 校验失败 ⇒ 这份字节不可信。**先删 .part**：留着会让下次续传
				// 把损坏的前缀当真，越续越偏；然后换下一个镜像重试，而不是整体失败。
				//
				// 为什么换镜像能救：内容错误的常见来源就是这个镜像本身
				// （被劫持、返回 HTML 错误页、缓存了别的版本），而另一个镜像
				// 的字节是好的。若这是最后一个镜像，循环结束后会带着
				// ErrChecksum 报错，语义仍然是「校验失败」。
				_ = os.Remove(part)
				lastErr = verr
				break
			}
			if err := os.Rename(part, dest); err != nil {
				return "", fmt.Errorf("update: 落盘 %s: %w", dest, err)
			}
			if err := chmodExecutable(dest); err != nil {
				return "", err
			}
			return dest, nil
		}
	}

	if lastErr == nil {
		lastErr = errors.New("update: 镜像列表为空")
	}
	// 保留可分支的哨兵：sha256 失败与传输失败对调用方是不同的提示。
	if errors.Is(lastErr, ErrChecksum) {
		return "", lastErr
	}
	return "", fmt.Errorf("update: 全部镜像下载失败：%w", lastErr)
}

// errRefetch 是内部哨兵：「.part 与服务端不一致，删掉它重新请求」。
//
// 不导出：调用方不需要知道这个中间状态，它只是一次内部重试。
var errRefetch = errors.New("update: 续传基线不可用，需重新请求")

// fetchToFile 用单个镜像把 url 下载到 path，支持续传。
//
// 关键分支（每一条都有实测依据）：
//
//	206  服务端接受 Range → 校验起点与已有字节数一致后追加
//	200  服务端忽略 Range（很多 CDN 与对象存储的默认行为）→ 截断重写
//	416  请求的起点已到/超过文件末尾 → 已有字节等于总长则视为完成，否则 errRefetch
//
// 大小校验：206 的 total 来自 Content-Range，200 的 total 来自 Content-Length。
// 两者都与 API 声明的 AssetSize 交叉核对。任何不一致都返回 ErrSizeMismatch——
// 静默接受一个短文件正是那个 SEGV 事故的形态。
func (u *Updater) fetchToFile(
	ctx context.Context,
	mirror, rawURL, path string,
	expected int64,
	emit func(Progress),
) error {
	have := int64(0)
	if st, err := os.Stat(path); err == nil {
		have = st.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mirror+rawURL, nil)
	if err != nil {
		return fmt.Errorf("update: 构造下载请求: %w", err)
	}
	req.Header.Set("User-Agent", "reply9842-update")
	if have > 0 {
		// 续传请求。If-Range 不用：镜像一般不返回 ETag/Last-Modified 的可信值，
		// 而最终 sha256 已经能兜住「拼了两份不同内容」的情况。
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}

	resp, err := u.downloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("update: 请求 %s: %w", MirrorLabel(mirror), err)
	}
	defer resp.Body.Close()

	var (
		start     int64
		total     int64 = -1
		truncated bool
	)

	switch resp.StatusCode {
	case http.StatusPartialContent:
		rng := resp.Header.Get("Content-Range")
		rStart, rTotal, perr := parseContentRange(rng)
		if perr != nil {
			return fmt.Errorf("update: %s 返回 206 但 Content-Range 不可解析（%q）: %w", MirrorLabel(mirror), rng, perr)
		}
		// 起点必须等于我们已有的字节数：不等说明服务端按别的方式算了区间
		// （或镜像改写了请求、或 .part 是别的版本的残骸）。继续追加会拼出
		// **内容错误**的文件——比失败更糟：只有 sha256 能发现它，而如果要
		// 校验的文件本身就是被换掉的，问题会一直潜伏。所以宁可重下。
		if rStart != have {
			return fmt.Errorf("%w：%s 的续传起点不符（本地 %d，服务端给 %d）",
				errRefetch, MirrorLabel(mirror), have, rStart)
		}
		start, total = rStart, rTotal

	case http.StatusOK:
		// 服务端不支持 Range（或镜像把请求改成了全量）。从头写。
		if have > 0 {
			truncated = true
		}
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			n, perr := strconv.ParseInt(cl, 10, 64)
			if perr != nil || n < 0 {
				return fmt.Errorf("update: %s 的 Content-Length 不可解析（%q）", MirrorLabel(mirror), cl)
			}
			total = n
		}

	case http.StatusRequestedRangeNotSatisfiable:
		// 416 = 请求的起点已到/超过文件末尾。Content-Range 形如 "bytes */1234"。
		//
		// 两种可能，必须分开：
		//   rTotal == have → 上次已经下完，只差 sha 校验（正常路径）
		//   其它           → .part 是坏的（超长残骸/别的版本的残留）→ errRefetch
		//
		// 注意：416 的 body 是空的，**绝不能**当数据流读——把空 body 读完会得出
		// 「实际 0 字节，服务端声明 8341378 字节」这种离真因很远的错误。
		_, rTotal, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr == nil && rTotal >= 0 && rTotal == have {
			// 已经下完了（上次 sha 校验前被打断）。让后续 sha 校验决定去留。
			emit(Progress{Stage: StageDownload, Mirror: MirrorLabel(mirror), Downloaded: have, Total: rTotal, Percent: 100})
			return nil
		}
		return fmt.Errorf("%w：%s 拒绝续传（416），本地 %d 字节，服务端总长 %s",
			errRefetch, MirrorLabel(mirror), have, formatTotal(rTotal, perr))

	default:
		return fmt.Errorf("update: %s 返回 HTTP %s", MirrorLabel(mirror), resp.Status)
	}

	// 用 API 声明的产物大小做第三方交叉核对：镜像报的 total 与它不一致时，
	// 说明字节流不是我们要的资产（极少数镜像会返回自己的错误页 HTML 且 200）。
	if expected > 0 && total > 0 && total != expected {
		return fmt.Errorf("%w：镜像声明 %d 字节，release 声明 %d 字节（%s）",
			ErrSizeMismatch, total, expected, MirrorLabel(mirror))
	}
	// 有总长时，把 expected 统一成可信的那个值，供末尾校验使用。
	if total < 0 && expected > 0 {
		total = expected
	}

	flags := os.O_CREATE | os.O_WRONLY
	if truncated {
		flags |= os.O_TRUNC
		start = 0
	} else {
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return fmt.Errorf("update: 打开临时文件 %s: %w", path, err)
	}
	// 显式 Close（而不是只 defer）：下面的 fsync 必须在关闭前做，且 Close
	// 的错误（延迟写入失败）必须能被看见。
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	label := MirrorLabel(mirror)
	written := start
	lastPct := -1
	lastReport := time.Time{}
	buf := make([]byte, 64<<10)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fmt.Errorf("update: 写入 %s: %w", path, werr)
			}
			written += int64(n)
			pct := percent(written, total)
			if pct != lastPct || time.Since(lastReport) >= progressTimeStep {
				emit(Progress{
					Stage: StageDownload, Mirror: label,
					Downloaded: written, Total: max64(total, 0), Percent: pct,
				})
				lastPct, lastReport = pct, time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// 保留已写入的字节：下次从 written 处续传。返回的 error 里带上
			// 「已收 N / 共 M」，便于界面显示「下到 63% 断了」而非「失败」。
			return fmt.Errorf("%w：已收 %d 字节%s: %v",
				ErrSizeMismatch, written, totalSuffix(total), readErr)
		}
		if ctx.Err() != nil {
			// ctx 已取消但 body 还在吐字节：主动停止（Read 未必及时返回错误）。
			return ctx.Err()
		}
	}

	// 长度必须与声明一致。total < 0 时（既无 Content-Length 也非 206、
	// 且 API 没给 AssetSize）无法校验，只能放行——但 sha256 仍会兜底。
	if total >= 0 && written != total {
		return fmt.Errorf("%w：收到 %d 字节，声明 %d 字节%s",
			ErrSizeMismatch, written, total, progressHint(written, total))
	}
	if expected > 0 && written != expected {
		return fmt.Errorf("%w：收到 %d 字节，release 声明 %d 字节",
			ErrSizeMismatch, written, expected)
	}

	emit(Progress{Stage: StageDownload, Mirror: label, Downloaded: written, Total: max64(total, written), Percent: 100})

	// fsync 再 rename：否则断电时可能 rename 成功而数据块没落盘，得到一个
	// 「存在但内容截断」的目标文件——正是最难排查的形态。
	if err := f.Sync(); err != nil {
		return fmt.Errorf("update: fsync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		closed = true
		return fmt.Errorf("update: 关闭 %s: %w", path, err)
	}
	closed = true
	return nil
}

// parseContentRange 解析 `bytes <start>-<end>/<total>` 或 `bytes */<total>`。
//
// total 为 "*" 时返回 -1（服务端不知道总长）；这在末尾校验时会被当作
// 「无法校验」而非「0 字节」——混淆这两者会造成误报失败。
func parseContentRange(v string) (start, total int64, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, -1, errors.New("Content-Range 缺失")
	}
	if !strings.HasPrefix(v, "bytes ") {
		return 0, -1, fmt.Errorf("Content-Range 单位不是 bytes: %q", v)
	}
	spec := strings.TrimSpace(strings.TrimPrefix(v, "bytes "))
	totalPart := ""
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		totalPart = spec[i+1:]
		spec = spec[:i]
	}
	if totalPart == "" {
		return 0, -1, fmt.Errorf("Content-Range 缺少总长: %q", v)
	}
	if totalPart == "*" {
		total = -1
	} else {
		n, perr := strconv.ParseInt(totalPart, 10, 64)
		if perr != nil || n < 0 {
			return 0, -1, fmt.Errorf("Content-Range 总长不可解析: %q", v)
		}
		total = n
	}
	if spec == "*" {
		return -1, total, nil
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, -1, fmt.Errorf("Content-Range 区间不可解析: %q", v)
	}
	s, perr := strconv.ParseInt(spec[:dash], 10, 64)
	if perr != nil || s < 0 {
		return 0, -1, fmt.Errorf("Content-Range 起点不可解析: %q", v)
	}
	return s, total, nil
}

// verifyChecksum 下载 SHA256SUMS.txt 并比对 path 的 sha256。
//
// 这是**强制的**（与 npm 侧的 best-effort 有意的差异，理由见 doc.go）：
// 自我替换失败没有退路，一个半截的二进制会让应用再也起不来。
//
// servedBy 是刚提供这些字节的镜像，它给出的校验和排在**第一个**试，其余镜像
// 依次兜底。这个顺序有两个作用：
//
//  1. 正常情况（镜像正确）：第一次就比较成功，零额外请求
//  2. 镜像返回错误内容时：它给出的校验和通常也错，但我们不会因此判死——
//     继续用其它镜像的校验和试。只要**任意一份**校验和与本地字节相符，
//     就说明字节是对的
//
// 为什么不能"选一个信源"就完事（本包实测的死结）：若固定从 mirrors[0] 取
// 校验和、而 mirrors[0] 恰好是坏的那个，好镜像的字节会被它的坏校验和判死；
// 更糟的是「校验失败→换镜像重试」时又拿同一份坏校验和再判一次，回退链彻底
// 失效（测试 TestDownloadChecksumCrossCheckedAcrossMirrors 锁住这个场景）。
//
// 若所有镜像的校验和都与本地字节不符，那就是字节真的坏了（传输截断 /
// 缓存污染），报 ErrChecksum 是正确结论。
//
// 校验和不走续传——它只有几百字节，续传的复杂度不值得。
func (u *Updater) verifyChecksum(ctx context.Context, path, artifact, tag, servedBy string, emit func(Progress)) error {
	// 先把本地字节算出来：无论试几份校验和，这个值都不变。
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}

	var lastErr error
	tried := 0
	for _, mirror := range checksumMirrorOrder(u.mirrors, servedBy) {
		sums, ferr := u.fetchChecksums(ctx, tag, mirror)
		if ferr != nil {
			lastErr = ferr
			continue
		}
		want, ok := sums[artifact]
		if !ok {
			lastErr = fmt.Errorf("%w：%s（来自 %s）里没有 %s 的记录",
				ErrChecksum, ChecksumsAsset, MirrorLabel(mirror), artifact)
			continue
		}
		tried++
		if strings.EqualFold(got, want) {
			if emit != nil {
				emit(Progress{Stage: StageVerify, Percent: 100})
			}
			return nil
		}
		lastErr = fmt.Errorf("%w：期望 %s，实际 %s（校验和来自 %s）",
			ErrChecksum, want, got, MirrorLabel(mirror))
	}

	if lastErr == nil {
		if tried == 0 {
			lastErr = fmt.Errorf("%w：所有镜像都取不到 %s 的可用记录", ErrChecksum, ChecksumsAsset)
		} else {
			lastErr = ErrChecksum
		}
	}
	// 保证调用方总能 errors.Is(err, ErrChecksum)：上面各分支都已包装它，
	// 这里再兜一层防止将来新增分支忘记包装。
	if !errors.Is(lastErr, ErrChecksum) {
		return fmt.Errorf("%w: %w", ErrChecksum, lastErr)
	}
	return lastErr
}

// checksumMirrorOrder 把「提供字节的镜像」排到最前（先信它），其余保持原顺序。
//
// servedBy 为空（复用缓存文件时不知道来源）就原样返回。
func checksumMirrorOrder(mirrors []string, servedBy string) []string {
	if servedBy == "" {
		return append([]string(nil), mirrors...)
	}
	out := make([]string, 0, len(mirrors)+1)
	out = append(out, servedBy)
	for _, m := range mirrors {
		if m != servedBy {
			out = append(out, m)
		}
	}
	return out
}

// fetchChecksums 从**单个**镜像取 SHA256SUMS.txt 并解析成 name → hex 的映射。
//
// 只取一个镜像而不内部回退：回退由 verifyChecksum 控制（它需要知道每份校验和
// 的来源，以便在比对失败后继续试下一个）。
func (u *Updater) fetchChecksums(ctx context.Context, tag, mirror string) (map[string]string, error) {
	url := u.downloadURLFor(tag, ChecksumsAsset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mirror+url, nil)
	if err != nil {
		return nil, fmt.Errorf("update: 构造校验和请求: %w", err)
	}
	req.Header.Set("User-Agent", "reply9842-update")
	resp, err := u.downloadClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: 获取 %s（%s）: %w", ChecksumsAsset, MirrorLabel(mirror), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: %s 返回 HTTP %s（%s）", ChecksumsAsset, resp.Status, MirrorLabel(mirror))
	}
	// 1MB 上限：校验和文件只有几百字节，超出的必定不是它。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("update: 读取 %s（%s）: %w", ChecksumsAsset, MirrorLabel(mirror), err)
	}
	sums := ParseChecksums(string(body))
	if len(sums) == 0 {
		return nil, fmt.Errorf("update: %s 返回的 %s 为空或格式不识别", MirrorLabel(mirror), ChecksumsAsset)
	}
	return sums, nil
}

// ParseChecksums 解析 sha256sum / shasum 输出的文本。
//
// 两种格式都要吃下：
//
//	<hex>  <name>   GNU sha256sum（文本模式）
//	<hex> *<name>   GNU sha256sum（二进制模式，Windows 常见）
//
// 忽略空行与无法识别的行：校验和文件里出现注释/额外内容不该让整次更新失败，
// 但**目标产物**找不到时必须失败（由 verifyChecksum 负责）。
func ParseChecksums(text string) map[string]string {
	sums := make(map[string]string, 16)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 前 64 个字符是 hex；之后是若干空白 + 可选 '*' + 文件名。
		if len(line) < 64+2 {
			continue
		}
		hexPart := line[:64]
		if !isHex(hexPart) {
			continue
		}
		name := strings.TrimSpace(line[64:])
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		sums[name] = strings.ToLower(hexPart)
	}
	return sums
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// fileSHA256 流式计算文件摘要（不整个读进内存：产物可能有 17MB+）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("update: 打开待校验文件: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("update: 计算 sha256: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// chmodExecutable 给下载产物加执行位。
//
// Windows 上 os.Chmod 只切只读位，执行位无意义——忽略其错误（返回 ErrInvalid
// 之类的平台上不该因此中断整次更新）。
func chmodExecutable(path string) error {
	if err := os.Chmod(path, 0o755); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not supported") {
			return nil
		}
		return fmt.Errorf("update: 设置执行权限 %s: %w", path, err)
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func totalSuffix(total int64) string {
	if total <= 0 {
		return ""
	}
	return " / 共 " + strconv.FormatInt(total, 10) + " 字节"
}

func progressHint(got, total int64) string {
	if total <= 0 {
		return ""
	}
	return "（" + strconv.Itoa(percent(got, total)) + "%）"
}

// formatTotal 把 416 响应里的总长渲染成可读文本（解析失败时说明原因）。
func formatTotal(total int64, perr error) string {
	if perr != nil {
		return "不可解析（" + perr.Error() + "）"
	}
	if total < 0 {
		return "未知"
	}
	return strconv.FormatInt(total, 10)
}
