package main

import (
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// channelListSeparators are the characters an administrator's list may be
// broken up with.
//
// A person typing a list of channels into a settings box uses commas, or
// newlines, or semicolons, or spaces after commas — and does not think about
// which. Accepting all of them costs one line and removes a whole class of
// "the setting does not work" that looks exactly like a bug.
const channelListSeparators = ",;\n\r\t"

// channelIDFromPath reads the channel an upload belongs to out of the path the
// server built for it.
//
// This is the measurement per-channel availability rests on, and it was not
// obvious that it was possible at all. The hook is handed a *model.FileInfo
// whose ChannelId field is EMPTY at this point — but its Path is not, because
// the server assembles that path from post.ChannelId strictly before the hook
// runs. Verified against the test instance's database: a stored file's
// `channels/<id>/` segment matched its fileinfo.channelid exactly.
//
// The shape is `<date>/teams/<team>/channels/<channel>/users/<user>/<id>/<name>`.
// Anything that does not have that shape returns an empty string, which the
// caller reads as "the channel is unknown".
func channelIDFromPath(path string) string {
	const marker = "/channels/"
	start := strings.Index(path, marker)
	if start < 0 {
		return ""
	}
	rest := path[start+len(marker):]
	end := strings.Index(rest, "/")
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// channelIsOpenedUp reports whether a channel is one the administrator's list
// names.
//
// An EMPTY list means every channel, which is SPEC §4.3's default and the
// behaviour an untouched installation has. That is the one rule here that must
// not be got wrong in the other direction: reading an empty list as "no
// channels" would switch the whole feature off for every existing
// installation the moment this slice ships.
//
// Matching accepts the three things an administrator might reasonably write
// down, because they read channels off a screen rather than out of a database:
// the URL name (`team-updates`), the display name (`Team Updates`), and the
// channel ID for anyone who does work from the database. Comparison ignores
// case and surrounding spaces.
//
// It is deliberately an exact match per entry rather than a substring one. A
// substring rule would make `team` open up `team-updates`, `team-private` and
// `management-team` together — and an administrator who wrote one channel name
// would have opened up three without being told.
func channelIsOpenedUp(list string, channel *model.Channel) bool {
	entries := splitChannelList(list)
	if len(entries) == 0 {
		return true
	}
	if channel == nil {
		// The list names specific channels and this upload's channel could not
		// be identified. Failing closed is the only honest answer: the
		// alternative converts a recording in a channel that may well not be
		// on the list.
		return false
	}

	for _, entry := range entries {
		switch entry {
		case strings.ToLower(channel.Id),
			strings.ToLower(channel.Name),
			strings.ToLower(channel.DisplayName):
			return true
		}
	}
	return false
}

// splitChannelList turns the administrator's text into comparable entries.
func splitChannelList(list string) []string {
	fields := strings.FieldsFunc(list, func(r rune) bool {
		return strings.ContainsRune(channelListSeparators, r)
	})

	entries := make([]string, 0, len(fields))
	for _, f := range fields {
		trimmed := strings.ToLower(strings.TrimSpace(f))
		if trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

// voiceAllowedInChannel reports whether a recording uploaded to this channel
// may be converted.
//
// The channel is looked up through the API rather than matched on the ID
// alone, because the list an administrator wrote almost certainly contains
// names. A lookup that fails is treated as "unknown channel", which
// channelIsOpenedUp fails closed on whenever a list is configured.
//
// With no list configured the API is never called at all: the answer is "yes"
// for every channel, and a lookup per upload would be a database round trip
// bought for nothing on the default configuration.
func (p *MediaForgePlugin) voiceAllowedInChannel(cfg configuration, info *model.FileInfo) bool {
	if len(splitChannelList(cfg.VoiceChannels)) == 0 {
		return true
	}
	if info == nil {
		return false
	}

	channelID := info.ChannelId
	if channelID == "" {
		channelID = channelIDFromPath(info.Path)
	}
	if channelID == "" {
		return false
	}

	return channelIsOpenedUp(cfg.VoiceChannels, p.lookupChannel(channelID))
}

// lookupChannel resolves a channel through the plugin API, or nil.
func (p *MediaForgePlugin) lookupChannel(channelID string) *model.Channel {
	if p.API == nil {
		return nil
	}
	channel, err := p.API.GetChannel(channelID)
	if err != nil || channel == nil {
		return nil
	}
	return channel
}
