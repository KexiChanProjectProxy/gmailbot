package mailparse

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, name string) *Parsed {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return p
}

func TestParsePlain(t *testing.T) {
	p := load(t, "plain.eml")
	if p.FromName != "Alice Example" || p.FromAddr != "alice@example.com" {
		t.Errorf("from = %q <%q>", p.FromName, p.FromAddr)
	}
	if p.Subject != "Quarterly numbers" {
		t.Errorf("subject = %q", p.Subject)
	}
	if !strings.HasPrefix(p.Text, "Hi Bob,") || strings.Contains(p.Text, "\r") {
		t.Errorf("text = %q", p.Text)
	}
	if p.Date.IsZero() {
		t.Error("date not parsed")
	}
	if len(p.Attachments) != 0 {
		t.Errorf("attachments = %d", len(p.Attachments))
	}
}

func TestParseHTMLOnly(t *testing.T) {
	p := load(t, "html_only.eml")
	if !strings.Contains(p.Text, "#42") || strings.Contains(p.Text, "<b>") {
		t.Errorf("html not down-converted: %q", p.Text)
	}
}

func TestParseMultipart(t *testing.T) {
	p := load(t, "multipart.eml")
	if p.Text != "See attached." {
		t.Errorf("text = %q", p.Text)
	}
	if len(p.Attachments) != 2 || p.Attachments[0].Filename != "report.pdf" || p.Attachments[1].Filename != "data.csv" {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	if !strings.HasPrefix(string(p.Attachments[0].Content), "%PDF") {
		t.Errorf("pdf content not decoded: %q", p.Attachments[0].Content)
	}
	if len(p.To) != 2 || len(p.Cc) != 1 {
		t.Errorf("to=%v cc=%v", p.To, p.Cc)
	}
	if p.References != "<root@example.org> <prev@example.org>" {
		t.Errorf("references = %q", p.References)
	}
}

func TestParseLatin1(t *testing.T) {
	p := load(t, "latin1.eml")
	if p.Subject != "Café mañana" || p.FromName != "José" {
		t.Errorf("subject=%q from=%q", p.Subject, p.FromName)
	}
	if !strings.Contains(p.Text, "café") {
		t.Errorf("text = %q", p.Text)
	}
}

func TestNormalizeMessageID(t *testing.T) {
	for _, in := range []string{"<ABC123@Mail.Example.com>", "  abc123@mail.example.com ", "< abc123@MAIL.example.com >"} {
		if got := NormalizeMessageID(in); got != "abc123@mail.example.com" {
			t.Errorf("NormalizeMessageID(%q) = %q", in, got)
		}
	}
}

func TestDedupKey(t *testing.T) {
	a := load(t, "plain.eml")
	b := *a
	b.MessageID = "abc123@mail.example.com" // same id, different formatting
	if DedupKey(a) != DedupKey(&b) || !strings.HasPrefix(DedupKey(a), "mid:") {
		t.Errorf("message-id keys differ: %q vs %q", DedupKey(a), DedupKey(&b))
	}

	// Without a Message-ID the content hash is used and ignores sub-minute date drift.
	h := load(t, "html_only.eml")
	h2 := *h
	h2.Date = h.Date.Add(20e9)
	if !strings.HasPrefix(DedupKey(h), "h:") || DedupKey(h) != DedupKey(&h2) {
		t.Errorf("fallback keys differ")
	}
	h3 := *h
	h3.Text += " changed"
	if DedupKey(h) == DedupKey(&h3) {
		t.Error("different bodies must not collide")
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("héllo world", 5); got != "héll…" {
		t.Errorf("got %q", got)
	}
	if got := Snippet("a\n\n b   c", 10); got != "a b c" {
		t.Errorf("got %q", got)
	}
}
