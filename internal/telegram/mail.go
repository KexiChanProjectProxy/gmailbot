package telegram

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"strconv"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/mailbuild"
	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
	"github.com/kexiChanProjectProxy/gmailbot/internal/notify"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// ownedMessage loads a cached mail and checks it belongs to the chat.
func (a *App) ownedMessage(ctx context.Context, chatID, id int64) (store.Message, error) {
	m, err := a.Store.GetMessage(ctx, id)
	if err != nil || m.ChatID != chatID {
		return store.Message{}, uerr("This email is no longer cached (mail is kept for 7 days).")
	}
	return m, nil
}

func (a *App) clientFor(ctx context.Context, accountID int64) (*gmailx.Client, store.Account, error) {
	acc, err := a.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, acc, uerr("The Gmail account for this email was disconnected.")
	}
	c, err := a.Gmail.For(ctx, acc)
	return c, acc, err
}

func (a *App) onMailCallback(ctx context.Context, q *models.CallbackQuery, msg *models.Message) error {
	action, id, arg, ok := notify.ParseCB(q.Data)
	if !ok {
		return uerr("bad request")
	}
	chatID := msg.Chat.ID
	m, err := a.ownedMessage(ctx, chatID, id)
	if err != nil {
		return err
	}
	switch action {
	case notify.ActMore, notify.ActLess:
		if err := a.Store.SetExpanded(ctx, m.ID, action == notify.ActMore); err != nil {
			return err
		}
		a.answer(ctx, q, "", false)
		return a.refresh(ctx, m.ID, notify.ViewMain)
	case notify.ActActions, notify.ActBack:
		view := notify.ViewMain
		if action == notify.ActActions {
			view = notify.ViewActions
		}
		in, err := a.renderInput(ctx, m.ID, view)
		if err != nil {
			return err
		}
		_, k := notify.Render(in)
		a.answer(ctx, q, "", false)
		return a.editMarkup(ctx, chatID, msg.ID, k)
	case notify.ActArchive, notify.ActDelete, notify.ActSpam:
		return a.applyAction(ctx, q, m, action)
	case notify.ActReply:
		if err := a.Store.SetConv(ctx, q.From.ID, store.Conv{Kind: convReply, Payload: strconv.FormatInt(m.ID, 10)}); err != nil {
			return err
		}
		a.answer(ctx, q, "", false)
		to := mailbuild.ReplyRecipient(original(m))
		_, err := a.send(ctx, chatID, "✍️ Write your reply to <b>"+html.EscapeString(to.String())+"</b>. Attach a file by sending it with a caption. Any command cancels.", nil, m.TgMessageID)
		return err
	case notify.ActForward:
		a.answer(ctx, q, "", false)
		return a.startForward(ctx, q.From.ID, chatID, m)
	case notify.ActAttach:
		idx, err := strconv.Atoi(arg)
		if err != nil {
			return uerr("bad attachment")
		}
		a.answer(ctx, q, "Downloading…", false)
		return a.sendAttachment(ctx, m, idx)
	case notify.ActText:
		a.answer(ctx, q, "", false)
		_, err := a.B.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:          chatID,
			Document:        &models.InputFileUpload{Filename: safeFilename(m.Subject, "email") + ".txt", Data: bytes.NewReader([]byte(m.BodyText))},
			ReplyParameters: &models.ReplyParameters{MessageID: m.TgMessageID, AllowSendingWithoutReply: true},
		})
		return err
	}
	return uerr("unknown action")
}

var actionStatus = map[string]string{notify.ActArchive: "archived", notify.ActDelete: "deleted", notify.ActSpam: "spam"}
var actionDone = map[string]string{notify.ActArchive: "📥 Archived", notify.ActDelete: "🗑 Moved to Trash", notify.ActSpam: "🚫 Reported as spam"}

// applyAction archives, trashes or reports the mail; with dedup_actions_all_copies it does the
// same to suppressed copies in the user's other accounts.
func (a *App) applyAction(ctx context.Context, q *models.CallbackQuery, m store.Message, action string) error {
	targets := []store.Message{m}
	if a.Cfg.Sync.DedupActionsAllCopies {
		dups, err := a.Store.Duplicates(ctx, m.ID)
		if err != nil {
			return err
		}
		targets = append(targets, dups...)
	}
	for i, t := range targets {
		c, _, err := a.clientFor(ctx, t.AccountID)
		if err != nil {
			if i == 0 {
				return err
			}
			continue
		}
		switch action {
		case notify.ActArchive:
			err = c.Archive(ctx, t.GmailID)
		case notify.ActDelete:
			err = c.Trash(ctx, t.GmailID)
		case notify.ActSpam:
			err = c.Spam(ctx, t.GmailID)
		}
		if err != nil {
			if i == 0 {
				return err
			}
			a.Log.Warn("action on duplicate copy", "action", action, "gmail_id", t.GmailID, "err", err)
			continue
		}
		if err := a.Store.SetStatus(ctx, t.ID, actionStatus[action]); err != nil {
			return err
		}
	}
	a.answer(ctx, q, actionDone[action], false)
	return a.refresh(ctx, m.ID, notify.ViewMain)
}

// fetchAttachments re-downloads the raw mail and returns its attachments.
func (a *App) fetchAttachments(ctx context.Context, m store.Message) ([]mailparse.Attachment, error) {
	c, _, err := a.clientFor(ctx, m.AccountID)
	if err != nil {
		return nil, err
	}
	raw, err := c.GetRaw(ctx, m.GmailID)
	if err != nil {
		return nil, err
	}
	p, err := mailparse.Parse(raw.Raw)
	if err != nil {
		return nil, err
	}
	return p.Attachments, nil
}

func (a *App) sendAttachment(ctx context.Context, m store.Message, idx int) error {
	atts, err := a.fetchAttachments(ctx, m)
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(atts) {
		return uerr("Attachment not found.")
	}
	att := atts[idx]
	if len(att.Content) > a.Cfg.Sync.MaxAttachmentMB<<20 || len(att.Content) > 50<<20 {
		return uerr("%s is too large to send via Telegram. Open it in Gmail instead.", att.Filename)
	}
	_, err = a.B.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:          m.ChatID,
		Document:        &models.InputFileUpload{Filename: att.Filename, Data: bytes.NewReader(att.Content)},
		ReplyParameters: &models.ReplyParameters{MessageID: m.TgMessageID, AllowSendingWithoutReply: true},
	})
	return err
}

func original(m store.Message) mailbuild.Original {
	return mailbuild.Original{
		FromName: m.FromName, FromAddr: m.FromAddr, ReplyTo: m.ReplyTo, To: m.To, Cc: m.Cc,
		Subject: m.Subject, DateHdr: m.DateHdr, Body: m.BodyText,
		RFCMessageID: m.RFCMessageID, References: m.References,
	}
}

// sendReply sends text (and an optional Telegram file) as a threaded reply to m.
func (a *App) sendReply(ctx context.Context, chatID int64, m store.Message, text string, f *fileRef, replyTo int) error {
	if text == "" && f == nil {
		return uerr("Your reply is empty.")
	}
	c, acc, err := a.clientFor(ctx, m.AccountID)
	if err != nil {
		return err
	}
	var files []mailbuild.File
	if f != nil {
		data, err := a.download(ctx, f.FileID)
		if err != nil {
			return err
		}
		files = append(files, mailbuild.File{Name: f.Name, MIME: f.MIME, Content: data})
	}
	raw, err := mailbuild.Reply(acc.Email, original(m), text, files)
	if err != nil {
		return err
	}
	if _, err := c.Send(ctx, raw, m.ThreadID); err != nil {
		return err
	}
	if err := a.Store.SetStatus(ctx, m.ID, "replied"); err != nil {
		return err
	}
	if err := a.refresh(ctx, m.ID, notify.ViewMain); err != nil {
		a.Log.Warn("refresh after reply", "err", err)
	}
	to := mailbuild.ReplyRecipient(original(m))
	_, err = a.send(ctx, chatID, fmt.Sprintf("✅ Reply sent from %s to %s.", html.EscapeString(acc.Email), html.EscapeString(to.Address)), nil, replyTo)
	return err
}

func safeFilename(s, fallback string) string {
	var out []rune
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' || r < 32:
			out = append(out, '_')
		default:
			out = append(out, r)
		}
		if len(out) >= 60 {
			break
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return string(out)
}

// fileRef points at a file the user sent to the bot.
type fileRef struct {
	FileID string `json:"id"`
	Name   string `json:"name"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
}

func fileFromMessage(m *models.Message) *fileRef {
	if d := m.Document; d != nil {
		name := d.FileName
		if name == "" {
			name = "file"
		}
		return &fileRef{FileID: d.FileID, Name: name, MIME: d.MimeType, Size: d.FileSize}
	}
	if len(m.Photo) > 0 {
		p := m.Photo[len(m.Photo)-1] // largest size
		return &fileRef{FileID: p.FileID, Name: "photo_" + time.Unix(int64(m.Date), 0).UTC().Format("20060102_150405") + ".jpg", MIME: "image/jpeg", Size: int64(p.FileSize)}
	}
	return nil
}
