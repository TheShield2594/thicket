package mail

import "testing"

func TestAllowEntry(t *testing.T) {
	good := map[string]string{
		" Shop@Vendor.Example ":        "shop@vendor.example",
		"@Vendor.Example":              "@vendor.example",
		"<a.b+tag@sub.vendor.example>": "a.b+tag@sub.vendor.example",
	}
	for in, want := range good {
		if got, err := AllowEntry(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "@", "*@vendor.example", "@*.vendor.example", "Shop <shop@vendor.example>", "no at sign", "@-x.example", "@localhost", "a@b@c"} {
		if got, err := AllowEntry(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestAllowed(t *testing.T) {
	list := []string{"shop@vendor.example", "@trusted.example"}
	for addr, want := range map[string]bool{
		"shop@vendor.example":         true,
		"SHOP@Vendor.example":         true,
		"other@vendor.example":        false,
		"anyone@trusted.example":      true,
		"anyone@sub.trusted.example":  false, // a domain entry doesn't cover subdomains
		"anyone@trusted.example.evil": false,
		"nobody":                      false,
	} {
		if Allowed(list, addr) != want {
			t.Errorf("%s: want %v", addr, want)
		}
	}
}

func TestSettingsNeverPlaintext(t *testing.T) {
	s := Settings{IMAPHost: "imap.x.example", SMTPHost: "smtp.x.example", Username: "u"}
	s.Defaults()
	if s.IMAPPort != 993 || s.SMTPPort != 465 || s.Check("imap") != nil {
		t.Fatalf("%+v", s)
	}
	s.SMTPSecurity = "none"
	if s.Check("imap") == nil {
		t.Fatal("plaintext SMTP accepted")
	}
	if (Settings{SessionURL: "http://jmap.x.example/session"}).Check("jmap") == nil {
		t.Fatal("a JMAP session over http accepted")
	}
	if (Settings{SessionURL: "https://jmap.x.example/session"}).Check("jmap") != nil {
		t.Fatal("https JMAP refused")
	}
	if (Settings{}).Check("pop3") == nil {
		t.Fatal("unknown backend accepted")
	}
}
