package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tools runs the speech programs: ffmpeg to convert audio, whisper-cli
// to hear it, piper to speak. Each run gets its own temporary folder and
// a time limit; nothing is kept.
type tools struct {
	ffmpeg, whisper, piper string
	whisperModel           string
	piperDir               string
	defaultVoice           string
	language               string
	threads                int
	timeout                time.Duration
}

func newTools(c config) *tools {
	return &tools{ffmpeg: c.FFmpeg, whisper: c.Whisper, piper: c.Piper, whisperModel: c.whisperModel(),
		piperDir: c.piperDir(), defaultVoice: c.Voice, language: c.Language, threads: c.Threads, timeout: c.Timeout}
}

// run runs a program, with stdin and extra environment, and returns a
// readable error with the end of what it printed when it fails.
func (t *tools) run(ctx context.Context, stdin string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s took longer than %s", filepath.Base(name), t.timeout)
		}
		msg := strings.TrimSpace(out.String())
		if len(msg) > 400 {
			msg = "…" + msg[len(msg)-400:]
		}
		if msg != "" {
			return fmt.Errorf("%s failed: %v: %s", filepath.Base(name), err, msg)
		}
		return fmt.Errorf("%s failed: %v", filepath.Base(name), err)
	}
	return nil
}

// transcribe turns a recording (any format ffmpeg reads) into text.
func (t *tools) transcribe(ctx context.Context, audio []byte, language string) (string, error) {
	dir, err := os.MkdirTemp("", "voice-in-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	in, wav := filepath.Join(dir, "in"), filepath.Join(dir, "in.wav")
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return "", err
	}
	// whisper.cpp wants 16 kHz mono 16-bit WAV.
	if err := t.run(ctx, "", nil, t.ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", in, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav); err != nil {
		return "", fmt.Errorf("couldn't read the recording: %w", err)
	}
	if language == "" {
		language = t.language
	}
	out := filepath.Join(dir, "out")
	if err := t.run(ctx, "", nil, t.whisper, "-m", t.whisperModel, "-f", wav, "-l", language,
		"-t", strconv.Itoa(max(t.threads, 1)), "-nt", "-np", "-otxt", "-of", out); err != nil {
		return "", err
	}
	b, err := os.ReadFile(out + ".txt")
	if err != nil {
		return "", fmt.Errorf("whisper wrote no transcript: %w", err)
	}
	return strings.Join(strings.Fields(string(b)), " "), nil
}

// formats maps an OpenAI response_format to ffmpeg's output arguments
// and the content type. "wav" is what piper writes.
var formats = map[string]struct {
	args []string
	ct   string
}{
	"wav":  {nil, "audio/wav"},
	"mp3":  {[]string{"-c:a", "libmp3lame", "-q:a", "4", "-f", "mp3"}, "audio/mpeg"},
	"opus": {[]string{"-c:a", "libopus", "-b:a", "32k", "-f", "ogg"}, "audio/ogg"},
	"aac":  {[]string{"-c:a", "aac", "-b:a", "64k", "-f", "adts"}, "audio/aac"},
	"flac": {[]string{"-c:a", "flac", "-f", "flac"}, "audio/flac"},
	"pcm":  {[]string{"-ar", "24000", "-ac", "1", "-f", "s16le", "-c:a", "pcm_s16le"}, "audio/pcm"},
}

// speak reads text aloud in a voice and returns audio in format.
func (t *tools) speak(ctx context.Context, text, voice string, speed float64, format string) ([]byte, string, error) {
	f, ok := formats[format]
	if !ok {
		return nil, "", fmt.Errorf("unsupported response_format %q", format)
	}
	dir, err := os.MkdirTemp("", "voice-out-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	model := filepath.Join(t.piperDir, t.voiceName(voice)+".onnx")
	wav := filepath.Join(dir, "out.wav")
	args := []string{"--model", model, "--config", model + ".json", "--output_file", wav}
	if speed > 0 && speed != 1 {
		args = append(args, "--length_scale", strconv.FormatFloat(1/speed, 'f', 3, 64))
	}
	pdir := filepath.Dir(t.piper)
	if st, err := os.Stat(filepath.Join(pdir, "espeak-ng-data")); err == nil && st.IsDir() {
		args = append(args, "--espeak_data", filepath.Join(pdir, "espeak-ng-data"))
	}
	// Piper reads one utterance per line: the whole reply is one.
	line := strings.Join(strings.Fields(text), " ") + "\n"
	// Its own libraries sit next to it; only piper is given them.
	if err := t.run(ctx, line, []string{"LD_LIBRARY_PATH=" + pdir}, t.piper, args...); err != nil {
		return nil, "", err
	}
	src := wav
	if f.args != nil {
		src = filepath.Join(dir, "out."+format)
		a := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", wav}, f.args...)
		if err := t.run(ctx, "", nil, t.ffmpeg, append(a, src)...); err != nil {
			return nil, "", err
		}
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, "", err
	}
	if len(b) == 0 {
		return nil, "", fmt.Errorf("no audio was made")
	}
	return b, f.ct, nil
}

// voices lists the Piper voices in the models folder: each <name>.onnx
// with its <name>.onnx.json.
func (t *tools) voices() []string {
	matches, _ := filepath.Glob(filepath.Join(t.piperDir, "*.onnx"))
	var out []string
	for _, m := range matches {
		if _, err := os.Stat(m + ".json"); err == nil {
			out = append(out, strings.TrimSuffix(filepath.Base(m), ".onnx"))
		}
	}
	sort.Strings(out)
	return out
}

// voiceName picks the voice to use: the one asked for if it is there
// (OpenAI names such as alloy aren't, so they get the default).
func (t *tools) voiceName(asked string) string {
	for _, v := range t.voices() {
		if v == asked {
			return v
		}
	}
	return t.defaultVoice
}
