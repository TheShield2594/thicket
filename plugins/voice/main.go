// Command voice is Thicket's local speech plugin: it hears voice messages
// with whisper.cpp and reads replies aloud with Piper, behind the two
// OpenAI-compatible endpoints Thicket already speaks:
//
//	POST /v1/audio/transcriptions   (multipart: file, language, response_format)
//	POST /v1/audio/speech           (JSON: input, voice, response_format, speed)
//
// It is a small Go server in front of the whisper-cli, piper and ffmpeg
// programs, so it can be tested with fake ones. Model files are
// downloaded on first start into a volume and checked against pinned
// SHA-256 sums. Every endpoint but /health needs the token Thicket shares
// through a file both containers mount. See docs/voice.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is reported to Thicket (and `thicket doctor`) when it connects.
var version = "0.1.0"

// The default models: whisper.cpp's multilingual base model (quantised,
// about 60 MB, fast enough on a Raspberry Pi 5) and Piper's US English
// "lessac" voice. URLs are pinned to a repository revision and every file
// is checked against its SHA-256 before it is used.
const (
	defaultWhisperURL    = "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base-q5_1.bin"
	defaultWhisperSHA256 = "422f1ae452ade6f30a004d7e5c6a43195e4433bc370bf23fac9cc591f01a8898"
	defaultVoice         = "en_US-lessac-medium"
	defaultVoiceURL      = "https://huggingface.co/rhasspy/piper-voices/resolve/375a0fe641dea077c2a47b4e9a056d6da521eed3/en/en_US/lessac/medium/en_US-lessac-medium.onnx"
	defaultVoiceSHA256   = "5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f"
	defaultConfigURL     = "https://huggingface.co/rhasspy/piper-voices/resolve/375a0fe641dea077c2a47b4e9a056d6da521eed3/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json"
	defaultConfigSHA256  = "efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0"
)

func main() {
	cfg := configFromEnv(os.Getenv)
	s := newServer(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.prepare(ctx, time.Minute)
	srv := &http.Server{Addr: cfg.Addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("voice plugin %s listening on %s", version, cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// config is everything the plugin reads from its environment.
type config struct {
	Addr      string
	TokenFile string
	ModelsDir string

	FFmpeg  string
	Whisper string
	Piper   string

	Models   []modelFile
	Voice    string // the default Piper voice (a <name>.onnx in the piper folder)
	Language string // whisper's language: auto, or a code such as en
	Threads  int
	Jobs     int           // how many transcriptions or syntheses run at once
	Timeout  time.Duration // for one program run
}

func configFromEnv(getenv func(string) string) config {
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		if n, err := strconv.Atoi(getenv(k)); err == nil && n > 0 {
			return n
		}
		return def
	}
	c := config{
		Addr:      env("VOICE_ADDR", ":7071"),
		TokenFile: env("VOICE_TOKEN_FILE", "/plugin-auth/token"),
		ModelsDir: env("VOICE_MODELS_DIR", "/models"),
		FFmpeg:    env("VOICE_FFMPEG_BIN", "ffmpeg"),
		Whisper:   env("VOICE_WHISPER_BIN", "whisper-cli"),
		Piper:     env("VOICE_PIPER_BIN", "/opt/piper/piper"),
		Voice:     env("VOICE_PIPER_VOICE", defaultVoice),
		Language:  env("VOICE_LANGUAGE", "auto"),
		Threads:   num("VOICE_THREADS", min(runtime.NumCPU(), 4)),
		Jobs:      num("VOICE_JOBS", 1),
		Timeout:   time.Duration(num("VOICE_TIMEOUT_SECONDS", 120)) * time.Second,
	}
	// A URL set without a checksum is used unchecked (the log says so);
	// a URL left at its default keeps the pinned checksum.
	sum := func(urlKey, sumKey, defURL, defSum string) (string, string) {
		u := env(urlKey, defURL)
		if u == defURL {
			return u, env(sumKey, defSum)
		}
		return u, getenv(sumKey)
	}
	wURL, wSum := sum("VOICE_WHISPER_MODEL_URL", "VOICE_WHISPER_MODEL_SHA256", defaultWhisperURL, defaultWhisperSHA256)
	c.Models = append(c.Models, modelFile{Path: filepath.Join(c.ModelsDir, "whisper", fileName(wURL, "model.bin")), URL: wURL, SHA256: wSum})
	vURL, vSum, cURL, cSum := defaultVoiceURL, defaultVoiceSHA256, defaultConfigURL, defaultConfigSHA256
	if c.Voice != defaultVoice || getenv("VOICE_PIPER_VOICE_URL") != "" {
		// Another voice: its files come from where the owner says (or are
		// already in the folder), with the checksums they give.
		vURL, vSum = getenv("VOICE_PIPER_VOICE_URL"), getenv("VOICE_PIPER_VOICE_SHA256")
		cURL, cSum = getenv("VOICE_PIPER_CONFIG_URL"), getenv("VOICE_PIPER_CONFIG_SHA256")
	}
	onnx := filepath.Join(c.ModelsDir, "piper", c.Voice+".onnx")
	c.Models = append(c.Models,
		modelFile{Path: onnx, URL: vURL, SHA256: vSum},
		modelFile{Path: onnx + ".json", URL: cURL, SHA256: cSum})
	return c
}

// fileName is the last part of a download URL, or def.
func fileName(u, def string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	name := path.Base(u)
	if name == "" || name == "." || name == "/" || strings.HasPrefix(name, ".") {
		return def
	}
	return name
}

func (c config) whisperModel() string { return c.Models[0].Path }
func (c config) piperDir() string     { return filepath.Join(c.ModelsDir, "piper") }
