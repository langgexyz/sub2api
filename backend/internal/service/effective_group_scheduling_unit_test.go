//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// TestEffectiveGroupIDForSchedulingHit 跨组路由命中时选号必须用目标分组。
//
// 这条守的是 P2 部署后真调才炸出来的 bug：网关有两套彼此独立的调度器，OpenAI 兼容
// 路径（含 grok）不经过 resolveGatewayGroup。漏认有效分组的症状是「协议已按目标组
// 交给 OpenAIGateway，选号却仍在源组里找 grok 账号」-> no available accounts。
func TestEffectiveGroupIDForSchedulingHit(t *testing.T) {
	source := int64(1)
	ctx := context.WithValue(context.Background(), ctxkey.EffectiveGroupID, int64(5))

	got := EffectiveGroupIDForScheduling(ctx, &source)
	if got == nil || *got != 5 {
		t.Fatalf("want target group 5, got %v", got)
	}
}

// TestEffectiveGroupIDForSchedulingNoRoute 未命中路由时原样返回源分组，保持原行为。
func TestEffectiveGroupIDForSchedulingNoRoute(t *testing.T) {
	source := int64(1)

	got := EffectiveGroupIDForScheduling(context.Background(), &source)
	if got == nil || *got != 1 {
		t.Fatalf("want source group 1 unchanged, got %v", got)
	}
}

// TestEffectiveGroupIDForSchedulingNilGroup 未分组 key 不得 panic，原样返回 nil。
func TestEffectiveGroupIDForSchedulingNilGroup(t *testing.T) {
	if got := EffectiveGroupIDForScheduling(context.Background(), nil); got != nil {
		t.Fatalf("nil group must stay nil, got %v", got)
	}
}

// TestEffectiveGroupIDForSchedulingIgnoresZero ctx 里是 0 视为无效，回落源分组。
func TestEffectiveGroupIDForSchedulingIgnoresZero(t *testing.T) {
	source := int64(1)
	ctx := context.WithValue(context.Background(), ctxkey.EffectiveGroupID, int64(0))

	got := EffectiveGroupIDForScheduling(ctx, &source)
	if got == nil || *got != 1 {
		t.Fatalf("zero effective id must fall back to source, got %v", got)
	}
}

// TestEffectiveGroupIDForSchedulingIgnoresWrongType ctx 里类型不对不得 panic。
func TestEffectiveGroupIDForSchedulingIgnoresWrongType(t *testing.T) {
	source := int64(1)
	ctx := context.WithValue(context.Background(), ctxkey.EffectiveGroupID, "5")

	got := EffectiveGroupIDForScheduling(ctx, &source)
	if got == nil || *got != 1 {
		t.Fatalf("wrong-typed effective id must fall back to source, got %v", got)
	}
}
