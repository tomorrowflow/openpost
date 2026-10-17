package main

import (
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/config"
)

const (
	contentAIRequestTimeout = 240 * time.Second
	contentAIMaxRetries     = 12
)

func openRouterConfigs(cfg *config.Config) (ai.OpenRouterConfig, ai.OpenRouterConfig) {
	// An empty BaseURL keeps OpenRouter; any OpenAI-compatible endpoint (e.g. Ollama) works.
	base := ai.OpenRouterConfig{
		APIKey:         cfg.OpenRouterAPIKey,
		BaseURL:        cfg.OpenRouterBaseURL,
		HTTPReferer:    cfg.PublicURL,
		XTitle:         "OpenPost",
		SchemaInPrompt: cfg.OpenRouterBaseURL != "",
	}
	imageConfig := base
	imageConfig.Provider = cfg.ImageCaptionProvider
	imageConfig.RequireZDR = cfg.ImageCaptionRequireZDR
	contentConfig := base
	contentConfig.Timeout = contentAIRequestTimeout
	contentConfig.MaxRetries = contentAIMaxRetries
	contentConfig.Provider = cfg.ContentAIProvider
	contentConfig.RequireZDR = cfg.ContentAIRequireZDR
	return imageConfig, contentConfig
}
