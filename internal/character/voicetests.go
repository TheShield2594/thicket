package character

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// VoiceTest is one reference exchange for a sprig: a message and a reply
// that sounds right. `thicket voice-check` replays the message against the
// current model and prompt, and a judge compares the new reply with the
// reference, so drift after a model or prompt change shows up.
//
// They live next to the character file, one YAML file each:
// <characters dir>/<id>/voice_tests/NN-name.yaml
type VoiceTest struct {
	Prompt    string `yaml:"prompt"`
	Reference string `yaml:"reference"`
	// Notes say what matters in this one (shown to the judge).
	Notes string `yaml:"notes,omitempty"`

	Name string `yaml:"-"` // file name without .yaml
	Path string `yaml:"-"`
}

// VoiceTestDir is where a sprig's voice tests live.
func VoiceTestDir(charsDir, id string) string {
	return filepath.Join(charsDir, id, "voice_tests")
}

// LoadVoiceTests reads a sprig's voice tests in file-name order. A missing
// directory is no tests, not an error.
func LoadVoiceTests(charsDir, id string) ([]VoiceTest, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("%q isn't a valid sprig id", id)
	}
	paths, err := filepath.Glob(filepath.Join(VoiceTestDir(charsDir, id), "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []VoiceTest
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var vt VoiceTest
		if err := yaml.Unmarshal(b, &vt); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		vt.Prompt = strings.TrimSpace(vt.Prompt)
		vt.Reference = strings.TrimSpace(vt.Reference)
		if vt.Prompt == "" {
			return nil, fmt.Errorf("%s: prompt is required", filepath.Base(p))
		}
		vt.Name, vt.Path = strings.TrimSuffix(filepath.Base(p), ".yaml"), p
		out = append(out, vt)
	}
	return out, nil
}

// SaveVoiceTest writes a test back (used to record a reference).
func SaveVoiceTest(vt VoiceTest) error {
	if vt.Path == "" {
		return errors.New("voice test has no path")
	}
	b, err := yaml.Marshal(vt)
	if err != nil {
		return err
	}
	return writeAtomic(vt.Path, b)
}
