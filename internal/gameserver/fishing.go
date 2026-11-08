package gameserver

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 钓鱼状态、判鱼、咬钩调度和 14/14 推送。

// FishState 保存玩家单根竿的钓鱼状态，重连后继续使用。
type FishState struct {
	mu          fishStateMutex
	finishMu    sync.Mutex
	GearGuid    string
	RigType     int64 // jppmjnahbpo 钓组种类（on_cast 记录；路亚族单独节奏）
	Loc         string
	Spot        uint64
	DepthCM     int64 // 玩家设钓深（17/12 记入；14/1 的 158.f2 透传用）
	ReelSetting int64 // 17/15 收线速度档（路亚族）；摩擦档位见 rod_setting.reel_drag_setting
	// reelWear 是本竿搏鱼期间的渔轮磨损账（泄力打滑 + 锁轮过载）。仅进程内，
	// 落库后清零；口径见 specs/changes/reel-friction-life-model/spec.md。
	reelWear   fightReelWear
	CastActive bool
	CastM      float64 // 本杆 14/2 上报的实际落点距离（米），不是钓具理论最大值
	InWater    bool
	NeedBite   bool
	PosX       float64
	PosZ       float64
	HasPos     bool
	CastPosX   float64
	CastPosZ   float64
	HasCastPos bool
	// BoatUntil 是船票租用有效的截止 unix，3/7 验票上船时写入。0 表示本次会话
	// 还没有上船证据，判鱼对“已上船”条件保持中性。
	BoatUntil int64
	// BoatBoardAt / BoatOffAt 是客户端 3/10 boat_use 上报的上船、下船 unix。
	// 3/10 只差请求体尾字节，已用实机样本对位（0x0e 上船、0x0f 下船）。
	BoatBoardAt     int64
	BoatOffAt       int64
	CastTs          time.Time
	FallTs          time.Time
	FallSeen        bool
	RigSettled      bool
	TickSeen        bool
	TickSeq         uint64  // 14/7 f14 客户端 tick 序号（诊断/恢复）
	LastAnim        uint32  // 14/7 f5 上一次的动画位（仅在变更时打日志）
	LureMotionMask  uint32  // 14/7 f5 原始拟饵运动位，保留给完整状态映射
	LureAction      string  // 已识别的运动状态；未知时为空
	LureLayer       float64 // 0=水面，1=水底；路亚实时水层
	LureStateAt     time.Time
	LureLayerAt     time.Time
	LurePulseAt     time.Time
	LurePulseCount  int
	LureToggleAt    time.Time
	LureToggleCount int
	PushSent        bool
	pushing         *RolledFish // 仅进程内：防止同一尾鱼并发重推
	Hooked          bool
	Resolved        bool
	Pending         *RolledFish
	biteTimer       *time.Timer
	pendingTimer    *time.Timer
	castGeneration  uint64
	biteTicket      uint64
	pendingTicket   uint64
	PendingRepush   int           // 14/12 f0=3/5/6 连续拒收鱼实体时的重推次数
	BiteAt          time.Time     // 原定咬钩绝对时刻；硬重启后据此恢复 timer
	Keepnet         []*RolledFish // 回退缓存（DB 权威）
	Bait            string
	LastSkill       int64       // 本次钓鱼大类（1 浮钓 / 2 纺车）
	Armed           *RolledFish // GM 武装"下一杆必上"（唯一绕过重量上限的路径）
	Bonus           fishingSkillBonus
	persist         fishPersistMeta

	hookedChatFishGUID string // 仅进程内：同一尾鱼的确认与拒收重推共用播报标记。
}

// effectiveBaitDepthCMLocked 返回当前有效饵深。手杆从落水开始按固定
// 下沉速度接近调漂深度，到底后保持；底层杆由 rigType 直接锁定底层。
func (st *FishState) effectiveBaitDepthCMLocked(now time.Time) int64 {
	if st == nil {
		return 0
	}
	if rigTypeLureFamily[st.RigType] {
		return int64(math.Round(normalizeWaterLayer(st.LureLayer) * 500))
	}
	target := st.DepthCM
	if target <= 0 {
		target = 300
	}
	start := st.FallTs
	if start.IsZero() {
		start = st.CastTs
	}
	if start.IsZero() {
		return target
	}
	elapsed := now.Sub(start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	depth := int64(math.Round(elapsed * 25)) // 约 0.25 m/s
	if depth > target {
		depth = target
	}
	if depth < 0 {
		depth = 0
	}
	return depth
}

func (st *FishState) recordLurePulseLocked(now time.Time) {
	if st.LurePulseAt.IsZero() || now.Sub(st.LurePulseAt) > 1500*time.Millisecond {
		st.LurePulseCount = 0
	}
	st.LurePulseAt = now
	st.LurePulseCount++
	st.updateLureActionLocked(now, st.LureMotionMask, st.LureMotionMask)
}

// updateLureActionLocked 优先采用客户端已经算好的 kRigAnimationType_* 位；
// 14/9 右键脉冲用于补齐客户端未放进 14/7 的短抽动作。
func (st *FishState) updateLureActionLocked(now time.Time, previousMask, mask uint32) {
	moving := mask&0x400 != 0
	prevMoving := previousMask&0x400 != 0
	if moving != prevMoving && moving {
		if st.LureToggleAt.IsZero() || now.Sub(st.LureToggleAt) > 6*time.Second {
			st.LureToggleCount = 0
		}
		st.LureToggleAt = now
		st.LureToggleCount++
	}
	pulseRecent := !st.LurePulseAt.IsZero() && now.Sub(st.LurePulseAt) <= 1500*time.Millisecond
	previousAction := st.LureAction
	action := classifyLureMotion(mask)
	if action == "steady" || action == "speed_up" {
		action = ""
	}
	if action == "jig" && st.RigType >= 50 && st.RigType <= 60 {
		action = "marine_jig"
	}
	switch {
	case action != "":
		st.LureAction = action
	case pulseRecent && st.LurePulseCount >= 3:
		st.LureAction = "hard_twitch"
	case pulseRecent && st.LurePulseCount == 2:
		st.LureAction = "jerk"
	case pulseRecent:
		st.LureAction = "twitch"
	case moving && st.LureToggleCount >= 3:
		st.LureAction = "stop_go"
	case moving:
		st.LureAction = "steady"
	default:
		st.LureAction = ""
	}
	if st.LureAction != previousAction {
		st.LureStateAt = now
	}
}

type fishingTimerTicket struct{ generation, sequence uint64 }

// markBoardLocked 记录玩家上船到何时（取船票租用到期时间）。调用方需持有 st.mu。
func (st *FishState) markBoardLocked(unix int64) {
	st.BoatUntil = unix
}

// markBoatClientEventLocked 记录客户端 3/10 boat_use 上报的上船或下船时刻。
// 调用方需持有 st.mu。下船只翻转“是否在船上”，不动船票租期：租期没结束前玩家
// 可以回船上（3/7 会复用同一张票），下一次上船上报会把状态重新置为在船上。
func (st *FishState) markBoatClientEventLocked(nowUnix int64, board bool) {
	if board {
		st.BoatBoardAt = nowUnix
		return
	}
	st.BoatOffAt = nowUnix
}

// onBoatLocked 判断当前是否还在船上。调用方需持有 st.mu。
// known=false 表示本次会话没见过上船流程，调用方不该据此封鱼。
// 优先级：客户端 3/10 实时上报 > 3/7 验票租期。租期已过时即便没收到下船上报
// 也判不在船上（原版此时会把玩家从船上送回岸）。
func (st *FishState) onBoatLocked(nowUnix int64) (on, known bool) {
	if st.BoatUntil == 0 && st.BoatBoardAt == 0 && st.BoatOffAt == 0 {
		return false, false
	}
	if st.BoatUntil > 0 && st.BoatUntil <= nowUnix {
		return false, true
	}
	if st.BoatOffAt > st.BoatBoardAt {
		return false, true
	}
	return true, true
}

func (st *FishState) cancelBiteTimerLocked() {
	if st.biteTimer != nil {
		st.biteTimer.Stop()
		st.biteTimer = nil
	}
	st.biteTicket++
}

func (st *FishState) cancelPendingTimerLocked() {
	if st.pendingTimer != nil {
		st.pendingTimer.Stop()
		st.pendingTimer = nil
	}
	st.pendingTicket++
}

func (s *Server) armBiteTimerLocked(uid int64, st *FishState, due time.Time) {
	st.cancelBiteTimerLocked()
	st.BiteAt = due
	ticket := fishingTimerTicket{st.castGeneration, st.biteTicket}
	st.biteTimer = time.AfterFunc(time.Until(due), func() { s.onRodBiteTimerTicket(uid, st, ticket) })
}

func (s *Server) armPendingTimerLocked(uid int64, st *FishState, delay time.Duration) {
	st.cancelPendingTimerLocked()
	ticket := fishingTimerTicket{st.castGeneration, st.pendingTicket}
	fish := st.Pending
	st.pendingTimer = time.AfterFunc(delay, func() { s.repushRodPendingTicket(uid, st, ticket, fish) })
}

// maxPendingRepush 限制 14/12 f0=3/5/6 连续拒收鱼实体时的重推次数。
// 超过后放弃这尾鱼，避免每 6 秒无限重发 14/14。
const maxPendingRepush = 2

// RolledFish 待结算/已结算的鱼。
type RolledFish struct {
	GearGUID       string // 发鱼时的原竿；切手持竿不能改变这尾鱼的归属。
	HookGUID       string
	HappyHour      bool
	HappyHourKnown bool
	BaitGUIDs      string
	BaitsKnown     bool

	FishGUID string
	Species  string
	Grade    int64
	WeightG  int64
	LengthM  float64
	XP       int64
	Silver   int64
	LocLevel string
	Stamina  float64 // obj27#05 搏鱼体力池；0=按体重和已验证兜底比例计算
	Force    float64 // obj27#08 拉力；0=按体重和已验证比例计算
	NamedID  string  // 被命名鱼
	Name     string
	// DepthCM/OnBoat 是咬钩瞬间的钓深与船只状态，只供资格判定读取；随
	// pending_json 持久化，重启恢复后仍按咬钩时条件判定。
	DepthCM int64
	OnBoat  bool
	// Drift 记录咬钩瞬间钓组是否处于顺流移动（14/7 0x40），同样只供资格判定，
	// 并随 pending_json 持久化。
	Drift bool
	// QualBait 是咬钩瞬间的饵属性快照。天然饵会在 14/11 被 consumeHookedBait
	// 删除，14/4 再按 BaitGUIDs 反查只会得到空值，因此必须在咬钩时快照。
	QualBait qualificationBait
	// CastM / IsNight 同样是咬钩快照：抛投距离取本杆实际落点，夜间按咬钩
	// 时刻判定，避免搏鱼跨过 06:00/19:00 后改用结算时间。
	CastM   float64
	IsNight bool
}

// stateFor 按 uid 取/建状态。
func (s *Server) stateFor(uid int64) *FishState {
	st := s.fishingState(uid)
	s.resumeFishingState(st)
	return st
}

// ---- 钓组动画就位位族 ----
// 0x200 水层静止（浮标）、0x800 底部静止（沉底/Feeder）、0x1000 水面静止。
const rigAnimSettledMask = 0x200 | 0x800 | 0x1000

// rigAnimInWaterMask 是“钓组在水中”的位族。协议字段表确认
// 0x200=MotionlessInWaterColumn；0x400/0x800/0x1000 分别是不同水层/姿态的
// 静止状态。朝岸上抛竿不会出现这些位，因此仍能挡住“饵落在陆地”。
const rigAnimInWaterMask = 0x200 | 0x400 | 0x800 | 0x1000

// ---- 咬钩延迟 ----

// PUSH_DELAY_BANDS 四档混合分布（概率, 下限s, 上限s）。
var pushDelayBands = []struct {
	p      float64
	lo, hi float64
}{
	{0.40, 6.0, 10.0},
	{0.40, 10.0, 16.0},
	{0.15, 16.0, 24.0},
	{0.05, 24.0, 35.0},
}

// officialPushDelaySamples 是咬钩延迟样本。运行时过滤低于 6 秒的值，
// 与 14/3 的落水立漂保护一致。
var officialPushDelaySamples = []float64{
	0.52, 1.49, 1.93, 3.48, 3.52, 4.52,
	6.48, 6.7, 7.09, 7.53, 9.51, 9.77, 10.49, 11.53,
	12.1, 12.48, 13.43, 13.5, 17.7, 17.73, 17.79, 21.48,
	21.5, 23.5, 25.35, 25.62, 27.49, 34.47, 38.47, 38.49,
	38.49, 46.6,
}

var officialPushDelayEligible = func() []float64 {
	out := make([]float64, 0, len(officialPushDelaySamples))
	for _, v := range officialPushDelaySamples {
		if v >= 6.0 {
			out = append(out, v)
		}
	}
	return out
}()

func officialPushDelayWith(rng *rand.Rand) float64 {
	if len(officialPushDelayEligible) > 0 {
		if rng == nil {
			return officialPushDelayEligible[rand.Intn(len(officialPushDelayEligible))]
		}
		return officialPushDelayEligible[rng.Intn(len(officialPushDelayEligible))]
	}
	// 数据文件异常时的四档兜底；运行时正常不会走到这里。
	r := rand.Float64()
	if rng != nil {
		r = rng.Float64()
	}
	acc := 0.0
	for _, b := range pushDelayBands {
		acc += b.p
		if r <= acc {
			u := rand.Float64()
			if rng != nil {
				u = rng.Float64()
			}
			v := b.lo + u*(b.hi-b.lo)
			return math.Round(v*100) / 100
		}
	}
	return 10.0
}

func officialPushDelay() float64 { return officialPushDelayWith(nil) }

// biteDelay 依次应用采样、技能加成、范围约束和倍率。
// 路亚使用 1.5~6 秒的短窗，避免 14/14 晚于收线窗口。
func (s *Server) biteDelay(st *FishState, bonuses ...fishingSkillBonus) float64 {
	return s.biteDelayInWeather(st, fishingWeather{}, time.Now(), bonuses...)
}

// biteDelaySettings 是一次咬钩实际使用的参数。全局参数会被水域级 fish_bite_setting 覆盖，
// 延迟采样和落水保护必须共用同一份结果，否则两者会按不同配置工作。
type biteDelaySettings struct {
	fixed    float64
	scale    float64
	maxDelay float64
	lureMax  float64
}

func (s *Server) effectiveBiteDelaySettings(st *FishState) biteDelaySettings {
	cfg := biteDelaySettings{
		fixed:    s.tunableF("bite_delay_fixed", 0),
		scale:    s.tunableF("bite_delay_scale", 1.0),
		maxDelay: s.tunableF("bite_delay_max", 20.0),
		lureMax:  s.tunableF("bite_delay_lure_max", 6.0),
	}
	if st != nil && st.Loc != "" && s.DB != nil {
		biteLoc := st.Loc
		if resolved := s.DB.ResolveStatLoc(s.dbCtx(), biteLoc); resolved != "" {
			biteLoc = resolved
		}
		if setting, err := s.DB.GetFishBiteSetting(s.dbCtx(), biteLoc); err == nil && setting.HasOverride {
			cfg = biteDelaySettings{
				fixed:    setting.BiteDelayFixed,
				scale:    setting.BiteDelayScale,
				maxDelay: setting.BiteDelayMax,
				lureMax:  setting.BiteDelayLureMax,
			}
		}
	}
	return cfg
}

func (s *Server) biteDelayInWeather(st *FishState, weather fishingWeather, now time.Time, bonuses ...fishingSkillBonus) float64 {
	bonus := neutralFishingSkillBonus()
	if len(bonuses) > 0 {
		bonus = bonuses[0]
	}
	cfg := s.effectiveBiteDelaySettings(st)
	if cfg.fixed > 0 {
		return math.Max(0.05, cfg.fixed)
	}
	weatherDelay := s.weatherBiteDelayFactor(st, weather, now)
	if st != nil && rigTypeLureFamily[st.RigType] {
		d := (1.5 + rand.Float64()*(6.0-1.5)) * weatherDelay
		d *= bonus.BiteSpeed
		if d < 0.8 {
			d = 0.8
		}
		if capS := cfg.lureMax; capS > 0 && d > capS {
			d = capS
		}
		if cfg.scale != 1.0 {
			d *= cfg.scale
		}
		return math.Max(0.8, d)
	}
	d := officialPushDelay() * weatherDelay
	return standardBiteDelay(d, bonus.BiteSpeed, cfg.maxDelay, cfg.scale)
}

// biteFloorDelay 返回落水保护时长。
//
// 浮钓/底钓的基础保护是 6 秒；该值必须跟随 effective bite config，
// 否则把倍率调快后第一轮到点仍会被"落水未满 6 秒"顺延回去，
// 玩家看到的就是固定 6 秒延迟。下限 800ms，避免倍率极小时立刻判鱼。
// 路亚族固定 1.2 秒：拟饵落水即生效，没有"浮标立稳"这一环，也不吃倍率。
func biteFloorDelay(cfg biteDelaySettings, lure bool) time.Duration {
	if cfg.fixed > 0 {
		// fixed 是管理员明确指定的延迟，不能被默认落水保护反向覆盖。
		return time.Duration(math.Max(0.05, cfg.fixed) * float64(time.Second))
	}
	if lure {
		return 1200 * time.Millisecond
	}
	base := 6.0
	if cfg.maxDelay > 0 && cfg.maxDelay < base {
		// 普通竿的上限可能低于默认落水保护；否则 bite_delay_max 永远不会生效。
		base = cfg.maxDelay
	}
	floor := time.Duration(base * float64(time.Second))
	if cfg.scale > 0 {
		scaled := time.Duration(float64(floor) * cfg.scale)
		if scaled < 800*time.Millisecond {
			scaled = 800 * time.Millisecond
		}
		floor = scaled
	}
	return floor
}

func standardBiteDelay(sample, biteSpeed, capS, scale float64) float64 {
	if biteSpeed <= 0 {
		biteSpeed = 1
	}
	d := sample * biteSpeed
	if d < 6.0 {
		d = 6.0
	}
	if capS > 0 && d > capS {
		d = capS
	}
	if scale != 1.0 {
		d *= scale
	}
	return math.Max(0.05, d)
}

// biteProbabilityForLoc 上鱼概率（默认 1.0 每杆必出）。后台鱼口覆盖优先。
func (s *Server) biteProbabilityForLoc(loc string) float64 {
	p := s.tunableF("bite_probability", 1.0)
	if s.DB != nil && loc != "" {
		biteLoc := loc
		if resolved := s.DB.ResolveStatLoc(s.dbCtx(), biteLoc); resolved != "" {
			biteLoc = resolved
		}
		if setting, err := s.DB.GetFishBiteSetting(s.dbCtx(), biteLoc); err == nil && setting.HasOverride {
			p = setting.BiteProbability
		}
	}
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// tunableF 运行参数读取（PG tunables 优先，缓存 30s；缺省回退）。
func (s *Server) tunableF(key string, def float64) float64 {
	s.tunMu.Lock()
	defer s.tunMu.Unlock()
	if s.tunCache == nil || time.Since(s.tunAt) > 30*time.Second {
		m := map[string]float64{}
		if s.DB != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			rows, err := s.DB.SQL.QueryContext(ctx, `SELECT key, value FROM tunables`)
			if err == nil {
				for rows.Next() {
					var k, v string
					if rows.Scan(&k, &v) == nil {
						var f float64
						if jsonFloat(v, &f) == nil {
							m[k] = f
						}
					}
				}
				rows.Close()
			}
			cancel()
		}
		s.tunCache = m
		s.tunAt = time.Now()
	}
	if v, ok := s.tunCache[key]; ok {
		return v
	}
	return def
}

func jsonFloat(s string, out *float64) error {
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%g", out)
	return err
}

// ---- 判鱼 ----

type fishPoolEntry struct {
	species       string
	minG, maxG    int64
	valuableLineG int64
}

// 内置鱼池按地点保存鱼种、重量范围和达标线。
var fishPools = map[string][]fishPoolEntry{
	"loc1_02": {
		{"crucian", 150, 900, 400}, {"c.roach", 80, 600, 318},
		{"freshwater_mussel", 30, 150, 80}, {"perch", 100, 800, 340}, {"ruffe", 50, 300, 84},
	},
	"lvl2_01": {
		{"c.roach", 80, 700, 318}, {"perch", 100, 1200, 340}, {"ruffe", 50, 300, 84},
		{"ruffe_n", 50, 300, 84}, {"crucian", 150, 900, 400},
	},
	"level_019_american_pond": {
		{"perch", 100, 1500, 340}, {"c.roach", 80, 600, 318}, {"crucian", 150, 900, 400},
	},
}

var defaultFishPool = []fishPoolEntry{
	{"crucian", 150, 900, 400}, {"c.roach", 80, 600, 318},
	{"perch", 100, 1200, 340}, {"freshwater_mussel", 30, 150, 80},
}

// maxMockWeight 是缺少可信装备物理数据时使用的重量上限。
const maxMockWeight = 650

// 以下三项来自旧版对真实服务器 14/14 帧的逐字节校准：
// 53g -> 体力池 126.5661239624、拉力 0.5197524514；
// 63g -> 拉力 0.6178189516。旧版两条样本的拉力都严格满足每克 0.00980665。
const (
	verifiedStaminaPerGram      = 2.3880400747623083
	verifiedInitialStaminaRatio = 0.5628187656402588
	verifiedForcePerGram        = 0.009806650025503976
	// 默认使用真实样本校准值；需要调整手感时只通过 tunables 显式覆盖，
	// 不再把主观补偿值作为全体鱼的基准。
	defaultFishStaminaScale        = 1.0
	defaultFishInitialStaminaRatio = verifiedInitialStaminaRatio
)

// lengthSeed 保存体长公式 k*w^p 的参数。
var lengthSeed = map[string][2]float64{
	"perch":   {0.021948, 0.40868},
	"c.roach": {0.033498, 0.34010},
	"s.bream": {0.066818, 0.20866},
	"crucian": {0.035129, 1.0 / 3.0},
}

// scoreRate 是默认积分率。
func scoreRate(grade int64) float64 {
	switch grade {
	case 1:
		return 2.0
	case 2:
		return 2.6
	case 3:
		return 4.0
	case 4:
		return 6.0
	}
	return 2.0
}

// speciesPushCalib 按鱼种和档位提供 14/14 的积分率与质量系数。
// 未配置时使用通用值。
var speciesPushCalib = map[string]map[int64][2]float64{
	"ruffe":     {2: {0.8625, 0.4}},
	"c.roach":   {1: {1.6825, 0.5}, 2: {2.3515, 0.5}},
	"perch":     {1: {2.8268, 0.7}, 2: {2.4336, 0.8}},
	"c.carp":    {1: {2.6033, 0.4}, 2: {2.7555, 0.4}},
	"crucian":   {1: {0, 0.55}, 2: {1.9721, 0.6}},
	"a.sleeper": {1: {3.241, 0.3}, 2: {4.3602, 0.3}},
	"vjun":      {1: {23.0181, 0.1}},
	"e.chub":    {1: {0, 0.7}, 2: {0, 0.75}},
	"bream":     {2: {0, 0.3}},
	"s.bream":   {2: {0, 0.3}},
}

func pushScoreRate(species string, grade int64) float64 {
	if byGrade := speciesPushCalib[species]; byGrade != nil {
		if v := byGrade[grade]; v[0] > 0 {
			return v[0]
		}
	}
	return scoreRate(grade)
}

func fishQuality(species string, grade int64) float64 {
	if byGrade := speciesPushCalib[species]; byGrade != nil {
		if v := byGrade[grade]; v[1] > 0 {
			return v[1]
		}
	}
	return 0.5
}

// rollFish 优先从静态鱼库判鱼，地点无数据时回退内置鱼池。
// bait 是 14/2 记录的钩上饵 systemID，可影响鱼种权重。
func (s *Server) rollFish(loc string, bait string) *RolledFish {
	return s.rollFishWithRules(loc, bait, 0, fishingSkillBonus{BiteSpeed: 1})
}

// rollFishForRig 是判鱼入口：先按两个调试开关计算有效派鱼上限（默认 0，不截断），
// 再对静态鱼库和内置鱼池应用该上限及奖杯幸运。
func (s *Server) rollFishForRig(ctx context.Context, uid int64, gearGUID, loc, bait string, bonus fishingSkillBonus, contexts ...fishRollContext) *RolledFish {
	c := rollContext(contexts)
	c.loc, c.skill, c.uid = loc, bonus, uid
	c.forRig = s.tunableF("fish_bait_rules_enabled", 0) == 1 || s.tunableF("fish_weather_enabled", 0) == 1 || c.happyBite > 0
	c.rigType = s.rigTypeForWrite(ctx, gearGUID)
	if s.DB != nil && gearGUID != "" {
		if rod, _ := s.DB.GetItemByGUID(ctx, gearGUID); rod != nil && rod.UID != uid {
			return nil
		}
		c.baits = s.currentBaits(ctx, gearGUID)
	}
	if s.tunableF("fish_weather_enabled", 0) == 1 && s.weatherRigBroken(ctx, uid, gearGUID) {
		return nil
	}
	capKg, ids, weakest := s.gearWeightCapForRig(ctx, uid, gearGUID)
	groups := s.baitedHookSets(ctx, gearGUID)
	if len(groups) == 0 {
		rigType := int64(0)
		if s.DB != nil && gearGUID != "" {
			rigType = s.rigTypeForWrite(ctx, gearGUID)
		}
		if len(c.baits) > 0 && hookPositionsFor(rigType) > 1 {
			return nil
		}
		if capKg > 0 {
			// 默认两条截断都关（capKg=0 不进这里）；只有管理员显式打开才会打这条日志。
			strengthOn := s.tunableF("fish_weight_cap", 0) > 0
			entryOn := s.tunableF("hook_entry_cap", 0) > 0
			capRule := "hook_entry"
			switch {
			case strengthOn && entryOn:
				capRule = "strength+hook_entry"
			case strengthOn:
				capRule = "strength"
			}
			s.log.Info("派鱼重量上限", "rod", ids.Rod, "reel", ids.Reel, "line", ids.Line,
				"leader", ids.Leader, "weakest", weakest, "cap_kg", capKg, "rule", capRule)
		}
		return s.rollFishWithRules(loc, bait, capKg, bonus, c)
	}
	spec := s.loadGearSpecForRig(ctx, ids, gearGUID)
	rand.Shuffle(len(groups), func(i, j int) { groups[i], groups[j] = groups[j], groups[i] })
	for _, group := range groups {
		selected := spec
		if !s.applyBaitedHookSpec(ctx, group, &selected) {
			continue
		}
		c.baits = group.baits
		f := s.rollFishWithRules(loc, group.baits[0], s.fishWeightCapKg(selected), bonus, c)
		if f != nil {
			f.HookGUID = group.hookGUID
			f.BaitGUIDs, f.BaitsKnown = strings.Join(group.baitGUIDs, " "), true
			return f
		}
	}
	return nil
}

func (s *Server) rollFishWithRules(loc, bait string, gearCapKg float64, bonus fishingSkillBonus, contexts ...fishRollContext) *RolledFish {
	c := rollContext(contexts)
	if c.baits == nil {
		c.baits = []string{bait}
	}
	if s.tunableF("fish_bait_rules_enabled", 0) == 1 {
		if s.DB != nil && s.cachedResolveStatLoc(s.dbCtx(), c, loc) == "" {
			return s.rollFishPoolsWithRules(loc, gearCapKg, bonus, c)
		}
		if s.DB == nil {
			return nil
		}
		return s.rollFishFromDBWithRules(loc, bait, gearCapKg, bonus, c)
	}
	if s.DB != nil {
		if f := s.rollFishFromDBWithRules(loc, bait, gearCapKg, bonus, c); f != nil {
			return f
		}
		// 已知水域的鱼群无法匹配时保持空竿。
		if roster, err := s.cachedLocRoster(s.dbCtx(), c, s.cachedResolveStatLoc(s.dbCtx(), c, loc)); err == nil && len(roster) > 0 {
			return nil
		}
	}
	return s.rollFishPoolsWithRules(loc, gearCapKg, bonus, c)
}

// rollFishFromDB 根据静态统计判鱼：地点名册提供鱼种，捕获计数提供权重，
// 饵效调整权重，体重按鱼种分布采样，档位由重量决定，体长由鱼种参数计算。
// 地点无数据时返回 nil，由调用方回退内置鱼池。
func (s *Server) rollFishFromDB(loc, bait string) *RolledFish {
	return s.rollFishFromDBWithRules(loc, bait, 0, fishingSkillBonus{BiteSpeed: 1})
}

func (s *Server) rollFishFromDBWithRules(loc, bait string, gearCapKg float64, bonus fishingSkillBonus, contexts ...fishRollContext) *RolledFish {
	c := rollContext(contexts)
	if c.baits == nil {
		c.baits = []string{bait}
	}
	strict := s.tunableF("fish_bait_rules_enabled", 0) == 1
	distribution := s.tunableF("fish_distribution_enabled", 0) == 1
	cctx := s.dbCtx()
	statLoc := s.cachedResolveStatLoc(cctx, c, loc)
	if statLoc == "" {
		return nil
	}
	s.prepareFishRollRuntime(cctx, &c, statLoc)
	roster, err := s.cachedLocRoster(cctx, c, statLoc)
	if err != nil || len(roster) == 0 {
		return nil
	}
	keys := make([]string, 0, len(roster))
	wts := make([]float64, 0, len(roster))
	for _, r := range roster {
		keys = append(keys, r.FishKey)
		wts = append(wts, r.Catches)
	}
	// 管理后台鱼口覆盖：静态名册保持不变，覆盖值和额外鱼种在运行时叠加。
	// 若所有有效权重都被关成 0，则忽略整组覆盖，避免 weightedChoice 落到
	// 最后一个已禁用的鱼种。
	if s.DB != nil {
		if ovs, oerr := s.DB.FishWeightOverrides(cctx, statLoc); oerr == nil && len(ovs) > 0 {
			seen := make(map[string]int, len(keys))
			for i, key := range keys {
				seen[key] = i
			}
			base := append([]float64(nil), wts...)
			hasPositive := false
			for key, ov := range ovs {
				i, ok := seen[key]
				if !ok {
					keys = append(keys, key)
					wts = append(wts, 0)
					i = len(wts) - 1
					seen[key] = i
				}
				if !ov.Enabled || ov.Weight <= 0 {
					wts[i] = 0
					continue
				}
				wts[i] = ov.Weight
				hasPositive = true
			}
			if !hasPositive && !strict && !distribution && s.tunableF("fish_weather_enabled", 0) != 1 {
				copy(wts, base)
				keys = keys[:len(roster)]
				wts = wts[:len(roster)]
			}
		}
	}
	interests := map[string]float64{}
	if strict {
		base := map[string]float64{}
		for i, key := range keys {
			base[key] = wts[i]
		}
		keys, wts = s.filterFishBaitSet(statLoc, c.baits, keys, wts, c.now, c)
		if len(keys) == 0 {
			return nil
		}
		for i, key := range keys {
			interests[key] = math.Min(1, wts[i]/base[key]/maxBaitPreference)
		}
	}
	if distribution {
		applyFishDistribution(statLoc, keys, wts, c)
		if !slices.ContainsFunc(wts, func(w float64) bool { return w > 0 }) {
			return nil
		}
	}
	for i, key := range keys {
		wts[i] *= s.fishBiteEnvironmentWeight(cctx, key, c)
	}
	applyFishSpotBias(keys, wts, c.runtime.spotPref, c.runtime.spotData)
	// 窝料吸引：把落点格子里那锅料的食性亲和乘到候选权重上。
	// 关掉 groundbait_enabled 时这里一行都不执行。
	s.applyGroundbaitWeight(cctx, &c, keys, wts)
	if !slices.ContainsFunc(wts, func(w float64) bool { return w > 0 }) {
		return nil
	}
	// companion 周纪录饵效（config bait_pref_scale / bait_pref_weight_shift；
	// 缺表=空，运行中性）。tier：top5=3 / top20=2 / 在列=1 → ×8/×4/×2；
	// 未在列 ×(0.9 - 0.6×覆盖度)（覆盖度 = total_evidence/200，封顶 1）。
	prefScale := s.tunableF("bait_pref_scale", 1.0)
	var tiers map[string]int64
	var meta map[string]int64
	if !strict && bait != "" && prefScale > 0 {
		tiers = s.cachedBaitTiers(cctx, c, bait)
		if tiers != nil {
			meta = s.cachedCompanionEvidence(cctx, c)
		}
	}
	baitHit := false
	if tiers != nil {
		for i, key := range keys {
			t := tiers[key]
			if t > 0 {
				wts[i] *= companionTierMult(t) * prefScale
				baitHit = true
				continue
			}
			ev := meta[key]
			cov := float64(ev) / 200.0
			if cov > 1.0 {
				cov = 1.0
			}
			wts[i] *= (0.9 - 0.6*cov) * prefScale
		}
	}
	scale := s.tunableF("fish_weight_scale", 1)
	if scale <= 0 {
		scale = 1
	}
	capG := 0.0
	// gearCapKg 是上层按开关计算好的有效上限；0 表示完全不截断。
	if gearCapKg > 0 {
		capG = gearCapKg * 1000
		if capG < 1 {
			return nil
		}
	}
	for range len(keys) {
		if !slices.ContainsFunc(wts, func(w float64) bool { return w > 0 }) {
			return nil
		}
		key := weightedChoice(keys, wts)
		sample := func() float64 {
			return float64(s.sampleWeightDB(cctx, key, s.baitWeightModeForRoll(key, c.baits, c), scale, capG, c))
		}
		w := int64(applyTrophyLuck(sample(), bonus.TrophyLuck, sample))
		if w == 0 {
			for i, candidate := range keys {
				if candidate == key {
					wts[i] = 0
				}
			}
			continue
		}
		interest := 1.0
		if strict {
			interest = interests[key]
		}
		if c.forRig && !c.acceptHappyHourBite(fishingBaitInterest(interest, bonus), s.weatherFishActivity(key, c)) {
			return nil
		}
		grade := s.rollGradeForRoll(cctx, key, w, c)
		lk, lp := s.cachedSpeciesLengthKP(cctx, c, key)
		length := lk * math.Pow(float64(w), lp)
		length = math.Round(length*100000) / 100000
		f := &RolledFish{
			FishGUID: newFishGUID(),
			Species:  key,
			Grade:    grade,
			WeightG:  w,
			LengthM:  length,
		}
		if baitHit {
			s.log.Info("roll_from_db 命中饵效", "鱼种", key, "重量", w, "tier", tiers[key])
		}
		return f
	}
	return nil
}

// companionTierMult 将饵效档位转换为权重倍率。
func companionTierMult(tier int64) float64 {
	switch tier {
	case 3:
		return 8.0
	case 2:
		return 4.0
	case 1:
		return 2.0
	}
	return 1.0
}

// baitWeightMode 沿用饵效档位偏移峰值，组合饵取最强证据，缺少证据时保持原分布。
func (s *Server) baitWeightMode(species string, baits []string) float64 {
	return s.baitWeightModeForRoll(species, baits, fishRollContext{})
}

func (s *Server) baitWeightModeForRoll(species string, baits []string, c fishRollContext) float64 {
	if s.DB == nil || s.tunableF("bait_pref_scale", 1) <= 0 {
		return 1
	}
	shift := s.tunableF("bait_pref_weight_shift", 1)
	if shift == 0 {
		return 1
	}
	tier := int64(0)
	seen := map[string]bool{}
	for _, bait := range baits {
		if bait == "" || seen[bait] {
			continue
		}
		seen[bait] = true
		tier = max(tier, s.cachedBaitTiers(s.dbCtx(), c, bait)[species])
	}
	if tier < 2 {
		return 1
	}
	mode := 1 + 0.12*float64(tier)*shift
	if mode <= 0 || math.IsNaN(mode) || math.IsInf(mode, 0) {
		return 1
	}
	return mode
}

// weightSkewK 右偏钟形的右尾拉伸系数。
// 过大会把绝大多数体重压到平均值附近；1.6 能让小、中、大个体分布更明显。
const weightSkewK = 1.6

// sampleWeightDB 按鱼种自然体重分布采样。
// 在自然体重范围与装备上限的交集中按对数正态分布采样。
func (s *Server) sampleWeightDB(ctx context.Context, key string, modeMult, scale, capG float64, c fishRollContext) int64 {
	lines := s.cachedGradeLines(ctx, c, key)
	val, tro, sup := lines.val, lines.trophy, lines.blue
	var vf, trof, supf float64
	if val != nil {
		vf = float64(*val)
	}
	if tro != nil {
		trof = float64(*tro)
	}
	if sup != nil {
		supf = float64(*sup)
	}
	// 控制台可配置的档位比例（未达标/合格/奖杯/蓝冠）优先；
	// 抽样与最终判档必须共用 settleStatics 同一真源，避免鱼档占比失真。
	if s.tunableF("grade_ratio_enabled", 0) == 1 {
		settle := s.cachedSettleStatics(ctx, c, key)
		ratioLines := cachedGradeLines{val: &settle[0], trophy: &settle[1], blue: &settle[2]}
		if w, ok := s.sampleWeightByGradeRatio(ratioLines, modeMult, scale, capG); ok {
			return w
		}
	}
	// 峰值 M：达标带中点（缺 val→tro*0.6→sup*0.3→250）
	M := 250.0
	switch {
	case val != nil && tro != nil && trof > vf:
		M = (vf + trof) / 2.0
	case tro != nil:
		M = trof * 0.6
	case sup != nil:
		M = supf * 0.3
	}
	if modeMult != 1.0 {
		M *= modeMult // 饵效命中：峰值锚点向奖杯线右移
	}
	// 整体峰值向右上抬 15%，让日常鱼获里更容易出现中大个体。
	M *= 1.15
	// 上限 hi：蓝冠线×1.25（缺 sup→tro*1.45→M*6.0）
	var hi float64
	switch {
	case sup != nil:
		hi = supf * 1.25
	case tro != nil:
		hi = trof * 1.45
	default:
		hi = M * 6.0
	}
	// 下限 lo：少量"太小/不达标"个体
	lo := M * 0.12
	if val != nil {
		lo = math.Min(lo, vf*0.2)
	}
	lo = math.Max(1.0, math.Min(lo, M*0.6))
	if hi <= M {
		hi = M * 1.6
	}
	hi = math.Max(hi, lo+1.0)
	sd := math.Max(0.20, (math.Log(hi)-math.Log(M))/weightSkewK)
	mu := math.Log(M) + sd*sd
	if capG > 0 {
		hi = math.Min(hi, capG/scale)
	}
	if hi <= lo {
		return 0
	}
	logLo, logHi := math.Log(lo), math.Log(hi)
	peak := math.Max(logLo, math.Min(logHi, mu))
	peakZ := (peak - mu) / sd
	for range 128 {
		x := logLo + rand.Float64()*(logHi-logLo)
		z := (x - mu) / sd
		if rand.Float64() <= math.Exp((peakZ*peakZ-z*z)/2) {
			scaled := math.Max(1, math.Round(math.Exp(x)*scale))
			if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(math.MaxInt64) {
				continue
			}
			if capG <= 0 || scaled <= capG {
				return int64(scaled)
			}
		}
	}
	return 0
}

// rollGradeDB 根据重量和 species_weight 的三个阈值确定鱼档位。
// trophy_g 是达标线，trophy_line_g 是奖杯线，super_trophy_line_g 是蓝冠线；
// 缺少某个阈值时只使用已有的分界。
func (s *Server) rollGradeDB(ctx context.Context, key string, weightG int64) int64 {
	lines := s.settleStatics(ctx, key)
	return gradeFromLines(&lines[0], &lines[1], &lines[2], weightG)
}

// gradeRatioBand 一个档位的重量区间（g），下界含、上界开。
type gradeRatioBand struct {
	grade  int64
	lo, hi float64
}

// sampleWeightByGradeRatio 按控制台配置的档位比例直接抽样体重。
// 四个比例来自 tunables（grade_pct_below/qualified/trophy/super）；
// 鱼种缺某一档阈值时，其份额按其余可用档位归一化。
// 返回 (0,false) 表示未启用或没有可用档位，调用方退回对数正态标定；
// 返回 (0,true) 表示当前钓组够不到该鱼种，按空竿处理。
func (s *Server) sampleWeightByGradeRatio(l cachedGradeLines, modeMult, scale, capG float64) (int64, bool) {
	if s.tunableF("grade_ratio_enabled", 0) != 1 {
		return 0, false
	}
	// 缩放参数不可表示（NaN/Inf/<=0）时按"不可钓"处理，避免溢出成 MinInt64。
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale <= 0 {
		return 0, true
	}
	var vf, trof, supf float64
	if l.val != nil {
		vf = float64(*l.val)
	}
	if l.trophy != nil {
		trof = float64(*l.trophy)
	}
	if l.blue != nil {
		supf = float64(*l.blue)
	}
	lo := 1.0
	bands := make([]gradeRatioBand, 0, 4)
	if vf > lo {
		bands = append(bands, gradeRatioBand{1, lo, vf}) // 未达标
	}
	qLo, qHi := lo, 0.0
	if vf > 0 {
		qLo = vf
	}
	switch {
	case trof > 0:
		qHi = trof
	case supf > 0:
		qHi = supf * 0.98
	default:
		qHi = qLo * 6
	}
	if qHi > qLo {
		bands = append(bands, gradeRatioBand{2, qLo, qHi}) // 合格
	}
	if trof > 0 && supf > trof {
		bands = append(bands, gradeRatioBand{3, trof, supf}) // 奖杯
	}
	if supf > 0 {
		bands = append(bands, gradeRatioBand{4, supf, supf * 1.15}) // 蓝冠
	}
	if len(bands) == 0 {
		return 0, false
	}
	// 装备上限远低于该鱼种奖杯线（< 7.2%，与旧标定 lo=M*0.12 同量级）时，
	// 认为该鱼种在当前钓组下不可钓，交给上层换鱼种或空竿。
	if capG > 0 {
		ref := trof
		if ref <= 0 {
			ref = supf
		}
		if ref > 0 && capG/scale < ref*0.072 {
			return 0, true
		}
	}
	w := map[int64]float64{
		1: s.tunableF("grade_pct_below", 3),
		2: s.tunableF("grade_pct_qualified", 87.5),
		3: s.tunableF("grade_pct_trophy", 9),
		4: s.tunableF("grade_pct_super", 0.5),
	}
	// 饵效命中：把一部分"合格"份额挪给奖杯，模拟大饵更招大鱼。
	if modeMult > 1.0 {
		shift := math.Min(w[2]*0.5, w[2]*(modeMult-1)*0.5)
		w[2] -= shift
		w[3] += shift
	}
	usable := make([]gradeRatioBand, 0, len(bands))
	wts := make([]float64, 0, len(bands))
	total := 0.0
	for _, b := range bands {
		if capG > 0 && capG/scale < b.lo*0.999 {
			continue // 装备上限够不到该档起点
		}
		if w[b.grade] <= 0 {
			continue
		}
		usable = append(usable, b)
		wts = append(wts, w[b.grade])
		total += w[b.grade]
	}
	if len(usable) == 0 || total <= 0 {
		return 0, false
	}
	r := rand.Float64() * total
	idx := len(usable) - 1
	for i, ww := range wts {
		if r < ww {
			idx = i
			break
		}
		r -= ww
	}
	b := usable[idx]
	a, z := b.lo, b.hi
	if a < 1 {
		a = 1
	}
	if capG > 0 && z > capG/scale {
		z = capG / scale
	}
	var base float64
	if z > a*1.0001 {
		base = math.Exp(math.Log(a) + rand.Float64()*(math.Log(z)-math.Log(a)))
	} else {
		base = a
	}
	val := math.Max(1, math.Round(base*scale))
	if capG > 0 && val > capG {
		val = math.Round(capG)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) || val >= float64(math.MaxInt64) {
		return 0, true
	}
	return int64(val), true
}

func (s *Server) rollGradeForRoll(ctx context.Context, key string, weightG int64, c fishRollContext) int64 {
	lines := s.cachedSettleStatics(ctx, c, key)
	return gradeFromLines(&lines[0], &lines[1], &lines[2], weightG)
}

// gradeFromLines 根据重量和档位线判定档位。
// trophy 与 super 重合时不存在 3 档区间。
func gradeFromLines(val, tro, sup *int64, weightG int64) int64 {
	if tro != nil && sup != nil && *sup <= *tro {
		sup = nil
	}
	if val != nil && weightG < *val {
		return 1
	}
	if tro != nil && weightG < *tro {
		return 2
	}
	if sup != nil && weightG < *sup {
		return 3
	}
	if val == nil && tro == nil && sup == nil {
		return 1
	}
	if tro == nil {
		return 2
	}
	if sup == nil {
		return 3
	}
	return 4
}

// weightedChoice 按权重选择鱼种。
func weightedChoice(keys []string, wts []float64) string {
	total := 0.0
	for _, w := range wts {
		if w > 0 {
			total += w
		}
	}
	if total <= 0 {
		return keys[rand.Intn(len(keys))]
	}
	r := rand.Float64() * total
	var acc float64
	for i, w := range wts {
		if w <= 0 {
			continue
		}
		acc += w
		if r <= acc {
			return keys[i]
		}
	}
	return keys[len(keys)-1]
}

// rollFishPools 在静态鱼库无数据时使用内置鱼池。
func (s *Server) rollFishPools(loc string) *RolledFish {
	return s.rollFishPoolsWithRules(loc, 0, fishingSkillBonus{BiteSpeed: 1})
}

func (s *Server) rollFishPoolsWithRules(loc string, gearCapKg float64, bonus fishingSkillBonus, contexts ...fishRollContext) *RolledFish {
	c := rollContext(contexts)
	c.loc, c.skill = loc, bonus
	s.prepareFishRollRuntime(s.dbCtx(), &c, loc)
	strict := s.tunableF("fish_bait_rules_enabled", 0) == 1
	pool := fishPools[loc]
	if len(pool) == 0 {
		pool = defaultFishPool
	}
	if s.tunableF("fish_distribution_enabled", 0) == 1 {
		return nil
	}
	scale := s.tunableF("fish_weight_scale", 1)
	if scale <= 0 {
		scale = 1
	}
	capG := 0.0
	// 同上：没有生效上限（gearCapKg<=0）时不截断，鱼池按原分布投放。
	if gearCapKg > 0 {
		capG = poolWeightCapG(gearCapKg, bonus.FightTolerance)
		if capG > 0 {
			eligible := make([]fishPoolEntry, 0, len(pool))
			for _, entry := range pool {
				if float64(entry.minG)*scale < capG {
					eligible = append(eligible, entry)
				}
			}
			pool = eligible
			if len(pool) == 0 {
				return nil
			}
		}
	}
	keys := make([]string, len(pool))
	wts := make([]float64, len(pool))
	for i, p := range pool {
		keys[i] = p.species
		wts[i] = s.fishBiteEnvironmentWeight(s.dbCtx(), p.species, c)
	}
	if !slices.ContainsFunc(wts, func(w float64) bool { return w > 0 }) {
		return nil
	}
	interest := 1.0
	if strict && c.baits != nil {
		keys, wts = s.filterFishBaitSet("", c.baits, keys, wts, c.now, c)
		if len(keys) == 0 {
			return nil
		}
	}
	key := weightedChoice(keys, wts)
	if strict {
		for i := range keys {
			if keys[i] == key {
				interest = math.Min(1, wts[i]/maxBaitPreference)
				break
			}
		}
	}
	e := pool[0]
	for _, p := range pool {
		if p.species == key {
			e = p
			break
		}
	}
	if c.forRig && !c.acceptHappyHourBite(fishingBaitInterest(interest, bonus), s.weatherFishActivity(e.species, c)) {
		return nil
	}
	// 饵效移动自然峰值，装备只限制可采样的重量范围。
	minG, maxG := float64(e.minG), float64(e.maxG)
	mode := minG + (maxG-minG)*0.3
	mode = math.Max(minG, math.Min(maxG, mode*s.baitWeightModeForRoll(e.species, c.baits, c)))
	upper := maxG
	if capG > 0 {
		upper = math.Min(upper, capG/scale)
	}
	sample := func() float64 { return triangular(minG, maxG, mode, upper) }
	w := applyTrophyLuck(sample(), bonus.TrophyLuck, sample)
	if w == 0 {
		return nil
	}
	scaled := math.Max(1, math.Round(w*scale))
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(math.MaxInt64) || (capG > 0 && scaled > capG) {
		return nil
	}
	weightG := int64(scaled)
	// 有 species_weight 档位线时按重量判档；缺表时使用内置 valuable 线。
	grade := s.poolGradeForRoll(e, weightG, c)
	// 体长
	lp, ok := lengthSeed[e.species]
	if !ok || lp[0] == 0 {
		lp = [2]float64{defaultSpeciesLengthK, defaultSpeciesLengthP}
	}
	lp[0], lp[1] = calibratedLengthKP(e.species, lp[0], lp[1])
	length := lp[0] * math.Pow(float64(weightG), lp[1])
	length = math.Round(length*100000) / 100000

	return &RolledFish{
		FishGUID: newFishGUID(),
		Species:  e.species,
		Grade:    grade,
		WeightG:  weightG,
		LengthM:  length,
	}
}

func poolWeightCapG(gearCapKg, fightTolerance float64) float64 {
	if gearCapKg > 0 {
		return gearCapKg * 1000
	}
	return float64(maxMockWeight) * (1.0 + math.Max(0, fightTolerance))
}

// applyTrophyLuck 按技能概率将鱼提升一档，最高为 4 档；无加成时保持原档位。
func applyTrophyLuck(weight, luck float64, sample func() float64) float64 {
	if luck > 0 && rand.Float64() < luck {
		return math.Max(weight, sample())
	}
	return weight
}

func triangular(min, max, mode float64, upperBounds ...float64) float64 {
	r := rand.Float64()
	if len(upperBounds) > 0 && upperBounds[0] < max {
		upper := upperBounds[0]
		if upper <= min {
			return 0
		}
		var cdf float64
		if upper <= mode {
			cdf = (upper - min) * (upper - min) / ((max - min) * (mode - min))
		} else {
			cdf = 1 - (max-upper)*(max-upper)/((max-min)*(max-mode))
		}
		r *= cdf
	}
	if r < (mode-min)/(max-min) {
		return min + math.Sqrt(r*(max-min)*(mode-min))
	}
	return max - math.Sqrt((1-r)*(max-min)*(max-mode))
}

// poolGrade 优先使用 species_weight 档位线；缺表时使用内置池的 valuable 线。
func (s *Server) poolGrade(e fishPoolEntry, weightG int64) int64 {
	return s.rollGradeDB(s.dbCtx(), e.species, weightG)
}

func (s *Server) poolGradeForRoll(e fishPoolEntry, weightG int64, c fishRollContext) int64 {
	return s.rollGradeForRoll(s.dbCtx(), e.species, weightG, c)
}

// newFishGUID 随机 16B hex，第 13 个 nibble 固定 '9'。
func newFishGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := fmt.Sprintf("%x", b[:])
	h = h[:12] + "9" + h[13:]
	return h
}

// ---- 14/14 判鱼推送 ----

// officialSpeciesStatics 保存 f9 到 f20 的鱼种静态参数，重量单位为克。
type fishStatics struct {
	f13 map[int64]float64
	f14 map[int64]float64
	f15 map[int64]float64
	f17 map[int64]float64
	f20 float64
	f9  [2]float64 // A, p
	f10 [2]float64
	f11 [2]float64
	f19 [2]float64
}

var officialSpeciesStatics = map[string]*fishStatics{
	"bream": {
		f13: map[int64]float64{2: 0.35}, f14: map[int64]float64{2: 2.0}, f15: map[int64]float64{2: 50.0},
		f17: map[int64]float64{2: 0.8406126}, f20: 0.9978698,
		f9: [2]float64{0.159245, 0.2656}, f10: [2]float64{0.153964, 0.2191},
		f11: [2]float64{0.0991065, 0.2604}, f19: [2]float64{1.60587e-05, 0.7986},
	},
	"c.carp": {
		f13: map[int64]float64{1: 0.25, 2: 0.35}, f14: map[int64]float64{1: 0.5, 2: 0.5},
		f15: map[int64]float64{1: 4.0, 2: 4.0}, f17: map[int64]float64{1: 0.8406168, 2: 0.8406168},
		f20: 0.9978698,
		f9:  [2]float64{0.485972, 0.1722}, f10: [2]float64{0.215219, 0.2493},
		f11: [2]float64{0.227746, 0.1307}, f19: [2]float64{3.18019e-05, 0.7000},
	},
	"c.roach": {
		f13: map[int64]float64{1: 0.30, 2: 0.15}, f14: map[int64]float64{1: 0.6, 2: 0.7},
		f15: map[int64]float64{1: 3.0, 2: 15.0}, f17: map[int64]float64{1: 0.8406168, 2: 0.8406168},
		f20: 0.9978698,
		f9:  [2]float64{0.414243, 0.2656}, f10: [2]float64{0.369253, 0.2191},
		f11: [2]float64{0.139494, 0.2604}, f19: [2]float64{1.78540e-05, 0.7986},
	},
	"crucian": {
		f13: map[int64]float64{1: 0.25, 2: 0.35}, f14: map[int64]float64{1: 0.5, 2: 0.5},
		f15: map[int64]float64{1: 4.0, 2: 4.0}, f17: map[int64]float64{1: 0.8406168, 2: 0.8406168},
		f20: 0.9978698,
		f9:  [2]float64{0.452706, 0.1941}, f10: [2]float64{0.233332, 0.2355},
		f11: [2]float64{0.206706, 0.1482}, f19: [2]float64{3.18023e-05, 0.7000},
	},
	"e.chub": {
		f13: map[int64]float64{1: 0.35, 2: 0.45}, f14: map[int64]float64{1: 0.6, 2: 0.6},
		f15: map[int64]float64{1: 6.0, 2: 6.0}, f17: map[int64]float64{1: 0.8406126, 2: 0.7030094},
		f20: 0.9978698,
		f9:  [2]float64{2.77068, 0.0700}, f10: [2]float64{0.725204, 0.1019},
		f11: [2]float64{0.27588, 0.1533}, f19: [2]float64{1.01401e-04, 0.5211},
	},
	"perch": {
		f13: map[int64]float64{1: 0.15, 2: 0.24}, f14: map[int64]float64{1: 0.4, 2: 0.5},
		f15: map[int64]float64{1: 40.0, 2: 40.0}, f17: map[int64]float64{1: 0.8995978, 2: 0.8995978},
		f20: 0.9979957,
		f9:  [2]float64{0.589176, 0.2371}, f10: [2]float64{0.182802, 0.2936},
		f11: [2]float64{0.202101, 0.2187}, f19: [2]float64{3.18016e-05, 0.7000},
	},
	"s.bream": {
		f13: map[int64]float64{2: 0.30}, f14: map[int64]float64{2: 1.2}, f15: map[int64]float64{2: 13.0},
		f17: map[int64]float64{2: 0.8406126}, f20: 0.9978698,
		f9: [2]float64{0.160663, 0.2656}, f10: [2]float64{0.119378, 0.2191},
		f11: [2]float64{0.0792359, 0.2604}, f19: [2]float64{1.77994e-05, 0.7986},
	},
}

// staticPow 计算 A*w^p，重量单位为克。
func staticPow(ap [2]float64, w float64) float64 {
	return ap[0] * math.Pow(w, ap[1])
}

// 未知鱼种使用的搏鱼静态参数和体重校准表。
var fallbackFightStatic = map[int]float64{
	9: 1.536152, 10: 1.028922, 11: 0.514461, 12: 0.0,
	13: 0.35, 14: 0.5, 15: 4.0, 16: 0.833907,
	17: 0.830302, 18: 1.0, 19: 0.002332, 20: 0.997804,
}

var fallbackCalWeights = []float64{94, 189, 212, 255, 331, 405}
var fallbackCalFields = map[int][]float64{
	9:  {1.3414, 1.6466, 1.7205, 1.8586, 2.0138, 2.0467},
	10: {0.9795, 1.1512, 1.1928, 1.2705, 1.3586, 1.3792},
	11: {0.4442, 0.5396, 0.5627, 0.6058, 0.6552, 0.6675},
	17: {0.6936, 0.8406, 0.8406, 0.8406, 0.8406, 0.8406},
	19: {0.0006, 0.0012, 0.0014, 0.0015, 0.0018, 0.0021},
}

func fallbackFightStatics(weightG float64) map[int]float64 {
	out := make(map[int]float64, len(fallbackFightStatic))
	for k, v := range fallbackFightStatic {
		out[k] = v
	}
	for idx, ys := range fallbackCalFields {
		if weightG <= fallbackCalWeights[0] {
			out[idx] = ys[0]
			continue
		}
		if weightG >= fallbackCalWeights[len(fallbackCalWeights)-1] {
			out[idx] = ys[len(ys)-1]
			continue
		}
		for j := 1; j < len(fallbackCalWeights); j++ {
			if weightG <= fallbackCalWeights[j] {
				x0, x1 := fallbackCalWeights[j-1], fallbackCalWeights[j]
				y0, y1 := ys[j-1], ys[j]
				out[idx] = y0 + (y1-y0)*(weightG-x0)/(x1-x0)
				break
			}
		}
	}
	return out
}

// gradeMapVal 读取指定档位的值，缺失时使用表中已有档位。
func gradeMapVal(m map[int64]float64, grade int64) float64 {
	if v, ok := m[grade]; ok {
		return v
	}
	for _, g := range []int64{1, 2, 3, 4} {
		if v, ok := m[g]; ok {
			return v
		}
	}
	return 0
}

// buildFish27 构造 27 判鱼对象。
func (s *Server) fishStaminaFor(f *RolledFish) float64 {
	return s.fishStaminaForProfile(f, s.fishFightProfileFor(f))
}

func (s *Server) fishStaminaForProfile(f *RolledFish, profile fishFightProfile) float64 {
	if f == nil {
		return 0
	}
	stamina := f.Stamina
	if stamina <= 0 {
		stamina = verifiedStaminaPerGram * float64(f.WeightG)
	}
	scale := s.tunableF("fish_stamina_scale", defaultFishStaminaScale)
	if scale > 0 {
		stamina *= scale
	}
	return stamina * profile.StaminaScale
}

func (s *Server) fishFightPullFor(f *RolledFish, profile fishFightProfile) float64 {
	if f == nil {
		return 0
	}
	pull := f.Force
	if pull <= 0 {
		pull = verifiedForcePerGram * float64(f.WeightG)
	}
	if k := s.tunableF("fight_power_scale", 1.0); k > 0 && k != 1.0 {
		pull = math.Max(0.5, math.Round(pull*k*10000)/10000)
	}
	return math.Max(0.5, math.Round(pull*profile.ForceScale*10000)/10000)
}

func (s *Server) fishInitialStaminaRatio() float64 {
	ratio := s.tunableF("fish_initial_stamina_ratio", defaultFishInitialStaminaRatio)
	if ratio <= 0 {
		return defaultFishInitialStaminaRatio
	}
	return math.Max(0.05, math.Min(1.0, ratio))
}

func (s *Server) buildFish27(f *RolledFish, hookGUID, gearGuid string) *proto.Value {
	w := float64(f.WeightG)
	st := officialSpeciesStatics[f.Species]
	var f9, f10, f11, f12v, f13v, f14v, f15v, f16v, f17v, f18v, f19v, f20v float64
	if st != nil {
		f9 = staticPow(st.f9, w)
		f10 = staticPow(st.f10, w)
		f11 = staticPow(st.f11, w)
		f13v = gradeMapVal(st.f13, f.Grade)
		f14v = gradeMapVal(st.f14, f.Grade)
		f15v = gradeMapVal(st.f15, f.Grade)
		f12v = 0
		f16v = 1
		f17v = gradeMapVal(st.f17, f.Grade)
		f18v = 1
		f19v = staticPow(st.f19, w)
		f20v = st.f20
	} else {
		fb := fallbackFightStatics(w)
		f9, f10, f11 = fb[9], fb[10], fb[11]
		f12v, f13v, f14v, f15v = fb[12], fb[13], fb[14], fb[15]
		f16v, f17v, f18v = fb[16], fb[17], fb[18]
		f19v, f20v = fb[19], fb[20]
	}
	profile := s.fishFightProfileFor(f)
	stamina := s.fishStaminaForProfile(f, profile)
	initialStamina := s.fishInitialStaminaRatio()
	pull := s.fishFightPullFor(f, profile)

	var named *string
	name := f.Name
	if name != "" {
		named = &name
	}
	if named == nil && f.NamedID != "" {
		if nf, err := s.DB.NamedFishGet(s.dbCtx(), f.NamedID); err == nil && nf != nil {
			named = &nf.Name
		}
	}
	hook := hookGUID
	if hook == "" {
		// 线末端钩缺失时，27.f23 回退到鱼自身 GUID；
		// 客户端用 f23 查找鱼实体所属部件。
		hook = f.FishGUID
	}

	return wiregen.MustMakeObj("fgogccpglhb", map[int]*proto.Value{
		0:  proto.Guid(f.FishGUID),
		1:  proto.StrVal(f.Species),
		2:  proto.U32(uint32(f.Grade)),
		3:  proto.F32(float32(f.LengthM)),
		4:  proto.U32(uint32(f.WeightG)),
		5:  proto.F32(float32(stamina)),
		6:  proto.F32(float32(initialStamina)),
		8:  proto.F32(float32(pull)),
		9:  proto.F32(float32(f9)),
		10: proto.F32(float32(f10)),
		11: proto.F32(float32(f11)),
		12: proto.F32(float32(f12v)),
		13: proto.F32(float32(f13v)),
		14: proto.F32(float32(f14v)),
		15: proto.F32(float32(f15v)),
		16: proto.F32(float32(f16v)),
		17: proto.F32(float32(f17v)),
		18: proto.F32(float32(f18v)),
		19: proto.F32(float32(f19v)),
		20: proto.F32(float32(f20v)),
		21: s.buildFightCurvesForFish(f, pull, profile),
		22: proto.Str(named),
		23: proto.Guid(hook),
		24: proto.Null(),
	})
}

// ---- 搏鱼曲线（fight_curves_sets.json）----

// Push14_14 发判鱼推送：args("524")[guid(钓组), 27]。
func (s *Session) Push14_14(f *RolledFish, hookGUID, gearGuid string) bool {
	if f == nil {
		return false
	}
	// 同一尾鱼短时间内重复推送会让客户端重建鱼实体，出现"溜的是一条、结算是另一条"。
	if !s.claimFishPush(f.FishGUID) {
		s.log.Info("忽略重复的 14/14 推送", "guid", shortGUID(f.FishGUID))
		return false
	}
	if gearGuid == "" {
		gearGuid = "00000000000000000000000000000000"
	}
	profile := s.Srv.fishFightProfileFor(f)
	stamina := s.Srv.fishStaminaForProfile(f, profile)
	initialStamina := s.Srv.fishInitialStaminaRatio()
	force := s.Srv.fishFightPullFor(f, profile)
	s.log.Info("14/14 发鱼",
		"rod", shortGUID(gearGuid), "fish", shortGUID(f.FishGUID), "hook", shortGUID(hookGUID),
		"鱼种", f.Species, "重量_g", f.WeightG, "体力池", stamina,
		"初始体力", initialStamina, "拉力", force, "体型比例", profile.SizeRatio,
		"冲刺倍率", profile.ForceScale, "耐力倍率", profile.StaminaScale)
	return s.Push(14, 14, proto.Args524(
		proto.Guid(gearGuid),
		s.Srv.buildFish27(f, hookGUID, gearGuid),
	)) != 0
}

// marineHookConfirm 海钓额外挂钩推送开关，tunables 键 marine_hook_confirm。
// 真服成功抓包只有 14/14 -> 14/11 -> 14/8，没有 14/15 或中钩时的 4/22。
// 这两条额外推送会改变客户端鱼实体状态，默认关闭，只保留已证实的协议路径。
func (s *Session) marineHookConfirm() bool {
	return s.Srv.tunableF("marine_hook_confirm", 0) != 0
}

// marineHookItemSync 是否在 14/11/14/9 后补发单钩 4/22。
// 真服抓包里没有单独的挂钩 4/22，但私服的 14/1/2 钓组树可能缺少客户端
// 启动搏鱼所需的 mounted hook，因此单独保留开关做实测。
func (s *Session) marineHookItemSync() bool {
	return s.Srv.tunableF("marine_hook_item_sync", 0) != 0
}

// Push14_15 确认海钓鱼实体与当前钓组的挂钩关系。
// 客户端契约为 args("524")[钓组 GUID, 鱼 GUID]；缺少该推送时海钓右键抬竿
// 虽会上报 14/9，但鱼不会进入搏鱼状态，力度条也不会激活。
func (s *Session) Push14_15(gearGUID, fishGUID string) bool {
	if s == nil || gearGUID == "" || fishGUID == "" {
		return false
	}
	return s.Push(14, 15, proto.Args524(
		proto.Guid(gearGUID),
		proto.Guid(fishGUID),
	)) != 0
}

// fishPushDedupeWindow 同一尾鱼重复推送的抑制窗口。
const fishPushDedupeWindow = 5 * time.Second

// claimFishPush 同一 FishGUID 在窗口内只放行一次推送；空 GUID 不抑制。
func (s *Session) claimFishPush(guid string) bool {
	if guid == "" {
		return true
	}
	s.fishPushMu.Lock()
	defer s.fishPushMu.Unlock()
	if s.fishPushGUID == guid && time.Since(s.fishPushAt) < fishPushDedupeWindow {
		return false
	}
	s.fishPushGUID = guid
	s.fishPushAt = time.Now()
	return true
}

// scheduleBite 排咬钩定时器（重抛作废上一杆）。
func (s *Server) scheduleBite(sess *Session, st *FishState) bool {
	return s.scheduleBiteState(sess, st, false, true)
}

// scheduleBiteForHooked 在 14/14 尚未到达时补排判鱼，保留 Hooked 和原 CastTs，
// 不按新抛竿重置落水保护。
func (s *Server) scheduleBiteForHooked(sess *Session, st *FishState) {
	s.scheduleBiteState(sess, st, true, false)
}

func (s *Server) scheduleBiteState(sess *Session, st *FishState, preserveHooked, resetCast bool) bool {
	st.finishMu.Lock()
	defer st.finishMu.Unlock()
	now := time.Now()
	bonus := s.fishingBonus(sess.pushCtx(), sess.Userid, st.RigType, st.GearGuid)
	st.mu.Lock()
	if completedCatchPending(st) {
		st.mu.Unlock()
		return false
	}
	// 14/7 的“补排咬钩”只用于 14/14 尚未到达的场景。若当前鱼已经
	// Pending、已推送或正在推送，绝不能再清空状态重排，否则会把第一条鱼
	// 覆盖成新鱼，几秒后客户端收到第二条 14/14。
	if !resetCast && (st.Pending != nil || st.PushSent || st.pushing != nil) {
		st.mu.Unlock()
		return false
	}
	st.cancelBiteTimerLocked()
	st.cancelPendingTimerLocked()
	st.castGeneration++
	st.pushing = nil
	st.persist.recoverCompleted = false
	st.CastActive = true
	st.Resolved = false
	st.Pending = nil
	st.hookedChatFishGUID = ""
	st.PendingRepush = 0
	if !preserveHooked {
		st.Hooked = false
	}
	st.PushSent = false
	st.FallSeen = false
	st.RigSettled = false
	st.TickSeen = false
	// 上一竿的磨损账若还没落库（异常退出、断线等），先取走，解锁后补写。
	pendingWear, hasPendingWear := st.reelWear.claimLocked()
	st.reelWear.reset()
	// 抛竿即算“下水使用”：这一竿即使没咬钩，轮内零件也要结算每竿基准与泡水。
	st.reelWear.markRodUsedLocked(st.GearGuid)
	if resetCast || st.CastTs.IsZero() {
		st.CastTs = now
	}
	if resetCast {
		// 新的一杆：还没确认在水中，等 14/7 的 0x400。
		st.InWater = false
		st.NeedBite = false
	}
	st.Bonus = bonus
	delay := s.biteDelayInWeather(st, sess.weatherAt(st.Loc, now), now, bonus)
	s.armBiteTimerLocked(sess.Userid, st, now.Add(time.Duration(delay*float64(time.Second))))
	st.mu.Unlock()
	if hasPendingWear {
		s.applyFightReelWear(s.dbCtx(), sess.Userid, pendingWear, "14/7")
	}
	return true
}

// onBiteTimer 到点：顺延保护检查 → 判鱼 → 推 14/14。
func (s *Server) onBiteTimer(uid int64) {
	st := s.fishingState(uid)
	st.mu.Lock()
	ticket := fishingTimerTicket{st.castGeneration, st.biteTicket}
	st.mu.Unlock()
	s.onBiteTimerTicket(uid, ticket)
}

func (s *Server) onBiteTimerTicket(uid int64, ticket fishingTimerTicket) {
	s.onRodBiteTimerTicket(uid, s.fishingState(uid), ticket)
}

func (s *Server) onRodBiteTimerTicket(uid int64, st *FishState, ticket fishingTimerTicket) {
	sess := s.liveSessionByUID(uid)
	if sess != nil {
		sess.updateHappyHour(time.Now(), false)
	}
	st.mu.Lock()
	if ticket.generation != st.castGeneration || ticket.sequence != st.biteTicket {
		st.mu.Unlock()
		return
	}
	st.cancelBiteTimerLocked()
	if sess == nil {
		// 进程仍活着但玩家断开：保留 CastActive/Pending/BiteAt，下一次
		// stateFor（重连首个业务请求）会按过期时刻立即恢复 timer。
		st.mu.Unlock()
		return
	}
	if !st.CastActive || st.Resolved || st.PushSent || st.pushing != nil {
		st.mu.Unlock()
		return
	}
	if st.Pending != nil {
		f, gear, generation := st.Pending, st.GearGuid, st.castGeneration
		st.mu.Unlock()
		s.pushPendingBite(sess, st, f, gear, generation)
		return
	}
	if !st.InWater {
		// 还没等到 14/7 的“在水中”动画位：先挂起，等它出现再派鱼。
		st.NeedBite = true
		st.mu.Unlock()
		return
	}
	base := st.CastTs
	if st.FallSeen && !st.FallTs.IsZero() {
		base = st.FallTs
	}
	lure := rigTypeLureFamily[st.RigType]
	floor := biteFloorDelay(s.effectiveBiteDelaySettings(st), lure)
	minWait := time.Second
	if lure {
		minWait = 500 * time.Millisecond
	}
	now := time.Now()
	if !base.IsZero() && now.Sub(base) < floor {
		// 落水保护：顺延
		wait := time.Until(base.Add(floor))
		if wait < minWait {
			wait = minWait
		}
		s.armBiteTimerLocked(uid, st, now.Add(wait))
		st.mu.Unlock()
		return
	}
	// 浮钓允许在饵仍下沉时接口；目标深度尚未到达就是“行程”窗口。
	onTheDrop := floatRigTypes[st.RigType] && !st.FallTs.IsZero() &&
		st.effectiveBaitDepthCMLocked(now) < st.DepthCM
	// 钓组就位守门（Motionless 位族）——**路亚跳过**：收线动画族
	// StreamMoving/Twitch/StopAndGo 永远没有 Motionless 位，守门会把每杆
	// 路亚都顺延到 +12s（14/14 全部错过收线窗口）。
	if !lure && !onTheDrop && now.Sub(st.CastTs) < 12*time.Second && st.TickSeen && !st.RigSettled {
		// 顺延 1s 轮询（12s 兜底）
		s.armBiteTimerLocked(uid, st, now.Add(time.Second))
		st.mu.Unlock()
		return
	}
	if st.Armed != nil {
		// GM 武装的"下一杆必上"优先，且绕过两道重量上限（arm_next_fish）
		f := st.Armed
		f.HappyHour, f.HappyHourKnown = sess.currentHappyHour().Active, true
		st.Armed = nil
		f.LocLevel = s.currentLevel(uid)
		f.DepthCM = st.effectiveBaitDepthCMLocked(now)
		// 海钓的扬程由 17/12 设置的钓深表达，LureLayer 模型最大只有 5m。
		if marineRigType(st.RigType) && st.DepthCM > 0 {
			f.DepthCM = st.DepthCM
		}
		f.OnBoat, _ = st.onBoatLocked(now.Unix())
		f.Drift = st.LureMotionMask&lureMotionDriftBit != 0
		f.CastM = st.CastM
		f.IsNight = worldIsNight(now.Unix())
		if !f.BaitsKnown {
			f.BaitGUIDs, f.BaitsKnown = s.biteBaitSnapshot(s.dbCtx(), st.GearGuid)
		}
		f.QualBait = s.qualificationMountedBait(s.dbCtx(), f.BaitGUIDs)
		st.Pending = f
		st.PendingRepush = 0
		gear, generation := st.GearGuid, st.castGeneration
		st.mu.Unlock()
		s.pushPendingBite(sess, st, f, gear, generation)
		return
	}
	if !s.canFishWithBait(s.dbCtx(), uid, st.GearGuid) {
		// 空竿
		st.Pending = nil
		st.Resolved = true
		st.mu.Unlock()
		return
	}
	x, z, hasPos := st.CastPosX, st.CastPosZ, st.HasCastPos
	if !hasPos {
		x, z, hasPos = st.PosX, st.PosZ, st.HasPos
	}
	probability := s.biteProbabilityForLoc(st.Loc)
	if s.tunableF("fish_bait_rules_enabled", 0) == 1 || s.tunableF("fish_weather_enabled", 0) == 1 {
		probability = math.Min(.95, probability)
	}
	// 窝料死窝闸门：同一格子堆超过 15 份开始压开口率，22 份以上不排咬钩。
	probability *= s.groundbaitBiteGate(s.dbCtx(), uid, st.Loc, x, z, hasPos, st.Spot, st.RigType)
	if rand.Float64() >= probability {
		s.log.Info("鱼口未命中，继续等待", "rod", shortGUID(st.GearGuid), "rig_type", st.RigType)
		s.emptyWeatherBiteLocked(sess, st, now)
		st.mu.Unlock()
		return
	}
	baitDepthCM := st.effectiveBaitDepthCMLocked(now)
	rigType := st.RigType
	lureLayer, lureAction, lureMask := st.LureLayer, st.LureAction, st.LureMotionMask
	lureSpeed := st.ReelSetting
	boatOn, boatKnown := st.onBoatLocked(now.Unix())
	f := s.rollFishForRig(s.dbCtx(), uid, st.GearGuid, st.Loc, st.Bait, st.Bonus, fishRollContext{
		x: x, z: z, hasPos: hasPos, spot: st.Spot, depthCM: baitDepthCM,
		lureLayer: lureLayer, lureAction: lureAction, lureMask: lureMask, lureSpeed: lureSpeed, rigType: rigType,
		onBoat: boatOn, boatKnown: boatKnown,
		now: now, weather: sess.weatherAt(st.Loc, now), happyBite: sess.currentHappyHour().BiteBonus,
	})
	if f == nil {
		s.log.Info("本轮无符合条件的鱼，继续等待", "rod", shortGUID(st.GearGuid), "rig_type", st.RigType)
		s.emptyWeatherBiteLocked(sess, st, now)
		st.mu.Unlock()
		return
	}
	// 鱼吃口扣份：窝被吃薄，死窝会自己缓过来（设计稿第 6 节）。
	s.groundbaitConsumeBite(s.dbCtx(), uid, st.Loc, x, z, hasPos, st.Spot, s.tunableF("groundbait_consume_bite", 0.20))
	if !f.BaitsKnown {
		f.BaitGUIDs, f.BaitsKnown = s.biteBaitSnapshot(s.dbCtx(), st.GearGuid)
	}
	f.HappyHour, f.HappyHourKnown = sess.currentHappyHour().Active, true
	f.LocLevel = s.currentLevel(uid)
	f.DepthCM = baitDepthCM
	if marineRigType(rigType) && st.DepthCM > 0 {
		f.DepthCM = st.DepthCM
	}
	f.OnBoat = boatOn
	f.Drift = lureMask&lureMotionDriftBit != 0
	f.CastM = st.CastM
	f.IsNight = worldIsNight(now.Unix())
	f.QualBait = s.qualificationMountedBait(s.dbCtx(), f.BaitGUIDs)
	st.Pending = f
	st.PendingRepush = 0
	gear, generation := st.GearGuid, st.castGeneration
	st.mu.Unlock()

	s.pushPendingBite(sess, st, f, gear, generation)
}

// repushPending14_14 在 14/12 f0=3 时于 6 秒后重推同一尾 pending 鱼，
// 不重新判定，以保持 GUID、重量和曲线不变。
func (s *Server) repushPending14_14(uid int64, expectedFishGUID ...string) {
	st := s.fishingState(uid)
	st.mu.Lock()
	ticket := fishingTimerTicket{st.castGeneration, st.pendingTicket}
	fish := st.Pending
	if len(expectedFishGUID) > 0 && expectedFishGUID[0] != "" && (fish == nil || fish.FishGUID != expectedFishGUID[0]) {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	s.repushPendingTicket(uid, ticket, fish)
}

func (s *Server) repushPendingTicket(uid int64, ticket fishingTimerTicket, fish *RolledFish) {
	s.repushRodPendingTicket(uid, s.fishingState(uid), ticket, fish)
}

func (s *Server) repushRodPendingTicket(uid int64, st *FishState, ticket fishingTimerTicket, fish *RolledFish) {
	sess := s.liveSessionByUID(uid)
	st.mu.Lock()
	if ticket.generation != st.castGeneration || ticket.sequence != st.pendingTicket || st.Pending != fish {
		st.mu.Unlock()
		return
	}
	st.cancelPendingTimerLocked()
	if sess == nil {
		st.mu.Unlock()
		return
	}
	if !st.CastActive || st.Resolved || st.Pending == nil {
		st.mu.Unlock()
		return
	}
	f := st.Pending
	gear, generation := st.GearGuid, st.castGeneration
	st.mu.Unlock()
	s.pushPendingBite(sess, st, f, gear, generation)
}

// pushPendingBite 只在隧道写成功后记为已推送，失败保留原鱼等待重推。
func (s *Server) pushPendingBite(sess *Session, st *FishState, f *RolledFish, gear string, generation uint64) {
	st.mu.Lock()
	if generation != st.castGeneration || !st.CastActive || st.Resolved || st.Pending != f || st.GearGuid != gear || st.pushing != nil {
		st.mu.Unlock()
		return
	}
	st.pushing = f
	if f.GearGUID == "" {
		f.GearGUID = gear
	}
	st.persist.recoverPending = false
	st.mu.Unlock()
	sent := s.pushBaitedFish(sess, f, gear)
	st.mu.Lock()
	defer st.mu.Unlock()
	if generation != st.castGeneration {
		return
	}
	st.pushing = nil
	if !st.CastActive || st.Resolved || st.Pending != f || st.GearGuid != gear {
		return // 发送期间已收竿或重抛，不能覆盖新状态。
	}
	st.PushSent = sent
	if sent {
		if st.persist.sessionID == sess.ID {
			st.persist.recoverPending = false
		}
		return
	}
	st.persist.recoverPending = true
	if !sess.Closed() && st.pendingTimer == nil {
		s.armPendingTimerLocked(sess.Userid, st, 6*time.Second)
	}
}

// speciesZhName 返回鱼种中文名（loc_wiki_fish.fish_zh）；缺名回退 key。
func (s *Server) speciesZhName(ctx context.Context, key string) string {
	if s == nil || s.DB == nil || key == "" {
		return key
	}
	var name string
	err := s.DB.SQL.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(fish_zh,''),?) FROM loc_wiki_fish WHERE fish_key=? LIMIT 1`,
		key, key).Scan(&name)
	if err != nil || name == "" {
		return key
	}
	return name
}

// formatWeightG 将克重格式化为聊天提示用的显示文本。
func formatWeightG(g int64) string {
	if g >= 1000 {
		return fmt.Sprintf("%.2fkg", float64(g)/1000.0)
	}
	return fmt.Sprintf("%dg", g)
}

// gradeZhName 档位中文名（1 普通 / 2 达标 / 3 奖杯 / 4 蓝冠）。
func gradeZhName(grade int64) string {
	switch grade {
	case 2:
		return "达标"
	case 3:
		return "奖杯"
	case 4:
		return "蓝冠"
	}
	return "普通"
}

// fishLandedText 钓获结算提示文本（KV 模板未启用时的降级文案）：
// 达标（Grade 2，超绿旗未到黄旗）及以上、以及破纪录必报；未达标鱼由结算卡
// 展示，聊天不播报。空串表示不播报。
func (s *Server) fishLandedText(f *RolledFish, newRecord bool) string {
	if s == nil || f == nil {
		return ""
	}
	name := s.speciesZhName(s.dbCtx(), f.Species)
	weight := formatWeightG(f.WeightG)
	switch {
	case f.Grade >= 2:
		text := fmt.Sprintf("钓获%s：%s %s", gradeZhName(f.Grade), name, weight)
		if newRecord {
			text += "（新纪录）"
		}
		return text
	}
	return ""
}

// fishKeptText 入护提示文本。
func (s *Server) fishKeptText(f *RolledFish) string {
	if s == nil || f == nil {
		return ""
	}
	return fmt.Sprintf("入护：%s %s", s.speciesZhName(s.dbCtx(), f.Species), formatWeightG(f.WeightG))
}

// fishReleasedText 放生提示文本；奖杯/蓝冠附档位。
func (s *Server) fishReleasedText(f *RolledFish) string {
	if s == nil || f == nil {
		return ""
	}
	name := s.speciesZhName(s.dbCtx(), f.Species)
	weight := formatWeightG(f.WeightG)
	if f.Grade >= 3 {
		return fmt.Sprintf("放生%s：%s %s", gradeZhName(f.Grade), name, weight)
	}
	return fmt.Sprintf("放生：%s %s", name, weight)
}

// fishEscapeText 跑鱼提示文本；f 为 nil 时只提示原因。
func (s *Server) fishEscapeText(f *RolledFish, reason string) string {
	if s == nil {
		return ""
	}
	if f == nil {
		return "跑鱼（" + reason + "）"
	}
	return fmt.Sprintf("跑鱼：%s %s（%s）",
		s.speciesZhName(s.dbCtx(), f.Species), formatWeightG(f.WeightG), reason)
}

// fishAnnouncementKind 返回落地播报使用的聊天模板类型；0 表示不发模板。
// 25=超级稀有（Grade>=4 蓝冠）、26=稀有（Grade 3 奖杯）、3=达标（Grade 2，
// 重量超过绿旗但未到黄旗）；未达标（Grade 1）一律不播报。
func fishAnnouncementKind(grade int64) int {
	switch {
	case grade >= 4:
		return 25
	case grade == 3:
		return 26
	case grade == 2:
		return 3
	}
	return 0
}

// fishLandedKV 构造上鱼播报的 401.f4 参数：键1 为中文鱼名（客户端查不到
// 鱼种 key 时按原样显示），键2 为重量克（客户端按克/公斤显示）。档位前缀
// 由客户端模板补：k25 蓝冠“超级稀有鱼{0},{1}”（深橘红）、k26 奖杯
// “稀有鱼 {0} ， {1}”（橘黄）、kind3 达标无前缀（蓝色）。
func (s *Server) fishLandedKV(f *RolledFish) *proto.Value {
	name := s.speciesZhName(s.dbCtx(), f.Species)
	if name == "" {
		name = f.Species
	}
	return fishTVKv(name, f.WeightG)
}

// notifyFishLanded 播报达标、奖杯和蓝冠；普通鱼由结算卡展示。
func (s *Server) notifyFishLanded(sess *Session, f *RolledFish, newRecord bool) {
	if sess == nil || f == nil {
		return
	}
	// k25/k26/k03 是带参数的官方模板播报，依赖 401.f4 模板参数；KV 未启用
	// （fish_tv_kv=0）时客户端解析模板缺参会抛异常断连（回登录界面）。此时
	// 退化为已验证的普通系统文本播报，保留提示且不掉线。
	kind := fishAnnouncementKind(f.Grade)
	if kind != 0 && s.tunableF("fish_tv_kv", 0) != 0 {
		sess.pushChatProbe(27, kind, "", s.fishLandedKV(f))
		return
	}
	if text := s.fishLandedText(f, newRecord); text != "" {
		sess.pushSystemChat(text)
	}
}

// notifyFishKept 入护播报已永久停用：结算卡已经展示入护结果，
// 聊天里再补一条白色“入护：…”只是重复信息，未达标/达标鱼都不再发。
func (s *Server) notifyFishKept(sess *Session, f *RolledFish) {
	if sess == nil || f == nil {
		return
	}
}

// notifyFishReleased 放生播报。
func (s *Server) notifyFishReleased(sess *Session, f *RolledFish) {
	if sess == nil {
		return
	}
	if text := s.fishReleasedText(f); text != "" {
		sess.pushSystemChat(text)
	}
}

// escapeTemplateKind 跑鱼原因 → 官方聊天模板类型（401.f0）。
// 客户端内置本地化模板：4=鱼线轮损坏 5=鱼竿损坏 6=鱼线拉断
// 7=鱼脱钩 10=牙齿咬断钓组 30=引线(子线)断了。
// 当前跑鱼提示统一改用 kind3 蓝色自由文案（见 notifyFishEscape），
// 本映射保留备查；恢复官方分原因文案时改回对应 pushChatProbe 即可。
var escapeTemplateKind = map[string]int{
	"脱钩离开": 7,
	"断主线":  6, "断子线": 30,
	"断竿":     5,
	"渔线轮超载":  4,
	"钓组组件断裂": 10,
}

// fishEscapeTaunts 跑鱼语气词池：随机抽一条拼进跑鱼提示。
var fishEscapeTaunts = []string{
	"真是FW啊",
	"这都能跑",
	"煮熟的鸭子飞了",
	"手滑了吧",
	"竿子太拉了",
	"鱼比人聪明",
	"白忙活一场",
	"差亿点点",
	"又给跑了",
	"空军预定",
	"打龟佬",
}

// notifyFishEscape 跑鱼提示：类型3（蓝色 KV 模板），文案带随机语气词，
// 重量走键2（客户端模板句尾自动显示“X 克/公斤”）。
// fish_escape_taunt=0 或没有在途鱼体时回退官方原因模板。
func (s *Server) notifyFishEscape(sess *Session, f *RolledFish, reason string) {
	if s == nil || sess == nil {
		return
	}
	officialKind := 7
	if kind, ok := escapeTemplateKind[reason]; ok {
		officialKind = kind
	}
	if f == nil || s.tunableF("fish_escape_taunt", 1) == 0 {
		sess.pushChatProbe(27, officialKind, "", nil)
		return
	}
	weight := int64(0)
	name := ""
	tier := ""
	if f != nil {
		if f.WeightG > 0 {
			weight = f.WeightG
		}
		name = s.speciesZhName(s.dbCtx(), f.Species)
		if name == "" {
			name = f.Species
		}
		if f.Grade >= 4 {
			tier = "超级稀有 "
		} else if f.Grade == 3 {
			tier = "稀有 "
		}
	}
	taunt := fishEscapeTaunts[rand.Intn(len(fishEscapeTaunts))]
	// 客户端 KV 模板恒在文案末尾追加“X 克/公斤”（缺键2 显示“0 克”），
	// 服务端无法把重量插到句子中间。想让重量紧跟鱼名，只能把鱼名放到句尾：
	//   "这都能跑！你的 稀有 丁鱵" + 客户端追加 " 1.5 千克"
	text := "你的 " + tier + name + "，跑掉了，" + taunt + "！"
	sess.pushChatProbe(27, 3, "", fishTVKv(text, weight))
}

// bumpFishingCounter 增加玩家钓鱼计数器（escapes 等），幂等累加。
func (s *Server) bumpFishingCounter(uid int64, key string) {
	if s.DB == nil {
		return
	}
	_, _ = s.DB.SQL.ExecContext(s.dbCtx(), `
		INSERT INTO player_counters(uid, key, value) VALUES(?,?,1)
		ON CONFLICT(uid,key) DO UPDATE SET value=value+1`, uid, key)
}

// liveSessionByUID 取 uid 的存活会话（重连后新连接）。
func (s *Server) liveSessionByUID(uid int64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.Userid == uid && !sess.Closed() {
			return sess
		}
	}
	return nil
}

// currentLevel 玩家当前地图。内存只作热缓存；缺失时从 profile 恢复，
// 进程重启后也不会默认回到 1 图。无效地图统一回退出生图。
func (s *Server) currentLevel(uid int64) string {
	if v, ok := s.levelByUID(uid); ok {
		v = travelLevelKey(v)
		s.setLevel(uid, v)
		return v
	}
	if s.DB != nil {
		if loc, err := s.DB.GetPlayerLocation(s.dbCtx(), uid); err == nil && loc != nil {
			v := travelLevelKey(loc.LevelKey)
			s.setLevel(uid, v)
			return v
		}
	}
	return spawnLevelKey
}

func (s *Server) levelByUID(uid int64) (string, bool) {
	s.levelMu.Lock()
	defer s.levelMu.Unlock()
	v, ok := s.levelMap[uid]
	return v, ok
}

func (s *Server) setLevel(uid int64, level string) {
	s.levelMu.Lock()
	s.levelMap[uid] = level
	s.levelMu.Unlock()
}

var _ = store.KeepnetEntry{}
var _ = wiregen.MakeObj
