package main

import (
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// applyingSettings is held while a settings change is applied to the running
// app, so changes apply one at a time.
var applyingSettings sync.Mutex

// onConfigChange runs apply after every settings change, with the latest
// config saved, not the one the change saved: settings changes saved together
// run their callbacks at the same time, so each applies the latest config, one
// at a time, and whichever applies last leaves the app running the last
// config saved, however slow applying one is.
func onConfigChange(cfgStore *config.Store, apply func(cfg *config.Config)) {
	cfgStore.OnChange(func(config.Config) {
		applyingSettings.Lock()
		defer applyingSettings.Unlock()
		cfg := cfgStore.Get()
		apply(&cfg)
	})
}
