// Package telegram implements the bot's chat interface.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/kexiChanProjectProxy/gmailbot/internal/config"
	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/notify"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// Watcher is implemented by the Pub/Sub watch renewer; nil in poll mode.
type Watcher interface {
	Renew(ctx context.Context, accountID int64) error
}

type App struct {
	B       *bot.Bot
	Store   *store.Store
	Gmail   *gmailx.Factory
	Cfg     config.Config
	Log     *slog.Logger
	Watcher Watcher
	// HTTP is used to download files from Telegram.
	HTTP *http.Client
}

var commands = []models.BotCommand{
	{Command: "start", Description: "Connect a Gmail account / resume notifications"},
	{Command: "new", Description: "Compose a new email"},
	{Command: "accounts", Description: "Manage linked Gmail accounts"},
	{Command: "settings", Description: "Notifications and sender blacklist"},
	{Command: "stop", Description: "Pause email notifications"},
	{Command: "resetbot", Description: "Disconnect everything and delete your data"},
	{Command: "help", Description: "How to use the bot"},
}

// New creates the Telegram bot. opts are appended (tests use bot.WithServerURL / WithSkipGetMe).
func New(cfg config.Config, st *store.Store, gm *gmailx.Factory, log *slog.Logger, opts ...bot.Option) (*App, error) {
	a := &App{Store: st, Gmail: gm, Cfg: cfg, Log: log, HTTP: http.DefaultClient}
	base := []bot.Option{
		bot.WithDefaultHandler(a.route),
		bot.WithMiddlewares(a.guard),
		bot.WithErrorsHandler(func(err error) { log.Warn("telegram", "err", err) }),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
	}
	if cfg.Telegram.APIURL != "" {
		base = append(base, bot.WithServerURL(cfg.Telegram.APIURL))
	}
	b, err := bot.New(cfg.Telegram.Token, append(base, opts...)...)
	if err != nil {
		return nil, err
	}
	a.B = b
	return a, nil
}

// Run registers commands and long-polls until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	if _, err := a.B.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands}); err != nil {
		a.Log.Warn("setMyCommands", "err", err)
	}
	a.B.Start(ctx)
	return nil
}

// guard drops non-private chats and users outside the allowlist.
func (a *App) guard(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, u *models.Update) {
		var uid int64
		var chat *models.Chat
		switch {
		case u.Message != nil && u.Message.From != nil:
			uid, chat = u.Message.From.ID, &u.Message.Chat
		case u.CallbackQuery != nil:
			uid = u.CallbackQuery.From.ID
			if m := u.CallbackQuery.Message.Message; m != nil {
				chat = &m.Chat
			}
		default:
			return
		}
		if chat == nil || chat.Type != models.ChatTypePrivate {
			return
		}
		if !a.Cfg.Allowed(uid) {
			if u.Message != nil {
				a.sendText(ctx, chat.ID, fmt.Sprintf("⛔ This bot is private. Your Telegram user id is <code>%d</code>; ask the operator to add it to <code>telegram.allowed_user_ids</code>.", uid))
			} else {
				a.answer(ctx, u.CallbackQuery, "Not authorized", true)
			}
			return
		}
		next(ctx, b, u)
	}
}

func (a *App) route(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if q := u.CallbackQuery; q != nil {
		a.onCallback(ctx, q)
		return
	}
	m := u.Message
	if m == nil {
		return
	}
	if cmd, args, ok := parseCommand(m); ok {
		a.onCommand(ctx, m, cmd, args)
		return
	}
	a.onMessage(ctx, m)
}

func parseCommand(m *models.Message) (cmd, args string, ok bool) {
	t := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	head, rest, _ := strings.Cut(t[1:], " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), strings.TrimSpace(rest), head != ""
}

func (a *App) onCallback(ctx context.Context, q *models.CallbackQuery) {
	msg := q.Message.Message
	if msg == nil {
		a.answer(ctx, q, "This message is too old.", true)
		return
	}
	prefix, _, _ := strings.Cut(q.Data, ":")
	var err error
	switch prefix {
	case "m":
		err = a.onMailCallback(ctx, q, msg)
	case "c":
		err = a.onComposeCallback(ctx, q, msg)
	case "a":
		err = a.onAccountCallback(ctx, q, msg)
	case "s":
		err = a.onSettingsCallback(ctx, q, msg)
	case "r":
		err = a.onResetCallback(ctx, q, msg)
	default:
		a.answer(ctx, q, "", false)
		return
	}
	if err != nil {
		a.Log.Error("callback", "data", q.Data, "err", err)
		a.answer(ctx, q, "⚠️ "+userError(err), true)
	}
}

// userError converts internal errors into something short enough for an alert.
func userError(err error) string {
	var ue *UserErr
	if errors.As(err, &ue) {
		return ue.Msg
	}
	s := err.Error()
	if len(s) > 180 {
		s = s[:180] + "…"
	}
	return "Something went wrong: " + s
}

// UserErr carries a message intended for the user.
type UserErr struct{ Msg string }

func (e *UserErr) Error() string { return e.Msg }

func uerr(format string, args ...any) error { return &UserErr{Msg: fmt.Sprintf(format, args...)} }

// ---- Telegram helpers ----

func (a *App) sendText(ctx context.Context, chatID int64, html string) *models.Message {
	m, err := a.send(ctx, chatID, html, nil, 0)
	if err != nil {
		a.Log.Warn("send", "err", err)
	}
	return m
}

func (a *App) send(ctx context.Context, chatID int64, html string, kb *models.InlineKeyboardMarkup, replyTo int) (*models.Message, error) {
	p := &bot.SendMessageParams{
		ChatID:             chatID,
		Text:               html,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	if kb != nil {
		p.ReplyMarkup = kb
	}
	if replyTo != 0 {
		p.ReplyParameters = &models.ReplyParameters{MessageID: replyTo, AllowSendingWithoutReply: true}
	}
	return a.B.SendMessage(ctx, p)
}

func (a *App) edit(ctx context.Context, chatID int64, msgID int, html string, kb *models.InlineKeyboardMarkup) error {
	p := &bot.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          msgID,
		Text:               html,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	if kb != nil {
		p.ReplyMarkup = kb
	}
	_, err := a.B.EditMessageText(ctx, p)
	if isNotModified(err) {
		return nil
	}
	return err
}

func (a *App) editMarkup(ctx context.Context, chatID int64, msgID int, kb *models.InlineKeyboardMarkup) error {
	_, err := a.B.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{ChatID: chatID, MessageID: msgID, ReplyMarkup: kb})
	if isNotModified(err) {
		return nil
	}
	return err
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

func (a *App) answer(ctx context.Context, q *models.CallbackQuery, text string, alert bool) {
	if _, err := a.B.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: text, ShowAlert: alert}); err != nil {
		a.Log.Debug("answerCallbackQuery", "err", err)
	}
}

func (a *App) deleteMsg(ctx context.Context, chatID int64, msgID int) {
	if msgID == 0 {
		return
	}
	_, _ = a.B.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: msgID})
}

// download fetches a Telegram file, enforcing the configured size cap.
func (a *App) download(ctx context.Context, fileID string) ([]byte, error) {
	f, err := a.B.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, err
	}
	limit := int64(a.Cfg.Sync.MaxAttachmentMB) << 20
	if f.FileSize > limit {
		return nil, uerr("File is larger than %d MB.", a.Cfg.Sync.MaxAttachmentMB)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.B.FileDownloadLink(f), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, uerr("File is larger than %d MB.", a.Cfg.Sync.MaxAttachmentMB)
	}
	return b, nil
}

// ---- syncer.Notifier ----

// NotifyNew sends a stored mail to the user's chat.
func (a *App) NotifyNew(ctx context.Context, user store.User, msgID int64) (int, error) {
	in, err := a.renderInput(ctx, msgID, notify.ViewMain)
	if err != nil {
		return 0, err
	}
	text, kb := notify.Render(in)
	m, err := a.send(ctx, user.ChatID, text, kb, 0)
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

// AuthExpired asks the user to re-authorize an account whose token Google rejected.
func (a *App) AuthExpired(ctx context.Context, user store.User, email string) {
	link, err := a.connectURL(ctx, user.TgUserID, user.ChatID)
	if err != nil {
		a.Log.Error("auth expired link", "err", err)
		return
	}
	if _, err := a.send(ctx, user.ChatID, "🔑 Google no longer accepts the authorization for <b>"+html.EscapeString(email)+
		"</b>, so new mail can't be fetched. Tap below to sign in again.", kb(row(urlBtn("🔐 Re-authorize", link))), 0); err != nil {
		a.Log.Warn("send auth expired", "err", err)
	}
}

// Refresh re-renders a shown mail in place.
func (a *App) Refresh(ctx context.Context, msgID int64) error {
	return a.refresh(ctx, msgID, notify.ViewMain)
}

func (a *App) refresh(ctx context.Context, msgID int64, view notify.View) error {
	in, err := a.renderInput(ctx, msgID, view)
	if err != nil {
		return err
	}
	if in.Msg.TgMessageID == 0 {
		return nil
	}
	text, kb := notify.Render(in)
	return a.edit(ctx, in.Msg.ChatID, in.Msg.TgMessageID, text, kb)
}

func (a *App) renderInput(ctx context.Context, msgID int64, view notify.View) (notify.Input, error) {
	m, err := a.Store.GetMessage(ctx, msgID)
	if err != nil {
		return notify.Input{}, err
	}
	acc, err := a.Store.GetAccount(ctx, m.AccountID)
	if err != nil {
		return notify.Input{}, err
	}
	accs, err := a.Store.AccountsByUser(ctx, acc.TgUserID)
	if err != nil {
		return notify.Input{}, err
	}
	dups, err := a.Store.Duplicates(ctx, m.ID)
	if err != nil {
		return notify.Input{}, err
	}
	var also []string
	for _, d := range dups {
		for _, x := range accs {
			if x.ID == d.AccountID && x.ID != acc.ID {
				also = append(also, x.Email)
			}
		}
	}
	return notify.Input{
		Msg:          m,
		AccountEmail: acc.Email,
		ShowAccount:  len(accs) > 1,
		AlsoIn:       also,
		View:         view,
		MaxBody:      a.Cfg.Sync.MaxBodyChars,
		GmailURL:     gmailx.WebURL(acc.Email, m.GmailID),
	}, nil
}
