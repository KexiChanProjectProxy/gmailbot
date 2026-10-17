package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
	"golang.org/x/oauth2"

	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

const helpText = `<b>Gmail Bot</b> brings your Gmail inbox into Telegram.

<b>Reading</b> – new mail arrives here. Tap <i>Show more</i> for the full text and attachments, <i>Actions</i> to reply, forward, archive, delete or report spam.
<b>Replying</b> – just use Telegram's <i>Reply</i> on an email. Text becomes the body; a file or photo is attached.
<b>Composing</b> – /new walks you through To, Subject and Message.

/start – connect a Gmail account (run again to add more) and resume notifications
/accounts – default sender, pause or disconnect accounts
/settings – notifications and sender blacklist
/stop – pause notifications
/resetbot – disconnect everything and delete your data`

func (a *App) onCommand(ctx context.Context, m *models.Message, cmd, args string) {
	uid, chatID := m.From.ID, m.Chat.ID
	_ = a.Store.ClearConv(ctx, uid) // any command cancels an in-progress wizard
	var err error
	switch cmd {
	case "start":
		err = a.cmdStart(ctx, uid, chatID)
	case "help":
		a.sendText(ctx, chatID, helpText)
	case "new":
		err = a.cmdNew(ctx, uid, chatID)
	case "accounts":
		err = a.cmdAccounts(ctx, uid, chatID, 0)
	case "settings":
		err = a.cmdSettings(ctx, uid, chatID, 0)
	case "stop":
		err = a.cmdStop(ctx, uid, chatID)
	case "resetbot":
		_, err = a.send(ctx, chatID, "⚠️ This disconnects <b>all</b> Gmail accounts, revokes the bot's access at Google and deletes everything stored about you. Continue?",
			kb(row(btn("Yes, reset everything", "r:yes"), btn("Cancel", "r:no"))), 0)
	default:
		a.sendText(ctx, chatID, "Unknown command. See /help.")
	}
	if err != nil {
		a.Log.Error("command", "cmd", cmd, "err", err)
		a.sendText(ctx, chatID, "⚠️ "+html.EscapeString(userError(err)))
	}
}

// requireAccounts returns the user's accounts or tells them to /start first.
func (a *App) requireAccounts(ctx context.Context, uid int64) ([]store.Account, error) {
	accs, err := a.Store.AccountsByUser(ctx, uid)
	if err != nil {
		return nil, err
	}
	if len(accs) == 0 {
		return nil, uerr("No Gmail account connected yet. Use /start to connect one.")
	}
	return accs, nil
}

// ---- /start and OAuth ----

func (a *App) connectURL(ctx context.Context, uid, chatID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	if err := a.Store.PutOAuthState(ctx, store.OAuthState{
		State: state, TgUserID: uid, ChatID: chatID, Verifier: oauth2.GenerateVerifier(),
	}, 15*time.Minute); err != nil {
		return "", err
	}
	return strings.TrimRight(a.Cfg.OAuth.PublicURL, "/") + "/oauth/start?s=" + state, nil
}

func (a *App) cmdStart(ctx context.Context, uid, chatID int64) error {
	if err := a.Store.UpsertUser(ctx, uid, chatID); err != nil {
		return err
	}
	u, err := a.Store.GetUser(ctx, uid)
	if err != nil {
		return err
	}
	resumed := false
	if !u.NotificationsEnabled {
		if err := a.Store.SetNotifications(ctx, uid, true); err != nil {
			return err
		}
		resumed = true
	}
	link, err := a.connectURL(ctx, uid, chatID)
	if err != nil {
		return err
	}
	accs, err := a.Store.AccountsByUser(ctx, uid)
	if err != nil {
		return err
	}
	var sb strings.Builder
	if len(accs) == 0 {
		sb.WriteString("👋 Hi! I deliver your Gmail to Telegram and let you reply, forward, archive and delete without leaving the chat.\n\nTap the button below to authorize me with Google. The link is valid for 15 minutes.")
	} else {
		if resumed {
			sb.WriteString("🔔 Notifications resumed.\n\n")
		}
		sb.WriteString("Connected accounts:\n")
		for _, x := range accs {
			sb.WriteString("• " + html.EscapeString(x.Email) + "\n")
		}
		sb.WriteString("\nTap below to connect another Gmail account.")
	}
	label := "🔐 Connect Gmail"
	if len(accs) > 0 {
		label = "➕ Connect another Gmail"
	}
	_, err = a.send(ctx, chatID, sb.String(), kb(row(urlBtn(label, link))), 0)
	return err
}

// OnLinked is called by the OAuth callback once an account is stored.
func (a *App) OnLinked(ctx context.Context, uid, chatID, accountID int64, email string, created bool) {
	if a.Watcher != nil {
		if err := a.Watcher.Renew(ctx, accountID); err != nil {
			a.Log.Error("start gmail watch", "account", email, "err", err)
			a.sendText(ctx, chatID, "⚠️ Could not register Gmail push notifications for "+html.EscapeString(email)+"; falling back to periodic checks.")
		}
	}
	verb := "Connected"
	if !created {
		verb = "Re-authorized"
	}
	a.sendText(ctx, chatID, fmt.Sprintf("✅ %s <b>%s</b>. New mail will appear here.\nUse /new to write an email, /accounts to manage accounts.", verb, html.EscapeString(email)))
}

// ---- /accounts ----

func (a *App) accountsView(ctx context.Context, uid int64) (string, *models.InlineKeyboardMarkup, error) {
	accs, err := a.Store.AccountsByUser(ctx, uid)
	if err != nil {
		return "", nil, err
	}
	u, err := a.Store.GetUser(ctx, uid)
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	var rows [][]models.InlineKeyboardButton
	if len(accs) == 0 {
		sb.WriteString("No accounts connected.")
	} else {
		sb.WriteString("<b>Linked Gmail accounts</b>\n")
		def := defaultAccount(u, accs)
		for _, x := range accs {
			marks := ""
			if len(accs) > 1 && x.ID == def.ID {
				marks += " ⭐"
			}
			if !x.NotifyEnabled {
				marks += " ⏸"
			}
			sb.WriteString("• " + html.EscapeString(x.Email) + marks + "\n")
			id := strconv.FormatInt(x.ID, 10)
			r := []models.InlineKeyboardButton{}
			if len(accs) > 1 && x.ID != def.ID {
				r = append(r, btn("⭐ Default", "a:def:"+id))
			}
			if x.NotifyEnabled {
				r = append(r, btn("⏸ Pause", "a:off:"+id))
			} else {
				r = append(r, btn("▶️ Resume", "a:on:"+id))
			}
			r = append(r, btn("❌ "+shortEmail(x.Email), "a:delq:"+id))
			rows = append(rows, r)
		}
		if len(accs) > 1 {
			sb.WriteString("\n⭐ default sender for /new · ⏸ notifications paused")
		}
	}
	rows = append(rows, row(btn("➕ Connect another", "a:add")))
	return sb.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func shortEmail(e string) string {
	if len(e) > 24 {
		return e[:23] + "…"
	}
	return e
}

func defaultAccount(u store.User, accs []store.Account) store.Account {
	for _, x := range accs {
		if x.ID == u.DefaultAccountID {
			return x
		}
	}
	return accs[0]
}

// cmdAccounts sends the accounts view, or edits editID in place when non-zero.
func (a *App) cmdAccounts(ctx context.Context, uid, chatID int64, editID int) error {
	if _, err := a.Store.GetUser(ctx, uid); errors.Is(err, store.ErrNotFound) {
		return uerr("Use /start first.")
	}
	text, k, err := a.accountsView(ctx, uid)
	if err != nil {
		return err
	}
	if editID != 0 {
		return a.edit(ctx, chatID, editID, text, k)
	}
	_, err = a.send(ctx, chatID, text, k, 0)
	return err
}

func (a *App) ownedAccount(ctx context.Context, uid int64, idStr string) (store.Account, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return store.Account{}, uerr("bad account id")
	}
	acc, err := a.Store.GetAccount(ctx, id)
	if err != nil || acc.TgUserID != uid {
		return store.Account{}, uerr("That account is no longer connected.")
	}
	return acc, nil
}

func (a *App) onAccountCallback(ctx context.Context, q *models.CallbackQuery, msg *models.Message) error {
	parts := strings.Split(q.Data, ":")
	uid, chatID := q.From.ID, msg.Chat.ID
	op := parts[1]
	if op == "add" {
		if err := a.Store.UpsertUser(ctx, uid, chatID); err != nil {
			return err
		}
		link, err := a.connectURL(ctx, uid, chatID)
		if err != nil {
			return err
		}
		a.answer(ctx, q, "", false)
		_, err = a.send(ctx, chatID, "Tap to authorize another Gmail account (link valid 15 minutes).", kb(row(urlBtn("🔐 Connect Gmail", link))), 0)
		return err
	}
	if len(parts) < 3 {
		return uerr("bad request")
	}
	acc, err := a.ownedAccount(ctx, uid, parts[2])
	if err != nil {
		return err
	}
	switch op {
	case "def":
		err = a.Store.SetDefaultAccount(ctx, uid, acc.ID)
	case "on", "off":
		err = a.Store.SetAccountNotify(ctx, acc.ID, op == "on")
	case "delq":
		a.answer(ctx, q, "", false)
		return a.edit(ctx, chatID, msg.ID, "Disconnect <b>"+html.EscapeString(acc.Email)+"</b>? The bot's access will be revoked at Google.",
			kb(row(btn("Yes, disconnect", "a:del:"+parts[2]), btn("Cancel", "a:back:0"))))
	case "del":
		a.disconnect(ctx, acc)
	case "back":
	default:
		return uerr("bad request")
	}
	if err != nil {
		return err
	}
	a.answer(ctx, q, "", false)
	return a.cmdAccounts(ctx, uid, chatID, msg.ID)
}

// disconnect stops push, revokes the token and deletes the account. Failures at Google are
// logged but never block local deletion.
func (a *App) disconnect(ctx context.Context, acc store.Account) {
	if c, err := a.Gmail.For(ctx, acc); err == nil {
		if a.Watcher != nil {
			if err := c.StopWatch(ctx); err != nil {
				a.Log.Warn("stop watch", "account", acc.Email, "err", err)
			}
		}
		if err := c.Revoke(ctx); err != nil {
			a.Log.Warn("revoke token", "account", acc.Email, "err", err)
		}
	}
	a.Gmail.Forget(acc.ID)
	if err := a.Store.DeleteAccount(ctx, acc.ID); err != nil {
		a.Log.Error("delete account", "account", acc.Email, "err", err)
	}
}

// ---- /stop and /resetbot ----

func (a *App) cmdStop(ctx context.Context, uid, chatID int64) error {
	if _, err := a.Store.GetUser(ctx, uid); errors.Is(err, store.ErrNotFound) {
		return uerr("Nothing to stop. Use /start to connect Gmail.")
	}
	if err := a.Store.SetNotifications(ctx, uid, false); err != nil {
		return err
	}
	a.sendText(ctx, chatID, "🔕 Notifications paused. Mail received meanwhile will not be delivered. Send /start to resume.")
	return nil
}

func (a *App) onResetCallback(ctx context.Context, q *models.CallbackQuery, msg *models.Message) error {
	uid, chatID := q.From.ID, msg.Chat.ID
	if q.Data != "r:yes" {
		a.answer(ctx, q, "", false)
		return a.edit(ctx, chatID, msg.ID, "Reset cancelled.", nil)
	}
	accs, err := a.Store.AccountsByUser(ctx, uid)
	if err != nil {
		return err
	}
	for _, acc := range accs {
		a.disconnect(ctx, acc)
	}
	if err := a.Store.DeleteUser(ctx, uid); err != nil {
		return err
	}
	a.answer(ctx, q, "", false)
	return a.edit(ctx, chatID, msg.ID, "🧹 Done. All accounts were disconnected and your data deleted. Send /start to begin again.", nil)
}

// ---- /settings ----

func (a *App) cmdSettings(ctx context.Context, uid, chatID int64, editID int) error {
	u, err := a.Store.GetUser(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return uerr("Use /start first.")
	}
	if err != nil {
		return err
	}
	bl, err := a.Store.Blacklist(ctx, uid)
	if err != nil {
		return err
	}
	notif := "🔔 Notifications: on"
	if !u.NotificationsEnabled {
		notif = "🔕 Notifications: off"
	}
	text := "<b>Settings</b>\n\nNotifications apply to all accounts; pause single accounts in /accounts.\nThe blacklist hides mail from listed senders or domains."
	k := kb(
		row(btn(notif, "s:notif")),
		row(btn(fmt.Sprintf("🚫 Blacklist (%d)", len(bl)), "s:bl")),
		row(btn("👤 Accounts", "s:acc")),
	)
	if editID != 0 {
		return a.edit(ctx, chatID, editID, text, k)
	}
	_, err = a.send(ctx, chatID, text, k, 0)
	return err
}

func (a *App) blacklistView(ctx context.Context, uid, chatID int64, editID int) error {
	bl, err := a.Store.Blacklist(ctx, uid)
	if err != nil {
		return err
	}
	text := "<b>Sender blacklist</b>\nMail from these senders is not delivered. Tap an entry to remove it."
	if len(bl) == 0 {
		text += "\n\n<i>Empty.</i>"
	}
	var rows [][]models.InlineKeyboardButton
	for i, p := range bl {
		rows = append(rows, row(btn("❌ "+p, "s:blrm:"+strconv.Itoa(i))))
	}
	rows = append(rows, row(btn("➕ Add", "s:bladd"), btn("‹ Back", "s:home")))
	k := &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	if editID != 0 {
		return a.edit(ctx, chatID, editID, text, k)
	}
	_, err = a.send(ctx, chatID, text, k, 0)
	return err
}

func (a *App) onSettingsCallback(ctx context.Context, q *models.CallbackQuery, msg *models.Message) error {
	uid, chatID := q.From.ID, msg.Chat.ID
	parts := strings.Split(q.Data, ":")
	a.answer(ctx, q, "", false)
	switch parts[1] {
	case "home":
		return a.cmdSettings(ctx, uid, chatID, msg.ID)
	case "notif":
		u, err := a.Store.GetUser(ctx, uid)
		if err != nil {
			return err
		}
		if err := a.Store.SetNotifications(ctx, uid, !u.NotificationsEnabled); err != nil {
			return err
		}
		return a.cmdSettings(ctx, uid, chatID, msg.ID)
	case "acc":
		return a.cmdAccounts(ctx, uid, chatID, msg.ID)
	case "bl":
		return a.blacklistView(ctx, uid, chatID, msg.ID)
	case "blrm":
		bl, err := a.Store.Blacklist(ctx, uid)
		if err != nil {
			return err
		}
		if len(parts) == 3 {
			if i, err := strconv.Atoi(parts[2]); err == nil && i >= 0 && i < len(bl) {
				if err := a.Store.RemoveBlacklist(ctx, uid, bl[i]); err != nil {
					return err
				}
			}
		}
		return a.blacklistView(ctx, uid, chatID, msg.ID)
	case "bladd":
		if err := a.Store.SetConv(ctx, uid, store.Conv{Kind: convBlacklist, Payload: strconv.Itoa(msg.ID)}); err != nil {
			return err
		}
		a.sendText(ctx, chatID, "Send an address (<code>news@example.com</code>) or a domain (<code>@example.com</code>) to block.")
		return nil
	}
	return nil
}

// ---- keyboard helpers ----

func btn(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

func urlBtn(text, url string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, URL: url}
}

func row(b ...models.InlineKeyboardButton) []models.InlineKeyboardButton { return b }

func kb(rows ...[]models.InlineKeyboardButton) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
