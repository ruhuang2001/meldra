package app

import "testing"

func TestCompactionDueWithoutCalibrationUsesByteBudget(t *testing.T) {
	agent := Agent{
		compactThresholdPercent: 80,
		maxTurnContextBytes:     1000,
		contextWindowTokens:     200000,
	}
	if agent.compactionDue(799) {
		t.Fatal("compaction triggered below the byte threshold")
	}
	if !agent.compactionDue(800) {
		t.Fatal("compaction did not trigger at the byte threshold")
	}
}

func TestCompactionDueUsesTokensAfterCalibration(t *testing.T) {
	agent := Agent{
		compactThresholdPercent: 80,
		maxTurnContextBytes:     4000,
		contextWindowTokens:     1000,
	}
	// 5000 bytes measured 500 input tokens, so bytes/token is 10.
	agent.recordPromptCalibration(5000, 500)
	if agent.compactionDue(1000) {
		t.Fatal("compaction triggered while the token estimate was below the window threshold")
	}
	// 8000 bytes estimate 800 tokens, which is the 80% threshold of 1000.
	if !agent.compactionDue(8000) {
		t.Fatal("compaction did not trigger at the token threshold")
	}
}

func TestCompactionDueByteBudgetStillWins(t *testing.T) {
	agent := Agent{
		compactThresholdPercent: 80,
		maxTurnContextBytes:     1000,
		contextWindowTokens:     1000000,
	}
	// A generous window alone must not disable the byte budget.
	agent.recordPromptCalibration(10, 1)
	if !agent.compactionDue(800) {
		t.Fatal("byte budget did not trigger compaction")
	}
}

func TestCompactionTokenTriggerNeedsAWindow(t *testing.T) {
	agent := Agent{compactThresholdPercent: 80, maxTurnContextBytes: 100000}
	agent.recordPromptCalibration(1000, 1000)
	if _, ok := agent.estimatedContextTokens(100000); ok {
		t.Fatal("token estimate was produced without a configured window")
	}
	if tokens, limit, ok := agent.contextWaterline(); ok {
		t.Fatalf("waterline reported without a window: %d/%d", tokens, limit)
	}
}
