// Package notify renders cached mail as Telegram HTML messages with inline keyboards.
package notify

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// View selects which keyboard is attached to a mail message.
type View int

const (
	ViewMain View = iota
	ViewActions
)

// Callback data prefixes. Format: "m:<action>:<message id>[:<arg>]", always under 64 bytes.
const (
	ActMore    = "more"
	ActLess    = "less"
	ActActions = "act"
	ActBack    = "back"
	ActReply   = "rep"
	ActForward = "fwd"
	ActArchive = "arc"
	ActDelete  = "del"
	ActSpam    = "spam"
	ActAttach  = "att"
	ActText    = "txt"
)

func CB(action string, msgID int64, arg ...string) string {
	s := "m:" + action + ":" + strconv.FormatInt(msgID, 10)
	if len(arg) > 0 {
		s += ":" + arg[0]
	}
	return s
}

// ParseCB splits callback data produced by CB.
func ParseCB(data string) (action string, msgID int64, arg string, ok bool) {
	parts := strings.SplitN(data, ":", 4)
	if len(parts) < 3 || parts[0] != "m" {
		return "", 0, "", false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	if len(parts) == 4 {
		arg = parts[3]
	}
	return parts[1], id, arg, true
}

// Input is everything needed to render one mail.
type Input struct {
	Msg          store.Message
	AccountEmail string
	ShowAccount  bool     // user has more than one linked account
	AlsoIn       []string // other accounts that received the same mail
	View         View
	MaxBody      int
	SnippetLen   int
	GmailURL     string
}

var statusLabel = map[string]string{
	"archived":  "📥 Archived",
	"deleted":   "🗑 Moved to Trash",
	"spam":      "🚫 Reported as spam",
	"replied":   "↩️ Replied",
	"forwarded": "➡️ Forwarded",
}

// Render returns the HTML text and keyboard for a mail message.
func Render(in Input) (string, *models.InlineKeyboardMarkup) {
	m := in.Msg
	if in.MaxBody <= 0 {
		in.MaxBody = 3500
	}
	if in.SnippetLen <= 0 {
		in.SnippetLen = 300
	}
	e := html.EscapeString
	var sb strings.Builder
	if in.ShowAccount || len(in.AlsoIn) > 0 {
		sb.WriteString("📧 <b>" + e(in.AccountEmail) + "</b>")
		if len(in.AlsoIn) > 0 {
			sb.WriteString(" · <i>also in " + e(strings.Join(in.AlsoIn, ", ")) + "</i>")
		}
		sb.WriteString("\n")
	}
	from := m.FromAddr
	if m.FromName != "" {
		from = m.FromName + " <" + m.FromAddr + ">"
	}
	// Header fields are capped so header + body always fit Telegram's 4096-char limit.
	sb.WriteString("<b>From:</b> " + e(mailparse.Truncate(from, 150)) + "\n")
	if m.Expanded {
		if m.To != "" {
			sb.WriteString("<b>To:</b> " + e(mailparse.Truncate(m.To, 150)) + "\n")
		}
		if m.Cc != "" {
			sb.WriteString("<b>Cc:</b> " + e(mailparse.Truncate(m.Cc, 150)) + "\n")
		}
		if m.DateHdr != "" {
			sb.WriteString("<b>Date:</b> " + e(m.DateHdr) + "\n")
		}
	}
	subj := m.Subject
	if subj == "" {
		subj = "(no subject)"
	}
	sb.WriteString("<b>Subject:</b> " + e(mailparse.Truncate(subj, 200)) + "\n\n")
	header := sb.String()

	var foot strings.Builder
	if n := len(m.Attachments); n > 0 {
		names := make([]string, 0, n)
		for _, a := range m.Attachments {
			names = append(names, a.Filename)
		}
		label := "attachment"
		if n > 1 {
			label = "attachments"
		}
		foot.WriteString(fmt.Sprintf("\n\n📎 %d %s: %s", n, label, e(mailparse.Truncate(strings.Join(names, ", "), 200))))
	}
	if st, ok := statusLabel[m.Status]; ok {
		foot.WriteString("\n\n<i>" + st + "</i>")
	}
	footer := foot.String()

	// Telegram limits the visible text to 4096 characters; the body gets what is left.
	budget := min(in.MaxBody, TelegramLimit-16-VisibleLen(header)-VisibleLen(footer))
	truncated := false
	var body string
	if m.Expanded {
		body = m.BodyText
		if body == "" {
			body = m.Snippet
		}
		if len([]rune(body)) > budget {
			truncated = true
			body = mailparse.Truncate(body, budget)
		}
	} else {
		body = m.Snippet
		if body == "" {
			body = mailparse.Snippet(m.BodyText, in.SnippetLen)
		}
		body = mailparse.Truncate(body, min(in.SnippetLen, budget))
	}
	return header + e(body) + footer, keyboard(in, truncated)
}

// TelegramLimit is the maximum visible length of a message.
const TelegramLimit = 4096

var tagRE = regexp.MustCompile(`<[^>]*>`)

// VisibleLen approximates how Telegram counts an HTML message: tags removed, entities decoded.
func VisibleLen(s string) int {
	return len([]rune(html.UnescapeString(tagRE.ReplaceAllString(s, ""))))
}

const maxAttachmentButtons = 20

func btn(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

func keyboard(in Input, truncated bool) *models.InlineKeyboardMarkup {
	m := in.Msg
	var rows [][]models.InlineKeyboardButton
	gone := m.Status == "archived" || m.Status == "deleted" || m.Status == "spam"

	if in.View == ViewActions && !gone {
		rows = append(rows,
			[]models.InlineKeyboardButton{btn("↩️ Reply", CB(ActReply, m.ID)), btn("➡️ Forward", CB(ActForward, m.ID))},
			[]models.InlineKeyboardButton{btn("📥 Archive", CB(ActArchive, m.ID)), btn("🗑 Delete", CB(ActDelete, m.ID)), btn("🚫 Spam", CB(ActSpam, m.ID))},
		)
		last := []models.InlineKeyboardButton{}
		if in.GmailURL != "" {
			last = append(last, models.InlineKeyboardButton{Text: "🔗 Open in Gmail", URL: in.GmailURL})
		}
		last = append(last, btn("‹ Back", CB(ActBack, m.ID)))
		rows = append(rows, last)
		return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}

	top := []models.InlineKeyboardButton{}
	if m.Expanded {
		top = append(top, btn("Show less", CB(ActLess, m.ID)))
	} else {
		top = append(top, btn("Show more", CB(ActMore, m.ID)))
	}
	if !gone {
		top = append(top, btn("Actions", CB(ActActions, m.ID)))
	}
	rows = append(rows, top)
	if m.Expanded {
		if truncated {
			rows = append(rows, []models.InlineKeyboardButton{btn("📄 Full text as .txt", CB(ActText, m.ID))})
		}
		for i, a := range m.Attachments {
			if i == maxAttachmentButtons {
				break // the rest remain reachable via "Open in Gmail"
			}
			rows = append(rows, []models.InlineKeyboardButton{
				btn("📎 "+mailparse.Truncate(a.Filename, 40)+" ("+humanSize(a.Size)+")", CB(ActAttach, m.ID, strconv.Itoa(a.Idx))),
			})
		}
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
