package repository

import "testing"

func TestShouldEnqueueSchedulerOutboxForExtraUpdates_CompactCapabilityKeysAreRelevant(t *testing.T) {
	updates := map[string]any{
		"openai_compact_supported":  true,
		"openai_compact_checked_at": "2026-04-10T10:00:00Z",
	}

	if !shouldEnqueueSchedulerOutboxForExtraUpdates(updates) {
		t.Fatalf("expected compact capability updates to enqueue scheduler outbox")
	}
}

func TestShouldEnqueueSchedulerOutboxForExtraUpdates_OpenAIResponsesCapabilityKeysAreRelevant(t *testing.T) {
	updates := map[string]any{
		"openai_responses_mode":      "force_chat_completions",
		"openai_responses_supported": false,
	}

	if !shouldEnqueueSchedulerOutboxForExtraUpdates(updates) {
		t.Fatalf("expected responses capability updates to enqueue scheduler outbox")
	}
}

// 容量反推只读保证：inferred_* 键必须是 scheduler-neutral，写它们绝不触发调度变更。
func TestShouldEnqueueSchedulerOutboxForExtraUpdates_InferredCapacityKeysAreNeutral(t *testing.T) {
	updates := map[string]any{
		"inferred_capacity_7d":         191.2,
		"inferred_capacity_5h":         25.0,
		"inferred_tier":                "pro",
		"inferred_tier_confidence":     0.22,
		"inferred_capacity_updated_at": "2026-06-08T10:00:00Z",
	}

	if shouldEnqueueSchedulerOutboxForExtraUpdates(updates) {
		t.Fatalf("inferred_* capacity keys must be scheduler-neutral (read-only observation, no scheduling impact)")
	}
}
