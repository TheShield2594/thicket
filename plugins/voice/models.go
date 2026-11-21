package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// modelFile is one file the plugin needs: where it lives in the models
// volume, where to get it, and what its SHA-256 must be.
type modelFile struct {
	Path   string
	URL    string
	SHA256 string // lower-case hex; "" = not checked
}

// maxModelBytes bounds one download (whisper's large model is about 3 GB).
const maxModelBytes = 4 << 30

// ensure makes sure a model file is present and matches its checksum,
// downloading it if it is missing or wrong. A file that is there but
// doesn't match is replaced, never used.
func (m modelFile) ensure(ctx context.Context, client *http.Client) error {
	if ok, err := m.present(); err != nil {
		return err
	} else if ok {
		return nil
	}
	if m.URL == "" {
		return fmt.Errorf("%s is missing and no download address is set for it", m.Path)
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		return err
	}
	log.Printf("downloading %s from %s", filepath.Base(m.Path), m.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "thicket-voice/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", filepath.Base(m.Path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", filepath.Base(m.Path), resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.Path), "."+filepath.Base(m.Path)+".*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxModelBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", filepath.Base(m.Path), err)
	}
	if n > maxModelBytes {
		return fmt.Errorf("downloading %s: larger than %d bytes", filepath.Base(m.Path), int64(maxModelBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); m.SHA256 != "" && !strings.EqualFold(got, m.SHA256) {
		return fmt.Errorf("%s: checksum mismatch (got %s, want %s); not used", filepath.Base(m.Path), got, m.SHA256)
	}
	if m.SHA256 == "" {
		log.Printf("warning: %s has no checksum set, so it wasn't checked", filepath.Base(m.Path))
	}
	return os.Rename(tmp.Name(), m.Path)
}

// present reports whether the file is there and matches its checksum.
func (m modelFile) present() (bool, error) {
	f, err := os.Open(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if m.SHA256 == "" {
		st, err := f.Stat()
		return err == nil && st.Size() > 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), m.SHA256) {
		log.Printf("%s doesn't match its checksum; fetching it again", filepath.Base(m.Path))
		return false, nil
	}
	return true, nil
}

// prepare fetches every model file, retrying every retry until all are
// there, then marks the server ready.
func (s *server) prepare(ctx context.Context, retry time.Duration) {
	for {
		err := s.fetchModels(ctx)
		if err == nil {
			s.setStatus("ready", "")
			log.Printf("models ready; voices: %s", strings.Join(s.tools.voices(), ", "))
			return
		}
		s.setStatus("error", err.Error())
		log.Printf("models not ready: %v (trying again in %s)", err, retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func (s *server) fetchModels(ctx context.Context) error {
	for _, m := range s.cfg.Models {
		s.setStatus("starting", "fetching "+filepath.Base(m.Path))
		if err := m.ensure(ctx, s.http); err != nil {
			return err
		}
	}
	return nil
}
