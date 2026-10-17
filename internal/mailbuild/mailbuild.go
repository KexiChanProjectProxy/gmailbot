// Package mailbuild composes outgoing RFC 822 messages: new mail, replies and forwards.
package mailbuild

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jhillyerd/enmime/v2"
)

type File struct {
	Name    string
	MIME    string
	Content []byte
}

// Original is the subset of a received mail needed to reply to or forward it.
type Original struct {
	FromName     string
	FromAddr     string
	ReplyTo      string
	To           string
	Cc           string
	Subject      string
	DateHdr      string
	Body         string
	RFCMessageID string
	References   string
}

// ParseRecipients accepts comma, semicolon or whitespace separated addresses.
func ParseRecipients(s string) ([]mail.Address, error) {
	s = strings.NewReplacer(";", ",", "\n", ",").Replace(s)
	if list, err := mail.ParseAddressList(s); err == nil && len(list) > 0 {
		out := make([]mail.Address, len(list))
		for i, a := range list {
			out[i] = *a
		}
		return out, nil
	}
	var out []mail.Address
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		a, err := mail.ParseAddress(f)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q", f)
		}
		out = append(out, *a)
	}
	if len(out) == 0 {
		return nil, errors.New("no recipients")
	}
	return out, nil
}

func encode(b enmime.MailBuilder) ([]byte, error) {
	part, err := b.Build()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := part.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func withFiles(b enmime.MailBuilder, files []File) enmime.MailBuilder {
	for _, f := range files {
		ct := f.MIME
		if ct == "" {
			ct = "application/octet-stream"
		}
		b = b.AddAttachment(f.Content, ct, f.Name)
	}
	return b
}

func body(b enmime.MailBuilder, text string) enmime.MailBuilder {
	return b.Text([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
}

// New builds a fresh message.
func New(from string, to []mail.Address, subject, text string, files []File) ([]byte, error) {
	b := enmime.Builder().From("", from).ToAddrs(to).Subject(subject)
	return encode(withFiles(body(b, text), files))
}

func prefixed(prefix, subject string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), strings.ToLower(prefix)) {
		return subject
	}
	return prefix + " " + subject
}

// ReplyRecipient picks Reply-To when present, otherwise From.
func ReplyRecipient(o Original) mail.Address {
	if o.ReplyTo != "" {
		return mail.Address{Address: o.ReplyTo}
	}
	return mail.Address{Name: o.FromName, Address: o.FromAddr}
}

// Reply builds a threaded reply to o: Re: subject, In-Reply-To/References and a quoted original.
func Reply(from string, o Original, text string, files []File) ([]byte, error) {
	b := enmime.Builder().From("", from).ToAddrs([]mail.Address{ReplyRecipient(o)}).Subject(prefixed("Re:", o.Subject))
	if o.RFCMessageID != "" {
		b = b.Header("In-Reply-To", o.RFCMessageID)
		refs := strings.TrimSpace(o.References + " " + o.RFCMessageID)
		b = b.Header("References", refs)
	}
	return encode(withFiles(body(b, text+"\n\n"+quote(o)), files))
}

func quote(o Original) string {
	who := o.FromAddr
	if o.FromName != "" {
		who = fmt.Sprintf("%s <%s>", o.FromName, o.FromAddr)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "On %s, %s wrote:\n", o.DateHdr, who)
	for _, line := range strings.Split(o.Body, "\n") {
		sb.WriteString("> ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ForwardBody renders the conventional Gmail forwarded-message block.
func ForwardBody(o Original, note string) string {
	who := o.FromAddr
	if o.FromName != "" {
		who = fmt.Sprintf("%s <%s>", o.FromName, o.FromAddr)
	}
	var sb strings.Builder
	if note != "" {
		sb.WriteString(note)
		sb.WriteString("\n\n")
	}
	sb.WriteString("---------- Forwarded message ---------\n")
	fmt.Fprintf(&sb, "From: %s\nDate: %s\nSubject: %s\nTo: %s\n", who, o.DateHdr, o.Subject, o.To)
	if o.Cc != "" {
		fmt.Fprintf(&sb, "Cc: %s\n", o.Cc)
	}
	sb.WriteString("\n")
	sb.WriteString(o.Body)
	return sb.String()
}

// Forward builds a forward of o to new recipients, carrying the original attachments.
func Forward(from string, to []mail.Address, o Original, note string, files []File) ([]byte, error) {
	b := enmime.Builder().From("", from).ToAddrs(to).Subject(prefixed("Fwd:", o.Subject))
	return encode(withFiles(body(b, ForwardBody(o, note)), files))
}
