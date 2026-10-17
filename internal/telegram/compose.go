package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/kexiChanProjectProxy/gmailbot/internal/mailbuild"
	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

const (
	convReply     = "reply"
	convCompose   = "compose"
	convBlacklist = "blacklist"
)

// draft is the state of the /new and forward wizards, persisted in conv_state.
type draft struct {
	Mode      string   `json:"mode"` // new | forward
	AccountID int64    `json:"acc"`
	SrcMsgID  int64    `json:"src,omitempty"`
	To        string   `json:"to"`
	Subject   string   `json:"subj"`
	Body      string   `json:"body"`
	File      *fileRef `json:"file,omitempty"`
	Awaiting  string   `json:"await"` // to | subject | body | attach | ""
	WizardID  int      `json:"wiz"`
}

func (a *App) loadDraft(ctx context.Context, uid int64) (*draft, error) {
	c, err := a.Store.GetConv(ctx, uid)
	if err != nil || c.Kind != convCompose {
		return nil, uerr("This draft has expired. Start again with /new.")
	}
	var d draft
	if err := json.Unmarshal([]byte(c.Payload), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (a *App) saveDraft(ctx context.Context, uid int64, d *draft) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return a.Store.SetConv(ctx, uid, store.Conv{Kind: convCompose, Payload: string(b)})
}

func (a *App) cmdNew(ctx context.Context, uid, chatID int64) error {
	accs, err := a.requireAccounts(ctx, uid)
	if err != nil {
		return err
	}
	u, err := a.Store.GetUser(ctx, uid)
	if err != nil {
		return err
	}
	d := &draft{Mode: "new", AccountID: defaultAccount(u, accs).ID, Awaiting: "to"}
	return a.showWizard(ctx, uid, chatID, d)
}

func (a *App) startForward(ctx context.Context, uid, chatID int64, m store.Message) error {
	d := &draft{Mode: "forward", AccountID: m.AccountID, SrcMsgID: m.ID, Subject: m.Subject, Awaiting: "to"}
	return a.showWizard(ctx, uid, chatID, d)
}

var prompts = map[string]string{
	"to":      "👉 Send the recipient address(es), separated by commas.",
	"subject": "👉 Send the subject.",
	"body":    "👉 Send the message text.",
	"attach":  "👉 Send a file or photo to attach.",
	"":        "Tap ✅ Send when ready, or tap a field to change it.",
}

// showWizard re-sends the wizard at the bottom of the chat so it stays visible while typing.
func (a *App) showWizard(ctx context.Context, uid, chatID int64, d *draft) error {
	accs, err := a.Store.AccountsByUser(ctx, uid)
	if err != nil {
		return err
	}
	from := ""
	for _, x := range accs {
		if x.ID == d.AccountID {
			from = x.Email
		}
	}
	e := html.EscapeString
	or := func(s, empty string) string {
		if strings.TrimSpace(s) == "" {
			return "<i>" + empty + "</i>"
		}
		return e(s)
	}
	var sb strings.Builder
	if d.Mode == "forward" {
		sb.WriteString("➡️ <b>Forward:</b> " + e(d.Subject) + "\n")
	} else {
		sb.WriteString("✉️ <b>New email</b>\n")
	}
	sb.WriteString("<b>From:</b> " + e(from) + "\n")
	sb.WriteString("<b>To:</b> " + or(d.To, "—") + "\n")
	if d.Mode == "new" {
		sb.WriteString("<b>Subject:</b> " + or(d.Subject, "—") + "\n")
	}
	if d.File != nil {
		sb.WriteString("<b>Attachment:</b> " + e(d.File.Name) + "\n")
	}
	if d.Mode == "forward" {
		if src, err := a.Store.GetMessage(ctx, d.SrcMsgID); err == nil && len(src.Attachments) > 0 {
			fmt.Fprintf(&sb, "<b>Original attachments:</b> %d\n", len(src.Attachments))
		}
		sb.WriteString("\n" + or(mailparse.Truncate(d.Body, 1500), "(no note)") + "\n\n")
	} else {
		sb.WriteString("\n" + or(mailparse.Truncate(d.Body, 1500), "(empty message)") + "\n\n")
	}
	p := prompts[d.Awaiting]
	if d.Mode == "forward" && d.Awaiting == "body" {
		p = "👉 Send a note to add above the forwarded mail, or tap ✅ Send."
	}
	sb.WriteString(p)

	var rows [][]models.InlineKeyboardButton
	if len(accs) > 1 {
		rows = append(rows, row(btn("From: "+shortEmail(from)+" 🔄", "c:from")))
	}
	if d.Mode == "forward" {
		rows = append(rows, row(btn("To", "c:to"), btn("Note", "c:body")))
	} else {
		rows = append(rows, row(btn("To", "c:to"), btn("Subject", "c:subject"), btn("Message", "c:body")))
	}
	if d.File != nil {
		rows = append(rows, row(btn("📎 Remove "+mailparse.Truncate(d.File.Name, 30), "c:rmatt")))
	} else {
		rows = append(rows, row(btn("📎 Attach", "c:attach")))
	}
	rows = append(rows, row(btn("✅ Send", "c:send"), btn("✖ Cancel", "c:cancel")))

	a.deleteMsg(ctx, chatID, d.WizardID)
	m, err := a.send(ctx, chatID, sb.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, 0)
	if err != nil {
		return err
	}
	d.WizardID = m.ID
	return a.saveDraft(ctx, uid, d)
}

func (a *App) onComposeCallback(ctx context.Context, q *models.CallbackQuery, msg *models.Message) error {
	uid, chatID := q.From.ID, msg.Chat.ID
	d, err := a.loadDraft(ctx, uid)
	if err != nil {
		return err
	}
	if d.WizardID != msg.ID {
		return uerr("This draft is no longer active.")
	}
	op := strings.TrimPrefix(q.Data, "c:")
	switch op {
	case "from":
		accs, err := a.Store.AccountsByUser(ctx, uid)
		if err != nil || len(accs) == 0 {
			return uerr("No accounts connected.")
		}
		next := accs[0].ID
		for i, x := range accs {
			if x.ID == d.AccountID {
				next = accs[(i+1)%len(accs)].ID
			}
		}
		d.AccountID = next
	case "to", "subject", "body", "attach":
		d.Awaiting = op
	case "rmatt":
		d.File = nil
	case "cancel":
		a.answer(ctx, q, "", false)
		_ = a.Store.ClearConv(ctx, uid)
		return a.edit(ctx, chatID, msg.ID, "✖ Draft discarded.", nil)
	case "send":
		a.answer(ctx, q, "Sending…", false)
		return a.sendDraft(ctx, uid, chatID, d)
	default:
		return uerr("bad request")
	}
	a.answer(ctx, q, "", false)
	return a.showWizard(ctx, uid, chatID, d)
}

// fillDraft applies a user message to the field the wizard is waiting for.
func (a *App) fillDraft(ctx context.Context, uid, chatID int64, d *draft, text string, f *fileRef) error {
	if f != nil {
		if f.Size > int64(a.Cfg.Sync.MaxAttachmentMB)<<20 {
			a.sendText(ctx, chatID, fmt.Sprintf("⚠️ Attachments are limited to %d MB.", a.Cfg.Sync.MaxAttachmentMB))
		} else {
			d.File = f
			if d.Awaiting == "attach" {
				d.Awaiting = ""
			}
		}
	}
	if text != "" {
		switch d.Awaiting {
		case "to":
			if _, err := mailbuild.ParseRecipients(text); err != nil {
				a.sendText(ctx, chatID, "⚠️ "+html.EscapeString(err.Error())+". Try again.")
				return a.showWizard(ctx, uid, chatID, d)
			}
			d.To = text
			if d.Mode == "forward" {
				d.Awaiting = "body"
			} else if d.Subject == "" {
				d.Awaiting = "subject"
			} else if d.Body == "" {
				d.Awaiting = "body"
			} else {
				d.Awaiting = ""
			}
		case "subject":
			d.Subject = text
			if d.Body == "" {
				d.Awaiting = "body"
			} else {
				d.Awaiting = ""
			}
		case "body":
			d.Body = text
			d.Awaiting = ""
		default:
			// No field selected: further text continues the message.
			if d.Body == "" {
				d.Body = text
			} else {
				d.Body += "\n\n" + text
			}
		}
	}
	return a.showWizard(ctx, uid, chatID, d)
}

func (a *App) sendDraft(ctx context.Context, uid, chatID int64, d *draft) error {
	to, err := mailbuild.ParseRecipients(d.To)
	if err != nil {
		return uerr("Add a recipient first.")
	}
	if d.Mode == "new" && strings.TrimSpace(d.Body) == "" && strings.TrimSpace(d.Subject) == "" && d.File == nil {
		return uerr("The email is empty.")
	}
	acc, err := a.Store.GetAccount(ctx, d.AccountID)
	if err != nil || acc.TgUserID != uid {
		return uerr("The sending account was disconnected. Pick another.")
	}
	c, err := a.Gmail.For(ctx, acc)
	if err != nil {
		return err
	}
	var files []mailbuild.File
	if d.File != nil {
		data, err := a.download(ctx, d.File.FileID)
		if err != nil {
			return err
		}
		files = append(files, mailbuild.File{Name: d.File.Name, MIME: d.File.MIME, Content: data})
	}

	var raw []byte
	var src store.Message
	if d.Mode == "forward" {
		src, err = a.Store.GetMessage(ctx, d.SrcMsgID)
		if err != nil {
			return uerr("The original email is no longer cached.")
		}
		if len(src.Attachments) > 0 {
			atts, err := a.fetchAttachments(ctx, src)
			if err != nil {
				return fmt.Errorf("fetch original attachments: %w", err)
			}
			for _, x := range atts {
				files = append(files, mailbuild.File{Name: x.Filename, MIME: x.MIME, Content: x.Content})
			}
		}
		total := 0
		for _, f := range files {
			total += len(f.Content)
		}
		if total > 25<<20 {
			return uerr("Attachments exceed Gmail's 25 MB limit.")
		}
		raw, err = mailbuild.Forward(acc.Email, to, original(src), d.Body, files)
	} else {
		raw, err = mailbuild.New(acc.Email, to, d.Subject, d.Body, files)
	}
	if err != nil {
		return err
	}
	if _, err := c.Send(ctx, raw, ""); err != nil {
		return err
	}
	_ = a.Store.ClearConv(ctx, uid)
	if d.Mode == "forward" {
		if err := a.Store.SetStatus(ctx, src.ID, "forwarded"); err == nil {
			_ = a.Refresh(ctx, src.ID)
		}
	}
	recips := make([]string, len(to))
	for i, x := range to {
		recips[i] = x.Address
	}
	return a.edit(ctx, chatID, d.WizardID, fmt.Sprintf("✅ Sent from %s to %s.\n<b>Subject:</b> %s",
		html.EscapeString(acc.Email), html.EscapeString(strings.Join(recips, ", ")), html.EscapeString(d.Subject)), nil)
}

// onMessage handles non-command messages: native replies to mail, wizard input and prompts.
func (a *App) onMessage(ctx context.Context, m *models.Message) {
	uid, chatID := m.From.ID, m.Chat.ID
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = strings.TrimSpace(m.Caption)
	}
	f := fileFromMessage(m)

	err := func() error {
		// 1. Telegram-native reply on an email message.
		if r := m.ReplyToMessage; r != nil {
			mail, err := a.Store.MessageByTelegram(ctx, chatID, r.ID)
			if err == nil {
				_ = a.Store.ClearConv(ctx, uid)
				return a.sendReply(ctx, chatID, mail, text, f, m.ID)
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		// 2. Pending conversation.
		conv, err := a.Store.GetConv(ctx, uid)
		if errors.Is(err, store.ErrNotFound) {
			a.sendText(ctx, chatID, "To answer an email, use Telegram's <i>Reply</i> on it. To write a new one, use /new. See /help.")
			return nil
		}
		if err != nil {
			return err
		}
		switch conv.Kind {
		case convReply:
			id, _ := strconv.ParseInt(conv.Payload, 10, 64)
			mail, err := a.ownedMessage(ctx, chatID, id)
			if err != nil {
				return err
			}
			_ = a.Store.ClearConv(ctx, uid)
			return a.sendReply(ctx, chatID, mail, text, f, m.ID)
		case convCompose:
			d, err := a.loadDraft(ctx, uid)
			if err != nil {
				return err
			}
			return a.fillDraft(ctx, uid, chatID, d, text, f)
		case convBlacklist:
			_ = a.Store.ClearConv(ctx, uid)
			p := strings.ToLower(strings.TrimSpace(text))
			if p == "" || strings.ContainsAny(p, " ,;") || (!strings.Contains(p, "@") && !strings.Contains(p, ".")) {
				return uerr("That doesn't look like an address or domain.")
			}
			if err := a.Store.AddBlacklist(ctx, uid, p); err != nil {
				return err
			}
			a.sendText(ctx, chatID, "🚫 Blocked <code>"+html.EscapeString(p)+"</code>.")
			if editID, _ := strconv.Atoi(conv.Payload); editID != 0 {
				_ = a.blacklistView(ctx, uid, chatID, editID)
			}
			return nil
		}
		return nil
	}()
	if err != nil {
		a.Log.Error("message", "err", err)
		a.sendText(ctx, chatID, "⚠️ "+html.EscapeString(userError(err)))
	}
}
