package config

import (
	"testing"
	"time"
)

func TestRecommendationConfig(t *testing.T) {
	// arrange
	t.Setenv("DATABASE_URL", "test")
	t.Setenv("RECOMMENDATION_DAILY_LIMIT", "")
	t.Setenv("RECOMMENDATION_QUERY_TIMEOUT", "")
	t.Setenv("RECOMMENDATION_REFRESH_INTERVAL", "")
	// act
	cfg, err := Load()
	// assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recommendation.DailyLimit != 30 || cfg.RecommendationQueryTimeout != 5*time.Second || cfg.Recommendation.RefreshInterval != time.Minute || cfg.Recommendation.EmbeddingModel != cfg.LLM.EmbeddingModel || cfg.Recommendation.ModelTimeout != cfg.LLM.Timeout {
		t.Fatal("incorrect recommendation defaults")
	}
	for _, test := range []struct{ name, key, value string }{
		{"invalid_limit", "RECOMMENDATION_DAILY_LIMIT", "abc"},
		{"zero_limit", "RECOMMENDATION_DAILY_LIMIT", "0"},
		{"negative_limit", "RECOMMENDATION_DAILY_LIMIT", "-1"},
		{"invalid_timeout", "RECOMMENDATION_QUERY_TIMEOUT", "abc"},
		{"zero_timeout", "RECOMMENDATION_QUERY_TIMEOUT", "0s"},
		{"invalid_interval", "RECOMMENDATION_REFRESH_INTERVAL", "abc"},
		{"zero_interval", "RECOMMENDATION_REFRESH_INTERVAL", "0s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid recommendation configuration accepted")
			}
		})
	}
}
