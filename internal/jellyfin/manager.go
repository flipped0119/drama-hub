package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 触发面的两个哨兵。internal/web 用 errors.Is 翻成状态码：
// ErrNotConfigured → 400（错误文本点出缺哪个键），ErrBusy → 409。
var (
	// ErrNotConfigured 表示三项启用条件没配齐（或前缀被手改成空），整段功能关闭。
	ErrNotConfigured = errors.New("jellyfin 未配置")
	// ErrBusy 表示上一轮 reconcile 还没结束：手动端点与轮末钩子共用一把单飞锁。
	ErrBusy = errors.New("jellyfin 媒体库同步正在跑")
)

// 一轮 reconcile 的四个阶段名。页面上直接显示它们，所以是给人看的中文短句；
// 顺序即执行顺序（创建与删除互斥：prune 时才有删除阶段）。
const (
	PhaseListing  = "列分类"
	PhaseDiffing  = "比对现有库"
	PhaseCreating = "创建媒体库"
	PhaseRemoving = "删除孤儿库"
	PhaseScanning = "触发扫描"
	// PhaseFinished 是一轮收尾后的阶段名：页面靠它区分「还在某个阶段」与「已经结束」。
	PhaseFinished = "已结束"
)

// State 是状态面要显示的现状：配置（密钥只报「已设置」）+ 上一次 reconcile 的结果。
//
// 只存在内存里，不落库、不加表：reconcile 本身幂等，跨重启不需要接续；重启后显示
// 「未运行」是可接受的（prune 的判定也完全不依赖历史）。
type State struct {
	Configured    bool
	Missing       []string
	URL           string
	LibraryRoot   string
	LibraryPrefix string
	APIKeySet     bool
	TimeoutSec    int
	Running       bool
	// RunSeq 是本进程里第几轮（从 1 起）。202 不带结果：页面点之前记下这个值，等它变大
	// 且 Running=false 就说明「刚点的那一轮收尾了」。重启归零。
	RunSeq int
	// StartedAt 是本轮开始时间。跑的过程中才是「这次什么时候开始的」—— LastRunAt 要等
	// 收尾才前进，所以只看它看不出「什么时候开始的」。
	StartedAt time.Time
	// Phase 是当前阶段（列分类 / 比对现有库 / 创建 / 删除 / 触发扫描），PhaseDone/PhaseTotal
	// 是该阶段已完成与总数。页面轮询读它们展示进度；收尾后停在最后一个阶段。
	Phase      string
	PhaseDone  int
	PhaseTotal int
	// DurationMS 是上一轮耗时；Failed 里的分类是 Jellyfin 明确拒掉的那些。
	DurationMS int64

	LastRunAt time.Time
	LastOKAt  time.Time
	LastError string
	Created   int
	Unchanged int
	// Uncertain 是「请求超时、Jellyfin 那边结果未知」的次数（建库为主）。
	// 实测 12.1 在扫库期间单次建库会 >10s：客户端先超时，但库往往已经建成了。
	// 所以这类不能算 created（没收到 204），也不能当成「一个都没建」——它有独立的
	// 计数与错误文案，下一轮列库会幂等地收口。
	Uncertain int
	// Failed 是这一轮建库被 Jellyfin 拒掉的分类名（实测 12.1：路径不存在回 400）。
	// 单独列出而不是并进 last_error 一句话：状态页要能点名是哪几个分类有问题。
	Failed    []string
	Orphans   []string
	Conflicts []string
}

// Result 是一轮 reconcile 做了什么（日志与状态面用）。
//
// Uncertain 与 Created 分开：一个建库请求超时时，Jellyfin 可能已经建成（实测 12.1
// 扫库期间单次 >10s），所以既不能计入 Created（没收到 204），也不能当成「一个都没建」。
type Result struct {
	Created int
	// Unchanged 是已有自管库、这一轮什么都没动的分类数（幂等命中的那些）。
	Unchanged int
	Uncertain int
	Failed    []string
	Removed   int
	Refreshed bool
	Orphans   []string
	Conflicts []string
}

// Manager 维护「一分类一库」，并在内存里保存上一次的结果。
type Manager struct {
	load       LoadConfig
	categories Categories
	logger     *slog.Logger

	// flight 是单飞锁：手动端点撞上就回 409，轮末钩子撞上就静默跳过（只记 debug）。
	flight sync.Mutex

	mu    sync.Mutex
	state State
	wg    sync.WaitGroup
}

// New 构造 Manager。load / categories 不可为 nil（装配根接上，漏接是装配 bug）。
func New(load LoadConfig, categories Categories, logger *slog.Logger) *Manager {
	return &Manager{
		load:       load,
		categories: categories,
		logger:     logger,
		state:      State{Missing: []string{}, Orphans: []string{}, Conflicts: []string{}},
	}
}

// Start 异步跑一轮 reconcile，返回 nil 表示已派出去（HTTP 202），不代表这一轮成功。
//
// 为什么异步：手动端点 202 之后请求就结束了，轮末钩子也不该延长同步轮的寿命（设计契约
// D4）。所以这一轮的上下文是 `context.WithoutCancel` + 整体超时，寿命归 Manager 所有
// （关闭时用 Wait 等它收尾）。
//
// 未配置 / 前缀为空 → ErrNotConfigured；已在跑 → ErrBusy。
func (m *Manager) Start(ctx context.Context, prune bool) error {
	cfg, err := m.load(ctx)
	if err != nil {
		return fmt.Errorf("读 jellyfin 配置: %w", err)
	}
	if missing := cfg.Missing(); len(missing) > 0 {
		return fmt.Errorf("%w: 缺少 %s", ErrNotConfigured, strings.Join(missing, "、"))
	}
	if cfg.Prefix == "" {
		// 只有库被手改成空前缀时到这里：空前缀会让所有库都算自管，prune 可能删到别人的库。
		return fmt.Errorf("%w: jellyfin_library_prefix 为空，拒绝执行", ErrNotConfigured)
	}
	if !m.flight.TryLock() {
		m.logger.Debug("jellyfin 媒体库同步已在跑，跳过这次触发")
		return fmt.Errorf("%w: 上一次还没结束", ErrBusy)
	}

	m.startProgress()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.flight.Unlock()
		defer m.setRunning(false)
		// 这一轮的上下文不属于调用方（202 后请求就结束了），寿命归 Manager 所有。
		// 不在这里定整轮预算：请求数要列库 + 算差异之后才知道，预算在 reconcile
		// 里按实际请求数给（固定倍数会在库多的部署上必然中途放弃，见 roundBudget）。
		runCtx := context.WithoutCancel(ctx)
		m.reconcile(runCtx, cfg, prune)
	}()
	return nil
}

// Wait 等手上的 reconcile 收尾（关闭时用：进程退出前不打断半轮建库）。
func (m *Manager) Wait() { m.wg.Wait() }

// State 现读配置并合上内存里的上次结果。
//
// 读配置失败只降级（按「未配置」显示），不让整个状态页 500：看板的职责是让人看见缺什么。
func (m *Manager) State(ctx context.Context) State {
	cfg, err := m.load(ctx)
	if err != nil {
		m.logger.Warn("读 jellyfin 配置失败，状态面按未配置显示", "err", err)
	}
	out := State{
		Configured:    cfg.Enabled() && cfg.Prefix != "",
		Missing:       nonNil(cfg.Missing()),
		URL:           cfg.URL,
		LibraryRoot:   cfg.LibraryRoot,
		LibraryPrefix: cfg.Prefix,
		APIKeySet:     cfg.APIKey != "",
		TimeoutSec:    int(cfg.Timeout / time.Second),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	out.Running = m.state.Running
	out.RunSeq = m.state.RunSeq
	out.StartedAt = m.state.StartedAt
	out.Phase = m.state.Phase
	out.PhaseDone = m.state.PhaseDone
	out.PhaseTotal = m.state.PhaseTotal
	out.DurationMS = m.state.DurationMS
	out.LastRunAt = m.state.LastRunAt
	out.LastOKAt = m.state.LastOKAt
	out.LastError = m.state.LastError
	out.Created = m.state.Created
	out.Unchanged = m.state.Unchanged
	out.Uncertain = m.state.Uncertain
	out.Failed = m.state.Failed
	out.Orphans = m.state.Orphans
	out.Conflicts = m.state.Conflicts
	return out
}

// roundBudget 给一轮 reconcile 的整轮预算：单请求超时 × (请求数 + 1)。
//
// 为什么要随请求数变：一轮要串行发「列库 1 + 建库 N + 扫描 1」个请求，每个请求各有
// 自己的单次超时。旧的固定 2 × timeout 只够一两个请求 —— 实测 58 个分类（单次建库
// ~1.1s、Jellyfin 扫库期间单次 >10s）时，默认 10s 超时下点上一次只建 15 个就整轮
// 中止，`last_error` 变成 `context deadline exceeded`，要连点 3–4 次才收敛。
// 多给的 1 倍是给「列库 + 算差异 + 扫描」的余量。
//
// requests <= 0 时按 1 个单位给：至少留一次请求的余量，不返回 0（0 会让 ctx 立刻过期）。
func roundBudget(requests int, timeout time.Duration) time.Duration {
	if requests < 0 {
		requests = 0
	}
	return time.Duration(requests+1) * timeout
}

// isTimeout 判断一次出站失败是不是超时（含整轮预算到期）。
//
// 只有超时是「结果未知」：Jellyfin 可能已经执行了那个请求。其余失败（连接被拒、401、
// 500）都确定没执行，照旧中止整轮。
func isTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// isRejected 判断一次建库失败是「Jellyfin 明确拒了这一个请求」（4xx）。
//
// 4xx 是可跳过的单点问题：实测 12.1 库路径不存在回 400
// （`ArgumentException: The specified path does not exist`），换下一个分类照样能建。
// 5xx / 连不上 / 认证失败不是单点的，交回默认分支整轮中止。
func isRejected(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	return se.code >= 400 && se.code < 500
}

// reconcile 跑一轮差异：列库 → 建缺的 → （prune 时）删孤儿 → 有变化就触发一次扫描。
//
// 中止策略：list 失败或 create/delete 返回非超时错误即整轮中止，记一条 last_error。
// 不做逐项重试 —— 下一轮从列库重新开始，本来就更便宜，也不会在半个配置上叠加失败。
//
// 唯一例外是请求超时：那时 Jellyfin 侧结果未知（可能已经建成），中止整轮会让库多的
// 部署永远收不了口（实测 58 个库时一次点击只建 15 个），所以计数后继续，下一轮列库
// 幂等收口。整轮预算按剩余请求数给，见 roundBudget。
func (m *Manager) reconcile(ctx context.Context, cfg Config, prune bool) (Result, error) {
	res := Result{}
	client := NewClient(cfg, nil)

	m.setPhase(PhaseListing, 0, 0)
	existing, err := client.ListLibraries(ctx)
	if err != nil {
		return m.record(res, err)
	}
	categories, err := m.categories(ctx)
	if err != nil {
		return m.record(res, fmt.Errorf("列分类: %w", err))
	}
	m.setPhase(PhaseListing, 2, 2)

	m.setPhase(PhaseDiffing, 0, 0)
	plan := Diff(existing, categories, cfg.LibraryRoot, cfg.Prefix)
	res.Orphans = plan.Orphans
	res.Conflicts = plan.Conflicts
	// 幂等命中的分类数 = 分类总数 - 要建的 - 冲突的。页面要报 unchanged。
	res.Unchanged = len(categories) - len(plan.Create) - len(plan.Conflicts)
	m.setPhase(PhaseDiffing, 1, 1)

	// 剩余请求数 = 建库 + （prune 时）删库 + 1 次扫描。整轮预算按它给：固定预算
	// 在库多的部署上必然中途放弃（58 个分类 × 实测 1.1s/次 远大于旧的 2×timeout）。
	pending := len(plan.Create) + 1
	if prune {
		pending += len(plan.Orphans)
	}
	ctx, cancel := context.WithTimeout(ctx, roundBudget(pending, cfg.Timeout))
	defer cancel()
	m.setPhase(PhaseCreating, 0, len(plan.Create))
	for _, action := range plan.Create {
		err := client.CreateLibrary(ctx, action.Name, action.Path)
		switch {
		case err == nil:
			res.Created++
		case isTimeout(err):
			// 超时 = Jellyfin 那边结果未知，不是「没建成」：单独计数后继续建剩下的。
			// 早先这里直接中止整轮，于是 58 个分类的首次同步永远停在错误态（实测
			// 一次点击只建 15 个）。下一轮列库会把已经建成的幂等收口。
			res.Uncertain++
			m.logger.Warn("建库请求超时，Jellyfin 侧结果未知（下一轮列库会收口）",
				"library", action.Name)
		case isRejected(err):
			// Jellyfin 明确拒了这一个（实测 12.1：库路径在容器里不存在回 400，
			// `ArgumentException: The specified path does not exist`）。跳过它、继续
			// 建其余分类 —— 一个坏分类不该让另外几十个都建不出来。
			res.Failed = append(res.Failed, strings.TrimPrefix(action.Name, cfg.Prefix))
			m.logger.Warn("Jellyfin 拒绝了建库，已跳过这个分类",
				"library", action.Name, "path", action.Path, "err", err)
		default:
			// 其余失败（连不上、认证失败、5xx）对一个有效配置来说是系统性的：
			// 继续打只会把同一个错误复读几十次，整轮中止留一条 last_error。
			return m.record(res, err)
		}
		m.reportCreate(res)
	}
	if prune {
		m.setPhase(PhaseRemoving, 0, len(plan.Orphans))
		for _, name := range plan.Orphans {
			if err := client.DeleteLibrary(ctx, name); err != nil {
				return m.record(res, err)
			}
			res.Removed++
			m.bumpPhase()
		}
	}
	// 0 变化时一个写请求都不发（这就是「再点一次」幂等的可验证点，AC-3）。
	// Uncertain 也算「可能有变化」：超时的那次建库很可能已经在 Jellyfin 侧落成，
	// 不扫描它就一直是个空库。
	if res.Created > 0 || res.Removed > 0 || res.Uncertain > 0 {
		m.setPhase(PhaseScanning, 0, 1)
		if err := client.Refresh(ctx); err != nil {
			return m.record(res, err)
		}
		res.Refreshed = true
		m.setPhase(PhaseScanning, 1, 1)
	}
	return m.record(res, nil)
}

// record 把一轮结果写进内存状态并记一条日志。
//
// 失败只在这里留痕：不写 source_state、不影响同步轮收尾、不进 /healthz（AC-6 的失败隔离）。
// 同名库冲突（conflicts）不是失败，last_ok_at 照常前进，但原因照样写进 last_error 让人看见。
func (m *Manager) record(res Result, err error) (Result, error) {
	now := time.Now().UTC()
	res.Failed = nonNil(res.Failed)
	res.Orphans = nonNil(res.Orphans)
	res.Conflicts = nonNil(res.Conflicts)

	m.mu.Lock()
	m.state.Phase = PhaseFinished
	m.state.LastRunAt = now
	m.state.Created = res.Created
	m.state.Unchanged = res.Unchanged
	m.state.Uncertain = res.Uncertain
	if !m.state.StartedAt.IsZero() {
		m.state.DurationMS = now.Sub(m.state.StartedAt).Milliseconds()
	}
	m.state.Failed = nonNil(res.Failed)
	m.state.Orphans = res.Orphans
	m.state.Conflicts = res.Conflicts
	m.state.LastError = ""
	if err != nil {
		m.state.LastError = err.Error()
	} else {
		switch {
		case len(res.Failed) > 0:
			// 部分失败：其余分类照建了，所以这不算整轮失败（last_ok_at 前进），但必须
			// 点名是哪几个分类被拒——Jellyfin 的原文是「路径不存在」，人要看得到。
			m.state.LastOKAt = now
			m.state.LastError = "Jellyfin 拒绝了这些分类的建库（多半是库路径在 Jellyfin 容器内不存在）：" +
				strings.Join(res.Failed, "、")
			if res.Uncertain > 0 {
				m.state.LastError += fmt.Sprintf("；另有 %d 个建库请求超时，结果未知", res.Uncertain)
			}
		case res.Uncertain > 0:
			// 超时的几次结果未知：既不能读成失败（Jellyfin 很可能已经建成），也不能
			// 读成成功（没收到 204），所以 last_ok_at 不前进 —— 它只表示「上一轮完整
			// 成功了」。下一轮列库幂等收口后照常前进。
			m.state.LastError = fmt.Sprintf(
				"有 %d 个建库请求超时，Jellyfin 侧结果未知（下一轮同步会收口）", res.Uncertain)
		default:
			// 冲突与孤儿都不是失败：last_ok_at 照常前进，原因照样写进 last_error 让人看见。
			m.state.LastOKAt = now
			if len(res.Conflicts) > 0 {
				m.state.LastError = "同名库不是本服务建的，已跳过：" + strings.Join(res.Conflicts, "、")
			}
		}
	}
	m.mu.Unlock()

	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	m.logger.Log(context.Background(), level, "jellyfin 媒体库同步结束",
		"created", res.Created,
		"unchanged", res.Unchanged,
		"failed", len(res.Failed),
		"removed", res.Removed,
		"refreshed", res.Refreshed,
		"orphans", len(res.Orphans),
		"conflicts", len(res.Conflicts),
		"err", err)
	return res, err
}

// setRunning 更新「正在跑」标志（状态页读它）。
func (m *Manager) setRunning(running bool) {
	m.mu.Lock()
	m.state.Running = running
	m.mu.Unlock()
}

// startProgress 开一轮的进度：序号 +1、记开始时间、把阶段与本轮计数复位。
//
// 在 flight 锁内调用（Start 拿到锁之后），所以不需要额外的并发保护。
//
// 计数一起复位是必须的：页面在这一轮跑的时候轮询，读到的是「本轮」而不是上一轮的
// 残留（否则「已建 58 个」会在新一轮开始时先跳成上轮的数字再跳回来）。
func (m *Manager) startProgress() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Running = true
	m.state.RunSeq++
	m.state.StartedAt = time.Now().UTC()
	m.state.Phase = PhaseListing
	m.state.PhaseDone, m.state.PhaseTotal = 0, 0
	m.state.Created = 0
	m.state.Unchanged = 0
	m.state.Uncertain = 0
	m.state.Failed = []string{}
	m.state.DurationMS = 0
}

// setPhase 记一个阶段的进度。done/total 是「该阶段已处理/总数」，页面轮询读它。
func (m *Manager) setPhase(phase string, done, total int) {
	m.mu.Lock()
	m.state.Phase = phase
	m.state.PhaseDone, m.state.PhaseTotal = done, total
	m.mu.Unlock()
}

// bumpPhase 只推进当前阶段的已完成数（逐项处理时用，不动 total）。
func (m *Manager) bumpPhase() {
	m.mu.Lock()
	m.state.PhaseDone++
	m.mu.Unlock()
}

// reportCreate 推进建库阶段的进度，并把实时计数写进状态。
//
// 实时写是为了让孩子轮询看见「已建 3 个」在动，而不是等整轮收尾才一次性出现；
// 收尾时 record 会再写一遍（含 Unchanged 与耗时）。
func (m *Manager) reportCreate(res Result) {
	m.mu.Lock()
	m.state.PhaseDone++
	m.state.Created = res.Created
	m.state.Uncertain = res.Uncertain
	m.state.Failed = nonNil(append([]string{}, res.Failed...))
	m.mu.Unlock()
}

// nonNil 把 nil 切片换成空切片：JSON 里 null 与 [] 对页面是两种东西，
// 状态面一律给 []，前端不需要为 null 多写一条分支。
func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
