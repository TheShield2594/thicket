package channel

import "testing"

func TestVoiceFormat(t *testing.T) {
	for _, c := range []struct {
		b    string
		ct   string
		want string
	}{
		{"OggS\x00\x02", "audio/mpeg", "ogg"}, // the bytes win over a wrong header
		{"ID3\x04", "", "mp3"},
		{"\xff\xfb\x90\x00", "", "mp3"},
		{"RIFF....WAVE", "", "wav"},
		{"fLaC", "", "flac"},
		{"\xff\xf1\x50\x80", "", "aac"},
		{"????", "audio/ogg; codecs=opus", "ogg"},
		{"????", "audio/opus", "ogg"},
		{"????", "audio/mpeg", "mp3"},
		{"????", "application/octet-stream", ""},
	} {
		if got := VoiceFormat([]byte(c.b), c.ct); got != c.want {
			t.Errorf("VoiceFormat(%q, %q) = %q, want %q", c.b, c.ct, got, c.want)
		}
	}
	if VoiceContentType("ogg") != "audio/ogg" || VoiceContentType("mp3") != "audio/mpeg" {
		t.Error("content types")
	}
}
