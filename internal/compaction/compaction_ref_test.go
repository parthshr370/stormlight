package compaction

import (
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

func TestEstimateTokensWeightsTextDocumentRefBySize(t *testing.T) {
	msg := ptypes.UserMessage{Content: ptypes.BlockContent(ptypes.NewDocumentRef("s", "k", "text/plain", "big.txt", 40000, 0))}
	if got := EstimateTokens(msg); got != 10000 {
		t.Fatalf("EstimateTokens(text docref) = %d, want 10000", got)
	}
}

func TestEstimateTokensWeightsPDFRefByPages(t *testing.T) {
	msg := ptypes.UserMessage{Content: ptypes.BlockContent(ptypes.NewDocumentRef("s", "k", "application/pdf", "big.pdf", 1000, 10))}
	if got := EstimateTokens(msg); got != 30000 {
		t.Fatalf("EstimateTokens(pdf docref) = %d, want 30000 (10 pages)", got)
	}
}
