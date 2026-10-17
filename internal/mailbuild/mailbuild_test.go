package mailbuild

import (
	"strings"
	"testing"

	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
)

var orig = Original{
	FromName: "Alice", FromAddr: "alice@example.com", To: "bob@example.com",
	Subject: "Lunch?", DateHdr: "Mon, 12 Oct 2026 09:30:00 +0000", Body: "Noon?\nOr 1pm.",
	RFCMessageID: "<m2@example.com>", References: "<m1@example.com>",
}

func TestReplyThreading(t *testing.T) {
	raw, err := Reply("bob@example.com", orig, "Noon works.", []File{{Name: "map.png", MIME: "image/png", Content: []byte("PNG")}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := mailparse.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "Re: Lunch?" {
		t.Errorf("subject = %q", p.Subject)
	}
	if p.InReplyTo != "<m2@example.com>" || p.References != "<m1@example.com> <m2@example.com>" {
		t.Errorf("in-reply-to=%q references=%q", p.InReplyTo, p.References)
	}
	if len(p.To) != 1 || !strings.Contains(p.To[0], "alice@example.com") {
		t.Errorf("to = %v", p.To)
	}
	if !strings.Contains(p.Text, "Noon works.") || !strings.Contains(p.Text, "> Or 1pm.") {
		t.Errorf("body = %q", p.Text)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "map.png" {
		t.Errorf("attachments = %+v", p.Attachments)
	}
}

func TestReplyPrefersReplyTo(t *testing.T) {
	o := orig
	o.ReplyTo = "list@example.com"
	if got := ReplyRecipient(o).Address; got != "list@example.com" {
		t.Errorf("got %q", got)
	}
	o.Subject = "RE: Lunch?"
	raw, _ := Reply("bob@example.com", o, "x", nil)
	p, _ := mailparse.Parse(raw)
	if p.Subject != "RE: Lunch?" {
		t.Errorf("subject double-prefixed: %q", p.Subject)
	}
}

func TestForward(t *testing.T) {
	to, _ := ParseRecipients("carol@example.com; Dan <dan@example.com>")
	raw, err := Forward("bob@example.com", to, orig, "FYI", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := mailparse.Parse(raw)
	if p.Subject != "Fwd: Lunch?" || len(p.To) != 2 {
		t.Errorf("subject=%q to=%v", p.Subject, p.To)
	}
	if !strings.HasPrefix(p.Text, "FYI") || !strings.Contains(p.Text, "Forwarded message") || !strings.Contains(p.Text, "From: Alice <alice@example.com>") {
		t.Errorf("body = %q", p.Text)
	}
}

func TestParseRecipients(t *testing.T) {
	cases := map[string]int{
		"a@x.com":                  1,
		"a@x.com, b@y.com":         2,
		"a@x.com b@y.com":          2,
		"A <a@x.com>; B <b@y.com>": 2,
	}
	for in, n := range cases {
		got, err := ParseRecipients(in)
		if err != nil || len(got) != n {
			t.Errorf("ParseRecipients(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "not an address", "a@x.com, nope"} {
		if _, err := ParseRecipients(bad); err == nil {
			t.Errorf("ParseRecipients(%q) should fail", bad)
		}
	}
}
