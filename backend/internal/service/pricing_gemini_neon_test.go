package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGeminiNeonCatalogBillsAllThinkingLevels(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)
	pricing := &PricingService{}
	pricing.pricingData, err = pricing.parsePricingData(data)
	require.NoError(t, err)
	billing := NewBillingService(&config.Config{}, pricing)
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}
	for _, model := range []string{"gemini-4-neon", "gemini-4-neon-low", "gemini-4-neon-medium", "gemini-4-neon-high", "gemini-4-neon-xhigh", "models/gemini-4-neon-xhigh"} {
		t.Run(model, func(t *testing.T) {
			card := pricing.GetIdentifiedModelPricing(model)
			require.NotNil(t, card)
			require.Zero(t, card.LongContextInputTokenThreshold)
			require.Zero(t, card.CacheStorageCostPerTokenPerHour)
			cost, err := billing.CalculateCostWithServiceTier(model, tokens, 1, "")
			require.NoError(t, err)
			require.InDelta(t, 3, cost.InputCost, 1e-12)
			require.InDelta(t, 15, cost.OutputCost, 1e-12)
			require.InDelta(t, 0.15, cost.CacheReadCost, 1e-12)
			cost, err = billing.CalculateCostWithServiceTier(model, tokens, 1, "priority")
			require.NoError(t, err)
			require.InDelta(t, 5.4+27+0.27, cost.TotalCost, 1e-12)
		})
	}
}
