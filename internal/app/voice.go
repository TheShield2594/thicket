package app

import (
	"context"
	"strings"

	"github.com/theshield2594/thicket/internal/plugin"
)

// Local speech (docs/voice.md). Voice messages are transcribed, and
// spoken replies made, on:
//
//  1. transcribe_url / speech_url, when set;
//  2. else the voice plugin (whisper.cpp and Piper, `docker compose
//     --profile voice`), while it is running and local_voice is on;
//  3. else the model server: OpenRouter's transcribe_model for voice
//     messages (other model servers can't hear audio), model_url for
//     speech.
//
// local_voice is on for new installs. An install that already had people
// when this version first started keeps what it did before (local_voice
// is stored off once, at that start); turning it on is one setting.

// localVoiceDecidedKey marks that local_voice was decided for this
// install, on the first start of the version that added it.
const localVoiceDecidedKey = "voice.local_default_decided"

// decideLocalVoice runs once per install. A database that already has
// people in it is an upgrade: local_voice is stored off so nothing about
// how it hears and speaks changes. A new install keeps the default (on).
func (a *App) decideLocalVoice(ctx context.Context) {
	if _, done, err := a.DB.Setting(ctx, localVoiceDecidedKey); err != nil || done {
		return
	}
	users, err := a.DB.ListUsers(ctx)
	if err != nil {
		return // decided on a later start
	}
	if len(users) > 0 && !a.Settings.IsSet(ctx, "local_voice") {
		if err := a.DB.SetSetting(ctx, "local_voice", "false"); err != nil {
			return
		}
		a.Log.Info("existing install: local voice stays off until you turn it on (the local_voice setting)")
	}
	_ = a.DB.SetSetting(ctx, localVoiceDecidedKey, "1")
}

// voiceBase is the voice plugin's OpenAI-compatible base URL (".../v1")
// while it is set up and its integration is on, else "".
func (a *App) voiceBase(ctx context.Context) string {
	ep, ok := a.pluginEndpoint(plugin.Voice.Name)
	if !ok || a.isKeptOff(ep.Name) {
		return ""
	}
	servers, err := a.DB.MCPServers(ctx)
	if err != nil {
		return ""
	}
	for _, s := range servers {
		if managed(s, ep) && s.Enabled && s.Target == ep.MCPURL() {
			return ep.URL + "/v1"
		}
	}
	return ""
}

// localVoice is the voice plugin's base URL when it should be used by
// default (local_voice on and the plugin running), else "".
func (a *App) localVoice(ctx context.Context) string {
	if !a.Settings.Bool(ctx, "local_voice") {
		return ""
	}
	return a.voiceBase(ctx)
}

// transcribeURL is where voice messages are transcribed: transcribe_url,
// else the voice plugin, else "" (OpenRouter's transcribe_model).
func (a *App) transcribeURL(ctx context.Context) string {
	if u := a.Settings.String(ctx, "transcribe_url"); u != "" {
		return u
	}
	return a.localVoice(ctx)
}

// speechURL is where spoken replies are made: speech_url, else the voice
// plugin, else the model server.
func (a *App) speechURL(ctx context.Context) string {
	if u := a.Settings.String(ctx, "speech_url"); u != "" {
		return u
	}
	if u := a.localVoice(ctx); u != "" {
		return u
	}
	return a.Settings.String(ctx, "model_url")
}

// voiceKey is the key sent to a speech or transcription server: the
// voice plugin's token when the address is the voice plugin's (set
// automatically or by hand), else what the model server would get.
func (a *App) voiceKey(next func(ctx context.Context, base string) (string, error)) func(ctx context.Context, base string) (string, error) {
	return func(ctx context.Context, base string) (string, error) {
		if ep, ok := a.pluginEndpoint(plugin.Voice.Name); ok && (base == ep.URL || strings.HasPrefix(base, ep.URL+"/")) {
			return a.Secrets.Get(ctx, ep.Secret())
		}
		return next(ctx, base)
	}
}
