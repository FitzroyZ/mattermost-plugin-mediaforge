package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// Mattermost treats a nil post from MessageWillBeUpdated as a REJECTION
// (plugin/hooks.go: "the modified post or nil if rejected"; the server then
// answers 400 "Post rejected by plugin."). Unlike MessageWillBePosted, nil
// does not mean "unchanged" here. Live on 2026-09-19..23 every edit of a post
// without a MediaForge note was refused server-wide, including bot patches.
func TestEditOfPostWithoutNoteIsNeverRejected(t *testing.T) {
	np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleLine})

	cases := map[string]*model.Post{
		"plain text post":         {Id: "p1", ChannelId: "c1", Message: "hello"},
		"bot post with props":     func() *model.Post { p := &model.Post{Id: "p2", ChannelId: "c1", Message: "x"}; p.AddProp("from_bot", "true"); return p }(),
		"post with file, no note": {Id: "p3", ChannelId: "c1", Message: "f", FileIds: []string{"f1"}},
	}
	for name, old := range cases {
		t.Run(name, func(t *testing.T) {
			edited := old.Clone()
			edited.Message = old.Message + " (edited)"
			got, reason := np.MessageWillBeUpdated(&plugin.Context{}, edited, old)
			if reason != "" {
				t.Fatalf("edit refused with reason %q", reason)
			}
			if got == nil {
				t.Fatal("nil post returned: Mattermost rejects the edit")
			}
			if got.Message != edited.Message {
				t.Fatalf("edit altered: got %q want %q", got.Message, edited.Message)
			}
		})
	}
}

func TestNilArgumentsDoNotRejectWhenANewPostExists(t *testing.T) {
	np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleLine})
	edited := &model.Post{Id: "p", Message: "m"}
	got, reason := np.MessageWillBeUpdated(&plugin.Context{}, edited, nil)
	if reason != "" || got == nil || got.Message != "m" {
		t.Fatalf("got=%v reason=%q", got, reason)
	}
}
