package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

const codexBaseInstructions = "You are Codex, a coding agent. Follow the user's instructions and use the available tools to complete software engineering tasks. Inspect relevant files before editing, preserve unrelated changes, and verify the result."

type codexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexModelEntry struct {
	Slug                              string                `json:"slug"`
	DisplayName                       string                `json:"display_name"`
	Description                       string                `json:"description"`
	DefaultReasoningLevel             string                `json:"default_reasoning_level"`
	SupportedReasoningLevels          []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                         string                `json:"shell_type"`
	Visibility                        string                `json:"visibility"`
	MinimalClientVersion              string                `json:"minimal_client_version"`
	SupportedInAPI                    bool                  `json:"supported_in_api"`
	Priority                          int                   `json:"priority"`
	AdditionalSpeedTiers              []string              `json:"additional_speed_tiers"`
	ServiceTiers                      []any                 `json:"service_tiers"`
	DefaultServiceTier                *string               `json:"default_service_tier"`
	AvailabilityNUX                   any                   `json:"availability_nux"`
	Upgrade                           any                   `json:"upgrade"`
	BaseInstructions                  string                `json:"base_instructions"`
	ModelMessages                     any                   `json:"model_messages"`
	IncludeSkillsUsageInstructions    bool                  `json:"include_skills_usage_instructions"`
	SupportsReasoningSummaryParameter bool                  `json:"supports_reasoning_summary_parameter"`
	SupportsReasoningSummaries        bool                  `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary           string                `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                  `json:"support_verbosity"`
	DefaultVerbosity                  *string               `json:"default_verbosity"`
	ApplyPatchToolType                *string               `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                `json:"web_search_tool_type"`
	TruncationPolicy                  codexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                  `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal       bool                  `json:"supports_image_detail_original"`
	ContextWindow                     int                   `json:"context_window"`
	MaxContextWindow                  int                   `json:"max_context_window"`
	AutoCompactTokenLimit             *int                  `json:"auto_compact_token_limit"`
	EffectiveContextWindowPercent     int                   `json:"effective_context_window_percent"`
	ExperimentalSupportedTools        []string              `json:"experimental_supported_tools"`
	InputModalities                   []string              `json:"input_modalities"`
	SupportsSearchTool                bool                  `json:"supports_search_tool"`
	UseResponsesLite                  bool                  `json:"use_responses_lite"`
}

type codexModelCatalog struct {
	Models []codexModelEntry `json:"models"`
}

var codexReasoningDescriptions = map[string]string{
	"none":   "No reasoning",
	"low":    "Fast responses with lighter reasoning",
	"medium": "Balances speed and reasoning depth for everyday tasks",
	"high":   "Greater reasoning depth for complex problems",
	"xhigh":  "Extra high reasoning depth for complex problems",
	"max":    "Maximum reasoning depth for the hardest problems",
}

func codexReasoningLevelsFor(levels []string) []codexReasoningLevel {
	result := make([]codexReasoningLevel, 0, len(levels))
	for _, level := range levels {
		result = append(result, codexReasoningLevel{Effort: level, Description: codexReasoningDescriptions[level]})
	}
	return result
}

func newCodexModelCatalog(items []modeldomain.PublicModel) codexModelCatalog {
	models := make([]codexModelEntry, 0, len(items))
	for index, item := range items {
		modalities := []string{"text"}
		if item.ImageInput {
			modalities = append(modalities, "image")
		}
		visibility := "hide"
		if item.AgentVisible {
			visibility = "list"
		}
		var applyPatchToolType *string
		toolsSupported := item.AgentTools
		if toolsSupported {
			value := "freeform"
			applyPatchToolType = &value
		}
		models = append(models, codexModelEntry{
			Slug:                              item.ID,
			DisplayName:                       codexDisplayName(item.ID),
			Description:                       item.Description,
			DefaultReasoningLevel:             item.DefaultReasoningLevel,
			SupportedReasoningLevels:          codexReasoningLevelsFor(item.ReasoningLevels),
			ShellType:                         "shell_command",
			Visibility:                        visibility,
			MinimalClientVersion:              "0.0.0",
			SupportedInAPI:                    true,
			Priority:                          index + 1,
			AdditionalSpeedTiers:              []string{},
			ServiceTiers:                      []any{},
			BaseInstructions:                  codexBaseInstructions,
			IncludeSkillsUsageInstructions:    false,
			SupportsReasoningSummaryParameter: item.ReasoningSupported,
			SupportsReasoningSummaries:        item.ReasoningSupported,
			DefaultReasoningSummary:           "auto",
			SupportVerbosity:                  false,
			ApplyPatchToolType:                applyPatchToolType,
			WebSearchToolType:                 "text",
			TruncationPolicy:                  codexTruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:         toolsSupported,
			SupportsImageDetailOriginal:       false,
			ContextWindow:                     item.ContextWindow,
			MaxContextWindow:                  item.ContextWindow,
			EffectiveContextWindowPercent:     95,
			ExperimentalSupportedTools:        []string{},
			InputModalities:                   modalities,
			SupportsSearchTool:                false,
			UseResponsesLite:                  false,
		})
	}
	return codexModelCatalog{Models: models}
}

func writeCodexModelCatalog(c *gin.Context, catalog codexModelCatalog) {
	body, err := json.Marshal(catalog)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "编码模型列表失败")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	c.Header("ETag", etag)
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func codexDisplayName(slug string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(slug))
	for index, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[index] = string(runes)
		}
	}
	return strings.Join(words, " ")
}
