package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/llm"
)

// voicePhrase is what the voice check says and expects to hear back:
// about a second of speech.
const voicePhrase = "Testing, one two three."

// voiceCheck is the voice plugin's doctor check: both endpoints answer,
// and a round trip works (a short phrase synthesized, then transcribed
// back). The time it took is in the note; words that come back different
// are advice (a small model mishears), not a failure.
func voiceCheck(ctx context.Context, url, token string) (string, error) {
	key := func(context.Context, string) (string, error) { return token, nil }
	base := url + "/v1"
	start := time.Now()
	sp, err := (&llm.SpeechClient{URL: func(context.Context) string { return base }, Key: key}).
		Speak(ctx, llm.SpeechRequest{Model: "piper", Voice: "default", Input: voicePhrase, Format: "wav"})
	if err != nil {
		return "", fmt.Errorf("speech (%s/audio/speech) failed: %w", base, voiceErr(err))
	}
	said := time.Since(start)
	tr, err := (&llm.TranscriptionClient{Key: key}).Transcribe(ctx, llm.TranscriptionRequest{
		URL: base, Model: "whisper-1", Audio: sp.Audio, Filename: "check.wav", Language: "en"})
	if err != nil {
		return "", fmt.Errorf("transcription (%s/audio/transcriptions) failed: %w", base, voiceErr(err))
	}
	total := time.Since(start)
	note := fmt.Sprintf("round trip in %s (speech %s, transcription %s)", total.Round(10*time.Millisecond),
		said.Round(10*time.Millisecond), (total - said).Round(10*time.Millisecond))
	if !sameWords(tr.Text, voicePhrase) {
		note += fmt.Sprintf("; said %q, heard %q", voicePhrase, tr.Text)
	}
	return note, nil
}

// voiceErr explains the plugin's "still fetching its models" answer.
func voiceErr(err error) error {
	var ae *llm.APIError
	if errors.As(err, &ae) && ae.Status == 503 {
		return fmt.Errorf("%s (the first start downloads about 130 MB of models; try again in a few minutes)", ae.Message)
	}
	return err
}

// sameWords compares two phrases ignoring case, punctuation and how the
// numbers are written.
func sameWords(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.NewReplacer("1", " one ", "2", " two ", "3", " three ").Replace(s)
		return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z') }), " ")
	}
	return norm(a) == norm(b)
}
