// Package mailparse turns raw RFC 822 mail into the fields the bot displays and stores.
package mailparse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jhillyerd/enmime/v2"
)

type Attachment struct {
	Filename string
	MIME     string
	Content  []byte
}

type Parsed struct {
	FromName    string
	FromAddr    string
	ReplyTo     string
	To          []string
	Cc          []string
	Subject     string
	Date        time.Time
	DateHdr     string
	Text        string
	MessageID   string
	InReplyTo   string
	References  string
	Attachments []Attachment
}

// Parse reads raw mail. HTML-only bodies are down-converted to text.
func Parse(raw []byte) (*Parsed, error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	p := &Parsed{
		Subject:    strings.TrimSpace(env.GetHeader("Subject")),
		DateHdr:    env.GetHeader("Date"),
		Text:       strings.TrimSpace(normalizeNewlines(env.Text)),
		MessageID:  strings.TrimSpace(env.GetHeader("Message-ID")),
		InReplyTo:  strings.TrimSpace(env.GetHeader("In-Reply-To")),
		References: strings.Join(strings.Fields(env.GetHeader("References")), " "),
	}
	if d, err := mail.ParseDate(p.DateHdr); err == nil {
		p.Date = d
	}
	if from, err := env.AddressList("From"); err == nil && len(from) > 0 {
		p.FromName, p.FromAddr = from[0].Name, from[0].Address
	} else {
		p.FromAddr = strings.TrimSpace(env.GetHeader("From"))
	}
	if rt, err := env.AddressList("Reply-To"); err == nil && len(rt) > 0 {
		p.ReplyTo = rt[0].Address
	}
	p.To = addrs(env, "To")
	p.Cc = addrs(env, "Cc")

	for _, part := range env.Attachments {
		p.Attachments = append(p.Attachments, toAttachment(part, len(p.Attachments)))
	}
	for _, part := range env.Inlines {
		if part.ContentID != "" { // embedded images referenced from the HTML body
			continue
		}
		p.Attachments = append(p.Attachments, toAttachment(part, len(p.Attachments)))
	}
	return p, nil
}

func toAttachment(part *enmime.Part, i int) Attachment {
	name := part.FileName
	if name == "" {
		name = fmt.Sprintf("attachment-%d", i+1)
	}
	ct := part.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return Attachment{Filename: name, MIME: ct, Content: part.Content}
}

func addrs(env *enmime.Envelope, key string) []string {
	list, err := env.AddressList(key)
	if err != nil {
		if h := strings.TrimSpace(env.GetHeader(key)); h != "" {
			return []string{h}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// collapse runs of 3+ blank lines that html down-conversion tends to produce
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// NormalizeMessageID canonicalises a Message-ID header for comparison.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.ToLower(strings.TrimSpace(id))
}

// DedupKey identifies the same mail across mailboxes. Gmail auto-forwarding preserves
// the original Message-ID, so that is the primary key. Mails without one fall back to
// a content hash over sender, subject, minute-truncated date and body.
func DedupKey(p *Parsed) string {
	if id := NormalizeMessageID(p.MessageID); id != "" {
		return "mid:" + id
	}
	body := sha256.Sum256([]byte(strings.TrimSpace(p.Text)))
	date := ""
	if !p.Date.IsZero() {
		date = p.Date.UTC().Truncate(time.Minute).Format(time.RFC3339)
	}
	h := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(p.FromAddr), strings.TrimSpace(p.Subject), date, hex.EncodeToString(body[:]),
	}, "\x00")))
	return "h:" + hex.EncodeToString(h[:])
}

// Snippet returns a single-paragraph preview of at most n runes.
func Snippet(text string, n int) string {
	s := strings.Join(strings.Fields(text), " ")
	return Truncate(s, n)
}

// Truncate shortens s to at most n runes, appending an ellipsis when cut.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
