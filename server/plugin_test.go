package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
)

// TestPluginSatisfiesClientMainContract asserts the contracts that decide
// whether this plugin can start at all.
//
// plugin.ClientMain panics unless the implementation provides SetAPI and
// SetDriver, which is what embedding plugin.MattermostPlugin supplies.
//
// Mattermost then discovers hooks by method set, so the lifecycle methods must
// exist with exactly the signatures plugin.Hooks declares; a renamed or
// re-typed hook would otherwise silently mean "this plugin has no such hook".
// plugin.MattermostPlugin supplies no default lifecycle implementations, so
// both assertions below are real compile-time checks.
//
// The full plugin.Hooks interface is deliberately NOT asserted: plugins
// implement only the hooks they need, and requiring all of them would be wrong.
func TestPluginSatisfiesClientMainContract(t *testing.T) {
	var _ interface {
		SetAPI(api plugin.API)
		SetDriver(driver plugin.Driver)
	} = (*MediaForgePlugin)(nil)

	var _ interface {
		OnActivate() error
		OnDeactivate() error
	} = (*MediaForgePlugin)(nil)

	// Method expressions pin the upstream signatures: if Mattermost ever
	// changes a lifecycle hook, this stops compiling instead of silently
	// leaving the plugin without that hook.
	var _ func(plugin.Hooks) error = plugin.Hooks.OnActivate
	var _ func(plugin.Hooks) error = plugin.Hooks.OnDeactivate
}

// TestLifecycleHooksSucceed exercises the activation and deactivation paths
// with a mock API, which is what Mattermost calls when an administrator toggles
// the plugin in the System Console. the initial skeleton's whole promise is that these two
// calls return without error.
func TestLifecycleHooksSucceed(t *testing.T) {
	api := &plugintest.API{}
	api.On("LogInfo", "MediaForge activated").Return()
	api.On("LogInfo", "MediaForge deactivated").Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)

	if err := p.OnActivate(); err != nil {
		t.Errorf("OnActivate returned an error: %v", err)
	}

	if err := p.OnDeactivate(); err != nil {
		t.Errorf("OnDeactivate returned an error: %v", err)
	}

	api.AssertExpectations(t)
}
