// settings.go — 用户偏好的落盘（~/.reply9842/settings.json）
//
// 正交意图（2 个）：
//  1. 两个「时间跨度」偏好各自可调：日志保留时长（磁盘）与图表显示区间（横轴）
//  2. 两者都是**跨重启生效**的持久配置，不能用 flag 当唯一来源
//
// 为什么单独一份文件而不是塞进 client-stats.json：后者是「观测结果的快照」，
// 会被统计逻辑反复覆盖写；把用户偏好混进去，一次统计重建就可能把设置冲掉。
// 观测数据与用户意图必须分开住。
package relayclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/jixoai/reply9842/internal/stats"
)

// RetainOptions 日志保留时长（磁盘上留多久）。
//
// 上限 30 天：按实测 300KB/天，30 天约 9MB，完全可控；再长也没有观测价值
// （同一天的历史请求体早就没意义了）。
var RetainOptions = []time.Duration{
	30 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
	7 * 24 * time.Hour,
	30 * 24 * time.Hour,
}

// WindowOptions 图表显示区间（横轴跨度）。
//
// 上限 24 小时与保留时长无关：再长的窗口一根柱子是 48 分钟，已经读不出
// 一次任务的起伏，而放大窗口只会让「刚才发生了什么」更难看清。
var WindowOptions = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// DefaultRetain 默认保留时长（与 stats 包的兜底值同一处定义，避免漂移）
const DefaultRetain = stats.DefaultRetain

// DefaultWindow 默认显示区间
const DefaultWindow = stats.DefaultWindow

// Settings 用户偏好（单位：秒，便于 JSON 直读）
type Settings struct {
	RetainSec int64 `json:"retain_sec"`
	WindowSec int64 `json:"window_sec"`
}

// DefaultSettings 默认偏好：保留一天、看最近 30 分钟
func DefaultSettings() Settings {
	return Settings{
		RetainSec: int64(DefaultRetain / time.Second),
		WindowSec: int64(DefaultWindow / time.Second),
	}
}

// Retain 保留时长
func (s Settings) Retain() time.Duration { return time.Duration(s.RetainSec) * time.Second }

// Window 显示区间
func (s Settings) Window() time.Duration { return time.Duration(s.WindowSec) * time.Second }

// Normalize 把越界或缺失的字段收敛到合法集合。
//
// 为什么要「吸附」到最近的合法档位而不是直接接受任意值：下拉框只有 6 档，
// 从手工编辑的文件里读到一个 37 分钟，UI 会显示成一个不存在的选项。
func (s *Settings) Normalize() {
	if s.RetainSec <= 0 {
		s.RetainSec = int64(DefaultRetain / time.Second)
	}
	if s.WindowSec <= 0 {
		s.WindowSec = int64(DefaultWindow / time.Second)
	}
	s.RetainSec = int64(nearestOption(time.Duration(s.RetainSec)*time.Second, RetainOptions) / time.Second)
	s.WindowSec = int64(nearestOption(time.Duration(s.WindowSec)*time.Second, WindowOptions) / time.Second)
	// 硬约束：显示区间不超过保留时长。超了就压到保留时长，
	// 再吸附一次——否则会出现「看 24 小时但只留 1 小时」这种自相矛盾的组合。
	if s.WindowSec > s.RetainSec {
		s.WindowSec = s.RetainSec
		if w := nearestOption(s.Window(), WindowOptions); time.Duration(s.WindowSec)*time.Second <= s.Retain() {
			s.WindowSec = int64(w / time.Second)
		}
	}
}

// nearestOption 找与 d 最接近的档位；空表时原样返回
func nearestOption(d time.Duration, opts []time.Duration) time.Duration {
	if len(opts) == 0 {
		return d
	}
	best, bestDiff := opts[0], absDuration(d-opts[0])
	for _, o := range opts[1:] {
		if diff := absDuration(d - o); diff < bestDiff {
			best, bestDiff = o, diff
		}
	}
	return best
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// SettingsPath 偏好文件路径（与日志同目录）
func SettingsPath() string { return filepath.Join(DefaultDir(), "settings.json") }

// LoadSettings 读取偏好。文件不存在 / 损坏时返回默认值——偏好读不出来是
// 可恢复的降级，不该让 GUI 起不来。
func LoadSettings(path string) Settings {
	s := DefaultSettings()
	if path == "" {
		path = SettingsPath()
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			s = DefaultSettings()
		}
	}
	s.Normalize()
	return s
}

// SaveSettings 写入偏好（原子替换，避免半截文件）。
func SaveSettings(path string, s Settings) error {
	if path == "" {
		path = SettingsPath()
	}
	s.Normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OptionIndex 在一组档位里找 d 的下标；找不到时返回最接近的那档下标。
func OptionIndex(d time.Duration, opts []time.Duration) int {
	if len(opts) == 0 {
		return 0
	}
	best, bestDiff := 0, absDuration(d-opts[0])
	for i, o := range opts[1:] {
		if diff := absDuration(d - o); diff < bestDiff {
			best, bestDiff = i+1, diff
		}
	}
	return best
}
