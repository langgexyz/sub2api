package service

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 订阅档位标识。容量按 log 空间就近吸附到这些档（倍率相对 Pro 基线）。
const (
	TierUnknown = "unknown"
	TierPro     = "pro"
	TierMax5x   = "max5x"
	TierMax10x  = "max10x"
	TierMax20x  = "max20x"
)

// tierLadder 是相对 Pro 基线的档位倍率阶梯（Pro=1, Max 5x=5, ...）。
var tierLadder = []struct {
	name string
	mult float64
}{
	{TierPro, 1},
	{TierMax5x, 5},
	{TierMax10x, 10},
	{TierMax20x, 20},
}

// account.Extra 中本服务读写的键。写入键统一 inferred_ 前缀，
// 已登记为 scheduler-neutral（见 repository.schedulerNeutralExtraKeyPrefixes），
// 因此写它们绝不触发调度变更——这是本服务「只读观测、零 enforcement 影响」的物理保证。
const (
	extraKeyUtil7d     = "passive_usage_7d_utilization" // 读：上游 7d 占比（0~1）
	extraKeyUtil5h     = "session_window_utilization"   // 读：上游 5h 占比（0~1）
	extraKeyCapacity7d = "inferred_capacity_7d"         // 写：反推周容量（USD）
	extraKeyCapacity5h = "inferred_capacity_5h"         // 写：反推 5h 容量（USD）
	extraKeyTier       = "inferred_tier"                // 写：定档结果
	extraKeyConfidence = "inferred_tier_confidence"     // 写：置信度 0~1
	extraKeyCapUpdated = "inferred_capacity_updated_at" // 写：更新时间 RFC3339
)

// AccountCapacityService 周期性反推每个 Anthropic 订阅账号的周/5h 真实容量
// （窗口成本 ÷ 上游占比），就近吸档，并把估计写回 account.Extra 的 inferred_* 键。
//
// 只读观测：本服务不改任何调度 / 限额 / 计费行为，仅写 scheduler-neutral 的
// inferred_* 键供展示与后续阶段消费。
type AccountCapacityService struct {
	accountRepo  AccountRepository
	usageLogRepo UsageLogRepository
	cfg          config.CapacityInferenceConfig
	interval     time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAccountCapacityService 构造服务。interval<=0 或 cfg.Enabled=false 时 Start 为空操作。
func NewAccountCapacityService(accountRepo AccountRepository, usageLogRepo UsageLogRepository, cfg config.CapacityInferenceConfig) *AccountCapacityService {
	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	return &AccountCapacityService{
		accountRepo:  accountRepo,
		usageLogRepo: usageLogRepo,
		cfg:          cfg,
		interval:     interval,
		stopCh:       make(chan struct{}),
	}
}

func (s *AccountCapacityService) Start() {
	if s == nil || !s.cfg.Enabled || s.accountRepo == nil || s.usageLogRepo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountCapacityService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *AccountCapacityService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		log.Printf("[AccountCapacity] list anthropic accounts failed: %v", err)
		return
	}

	now := time.Now()
	updated := 0
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsAnthropicOAuthOrSetupToken() {
			continue
		}
		if s.processAccount(ctx, acc, now) {
			updated++
		}
	}
	if updated > 0 {
		log.Printf("[AccountCapacity] updated capacity estimates for %d account(s)", updated)
	}
}

// processAccount 反推单账号容量并写回。返回是否写入。
func (s *AccountCapacityService) processAccount(ctx context.Context, acc *Account, now time.Time) bool {
	util7d := acc.getExtraFloat64(extraKeyUtil7d)
	util5h := acc.getExtraFloat64(extraKeyUtil5h)

	updates := map[string]any{}

	// 7d 窗口：反推 + EWMA + 定档。7d 是长期约束，定档以它为准。
	cost7d := s.windowStandardCost(ctx, acc.ID, now.Add(-7*24*time.Hour))
	if cap7dRaw, ok := inferCapacity(cost7d, util7d, s.cfg.MinUtilization); ok {
		prev := acc.getExtraFloat64(extraKeyCapacity7d)
		cap7d := ewmaCapacity(prev, cap7dRaw, s.cfg.EWMAAlpha)
		updates[extraKeyCapacity7d] = cap7d
		updates[extraKeyTier] = snapTier(cap7d, s.cfg.ProBaselineUSD)
		updates[extraKeyConfidence] = capacityConfidence(util7d, s.cfg.FullConfidenceUtil)
	} else {
		// 采样不足，标 unknown 但不动已有容量值。
		updates[extraKeyTier] = TierUnknown
		updates[extraKeyConfidence] = 0.0
	}

	// 5h 窗口：仅反推容量（用于 5h 约束观测），不参与定档。
	cost5h := s.windowStandardCost(ctx, acc.ID, now.Add(-5*time.Hour))
	if cap5hRaw, ok := inferCapacity(cost5h, util5h, s.cfg.MinUtilization); ok {
		prev := acc.getExtraFloat64(extraKeyCapacity5h)
		updates[extraKeyCapacity5h] = ewmaCapacity(prev, cap5hRaw, s.cfg.EWMAAlpha)
	}

	updates[extraKeyCapUpdated] = now.UTC().Format(time.RFC3339)

	if err := s.accountRepo.UpdateExtra(ctx, acc.ID, updates); err != nil {
		log.Printf("[AccountCapacity] update extra failed account_id=%d: %v", acc.ID, err)
		return false
	}
	return true
}

// windowStandardCost 取账号在 [startTime, now) 窗口内的标准成本（total_cost，不含倍率），
// 对齐上游 utilization 的 API 等价口径。取不到时返回 0。
func (s *AccountCapacityService) windowStandardCost(ctx context.Context, accountID int64, startTime time.Time) float64 {
	stats, err := s.usageLogRepo.GetAccountWindowStats(ctx, accountID, startTime)
	if err != nil || stats == nil {
		return 0
	}
	return stats.StandardCost
}

// --- 纯函数（可单测，无副作用）---

// inferCapacity 反推容量 = 窗口成本 / 占比。返回 (0,false) 表示本次无有效信号、应跳过：
//   - 占比非正或低于置信门槛：外推误差过大 / 除零
//   - 成本非正：窗口内无消费 = 无信号（占比快照可能是更早请求的残值，
//     与空成本窗口错配会算出假的 0；跳过以保留上次好值，由 EWMA 平滑过渡）
func inferCapacity(costUSD, utilization, minUtilization float64) (float64, bool) {
	if costUSD <= 0 {
		return 0, false
	}
	if utilization <= 0 || utilization < minUtilization {
		return 0, false
	}
	return costUSD / utilization, true
}

// ewmaCapacity 对容量做指数滑动平均，跟踪上游漂移又抑制单点噪声。
// 无历史值（prev<=0）时直接采用新值。
func ewmaCapacity(prev, raw, alpha float64) float64 {
	if prev <= 0 {
		return raw
	}
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	return alpha*raw + (1-alpha)*prev
}

// snapTier 在 log 空间把容量就近吸附到档位阶梯。baseline=Pro 周容量。
func snapTier(capacity, proBaseline float64) string {
	if capacity <= 0 || proBaseline <= 0 {
		return TierUnknown
	}
	logCap := math.Log(capacity)
	best := TierUnknown
	bestDist := math.MaxFloat64
	for _, t := range tierLadder {
		d := math.Abs(logCap - math.Log(proBaseline*t.mult))
		if d < bestDist {
			bestDist = d
			best = t.name
		}
	}
	return best
}

// capacityConfidence 置信度随观测到的占比上升：占比越高，反推外推越短、越可信。
// 达到 fullConfidenceUtil 即满信心 1.0。
func capacityConfidence(utilization, fullConfidenceUtil float64) float64 {
	if fullConfidenceUtil <= 0 {
		fullConfidenceUtil = 0.5
	}
	c := utilization / fullConfidenceUtil
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}
