package notify

import (
	"strings"
	"testing"

	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

func TestCallbackRoundTrip(t *testing.T) {
	data := CB(ActAttach, 1234567890123, "3")
	if len(data) > 64 {
		t.Fatalf("callback data too long: %d", len(data))
	}
	a, id, arg, ok := ParseCB(data)
	if !ok || a != ActAttach || id != 1234567890123 || arg != "3" {
		t.Errorf("got %v %v %v %v", a, id, arg, ok)
	}
	if _, _, _, ok := ParseCB("x:1"); ok {
		t.Error("bad data parsed")
	}
}

func TestRenderEscapesAndTruncates(t *testing.T) {
	m := store.Message{ID: 7, FromName: "Eve <script>", FromAddr: "eve@x.com", Subject: "a & b",
		BodyText: strings.Repeat("x", 5000), Snippet: "hello <b>", Expanded: true,
		Attachments: []store.Attachment{{Idx: 0, Filename: "f.pdf", Size: 2048}}}
	text, kb := Render(Input{Msg: m, AccountEmail: "me@gmail.com", MaxBody: 100})
	if strings.Contains(text, "<script>") || !strings.Contains(text, "a &amp; b") {
		t.Errorf("not escaped: %s", text)
	}
	if strings.Count(text, "x") > 100 {
		t.Error("body not truncated")
	}
	var labels []string
	for _, r := range kb.InlineKeyboard {
		for _, b := range r {
			labels = append(labels, b.Text)
		}
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"Show less", "Actions", ".txt", "f.pdf (2 KB)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("keyboard %q missing %q", joined, want)
		}
	}
	if strings.Contains(text, "me@gmail.com") {
		t.Error("account line shown for single-account user")
	}
}

func TestRenderAlsoInAndStatus(t *testing.T) {
	m := store.Message{ID: 1, FromAddr: "a@x.com", Snippet: "hi", Status: "archived"}
	text, kb := Render(Input{Msg: m, AccountEmail: "a@gmail.com", AlsoIn: []string{"b@gmail.com"}})
	if !strings.Contains(text, "also in b@gmail.com") || !strings.Contains(text, "Archived") {
		t.Errorf("text = %s", text)
	}
	for _, r := range kb.InlineKeyboard {
		for _, b := range r {
			if b.Text == "Actions" {
				t.Error("Actions offered on archived mail")
			}
		}
	}
}

func TestRenderFitsTelegramLimit(t *testing.T) {
	long := strings.Repeat("y", 5000)
	m := store.Message{ID: 1, FromName: long, FromAddr: "a@x.com", To: long, Cc: long, Subject: long,
		BodyText: long, Expanded: true}
	for i := 0; i < 40; i++ {
		m.Attachments = append(m.Attachments, store.Attachment{Idx: i, Filename: long})
	}
	text, kb := Render(Input{Msg: m, AccountEmail: "a@gmail.com", AlsoIn: []string{"b@gmail.com"}})
	if n := VisibleLen(text); n > TelegramLimit {
		t.Errorf("rendered %d chars, over Telegram's limit", n)
	}
	if len(kb.InlineKeyboard) > maxAttachmentButtons+3 {
		t.Errorf("keyboard has %d rows", len(kb.InlineKeyboard))
	}
}
