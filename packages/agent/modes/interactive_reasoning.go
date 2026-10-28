package modes

// Optional so existing SettingsStore implementations remain compatible.
type reasoningPreviewSettingsStore interface {
	SetShowReasoning(bool) error
}

func (i *Interactive) reasoningPreviewSetting() settingsItem {
	return settingsItem{
		key:   "show_reasoning",
		label: "show reasoning",
		desc:  "show readable reasoning live and keep its last three lines before each reply",
		value: i.cfg.ShowReasoning,
	}
}

func (i *Interactive) applyShowReasoning(enabled bool) {
	if store, ok := i.cfg.SettingsStore.(reasoningPreviewSettingsStore); ok {
		if err := store.SetShowReasoning(enabled); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			i.invalidate()
			return
		}
	}
	i.mu.Lock()
	i.cfg.ShowReasoning = enabled
	if i.agent != nil {
		i.agent.SetStreamReasoning(enabled)
	}
	if !enabled {
		i.reasoning.Reset()
	}
	i.statusOK = "show reasoning " + onOff(enabled)
	i.statusErr = ""
	i.mu.Unlock()
	if i.rend != nil {
		i.rend.Clear()
	}
	i.invalidate()
}
