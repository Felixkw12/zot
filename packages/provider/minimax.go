package provider

import "strings"

// miniMaxThinkingID marks MiniMax replay data, not a provider-issued item ID.
// Other wire formats must not interpret its signature as their own metadata.
const miniMaxThinkingID = "minimax-thinking"

// miniMaxTextID only identifies the built-in MiniMax routes. Custom providers
// and explicit user catalog entries retain their own model selection policy.
func miniMaxTextID(m Model) string {
	id := strings.ToLower(m.ID)
	switch m.Provider {
	case "minimax", "minimax-cn", "opencode", "opencode-go":
		if strings.HasPrefix(id, "minimax-") {
			return id
		}
	case "openrouter", "vercel-ai-gateway":
		if strings.HasPrefix(id, "minimax/") {
			return strings.TrimPrefix(id, "minimax/")
		}
	case "huggingface", "together":
		if strings.HasPrefix(id, "minimaxai/") {
			return strings.TrimPrefix(id, "minimaxai/")
		}
	case "fireworks":
		if strings.HasPrefix(id, "accounts/fireworks/models/minimax-") {
			return strings.TrimPrefix(id, "accounts/fireworks/models/")
		}
	case "amazon-bedrock":
		if strings.HasPrefix(id, "minimax.minimax-") {
			return strings.TrimPrefix(id, "minimax.")
		}
	}
	return ""
}

func miniMaxFlash(m Model) bool {
	switch m.Provider {
	case "minimax", "minimax-cn":
		return m.ID == "MiniMax-M3.1-Flash"
	case "openrouter":
		return m.ID == "minimax/minimax-m3.1-flash:free"
	case "opencode":
		return m.ID == "minimax-m3.1-flash-free"
	}
	return false
}

func ordinaryMiniMaxChoice(m Model) bool {
	if m.Source == "user" {
		return true
	}
	id := miniMaxTextID(m)
	// Only retire numbered text-model families; preserve media IDs.
	text := strings.HasPrefix(id, "minimax-m") && len(id) > len("minimax-m") && id[len("minimax-m")] >= '0' && id[len("minimax-m")] <= '9'
	text = text || id == "minimax-text-01"
	return !text || id == "minimax-m3" || miniMaxFlash(m)
}
