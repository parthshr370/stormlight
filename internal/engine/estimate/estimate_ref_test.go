package estimate

import (
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestEstimateMessageTokensWeightsTextDocumentRefBySize(t *testing.T) {
	block := types.NewDocumentRef("store", "key", "text/plain", "big.txt", 40000, 0)
	msg := types.UserMessage{Content: types.BlockContent(block)}
	if got := EstimateMessageTokens(msg); got != 10000 {
		t.Fatalf("EstimateMessageTokens(text docref) = %d, want 10000", got)
	}
}

func TestEstimateMessageTokensWeightsPDFRefByPages(t *testing.T) {
	block := types.NewDocumentRef("store", "key", "application/pdf", "big.pdf", 1000, 10)
	msg := types.UserMessage{Content: types.BlockContent(block)}
	if got := EstimateMessageTokens(msg); got != 30000 {
		t.Fatalf("EstimateMessageTokens(pdf docref) = %d, want 30000 (10 pages)", got)
	}
}

func TestEstimateMessageTokensImageRefKeepsImageWeight(t *testing.T) {
	block := types.NewImageRef("store", "key", "image/png", "chart.png", 5000)
	msg := types.UserMessage{Content: types.BlockContent(block)}
	if got := EstimateMessageTokens(msg); got != EstimatedImageChars/CharsPerToken {
		t.Fatalf("EstimateMessageTokens(imageRef) = %d, want %d", got, EstimatedImageChars/CharsPerToken)
	}
}
