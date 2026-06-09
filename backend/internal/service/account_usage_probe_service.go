package service

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// AccountUsageProbeService 周期性对 Anthropic OAuth/SetupToken 账号主动查询上游用量
// （等价于自动执行 /admin/accounts 的「查询上游账号」），把实时 5h/7d utilization 刷进
// passive_usage_* 缓存（GetUsage 内部 syncActiveToPassive）。
//
// 目的：无流量/低流量账号的容量反推与订阅份额也保持新鲜，不再只依赖真实流量的被动采样
// 或人工点「查询」。本服务只负责采样（刷新缓存）；容量反推由 AccountCapacityService 的
// ticker 独立消费，两者解耦。
type AccountUsageProbeService struct {
	accountRepo  AccountRepository
	usageService *AccountUsageService
	cfg          config.AccountUsageProbeConfig
	interval     time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAccountUsageProbeService 构造服务。interval<=0 或 cfg.Enabled=false 时 Start 为空操作。
func NewAccountUsageProbeService(accountRepo AccountRepository, usageService *AccountUsageService, cfg config.AccountUsageProbeConfig) *AccountUsageProbeService {
	return &AccountUsageProbeService{
		accountRepo:  accountRepo,
		usageService: usageService,
		cfg:          cfg,
		interval:     time.Duration(cfg.IntervalMinutes) * time.Minute,
		stopCh:       make(chan struct{}),
	}
}

func (s *AccountUsageProbeService) Start() {
	if s == nil || !s.cfg.Enabled || s.accountRepo == nil || s.usageService == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.probeOnce()
		for {
			select {
			case <-ticker.C:
				s.probeOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountUsageProbeService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *AccountUsageProbeService) probeOnce() {
	// 整轮宽松超时；GetUsage 自身带 jitter + 缓存 + singleflight，账号间天然错峰
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		log.Printf("[AccountUsageProbe] list anthropic accounts failed: %v", err)
		return
	}

	probed := 0
	for i := range accounts {
		acc := &accounts[i]
		if !acc.CanGetUsage() {
			continue
		}
		// 主动查询上游（force=true）；GetUsage 成功后 syncActiveToPassive 把新鲜 utilization 写回缓存
		if _, err := s.usageService.GetUsage(ctx, acc.ID, true); err != nil {
			log.Printf("[AccountUsageProbe] probe account %d (%s) failed: %v", acc.ID, acc.Name, err)
			continue
		}
		probed++
	}
	if probed > 0 {
		log.Printf("[AccountUsageProbe] refreshed upstream usage for %d account(s)", probed)
	}
}
