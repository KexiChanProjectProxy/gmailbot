// Package syncer pulls new INBOX mail for linked accounts and hands it to Telegram.
// Polling and Pub/Sub both funnel into Syncer.Sync.
package syncer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// Notifier delivers stored mail to Telegram.
type Notifier interface {
	// NotifyNew sends a newly stored message and returns the Telegram message id.
	NotifyNew(ctx context.Context, user store.User, msgID int64) (int, error)
	// Refresh re-renders an already shown message (e.g. to add "also in").
	Refresh(ctx context.Context, msgID int64) error
	// AuthExpired tells the user an account needs to be re-authorized.
	AuthExpired(ctx context.Context, user store.User, email string)
}

type Options struct {
	Dedup      bool
	SnippetLen int
}

type Syncer struct {
	Store    *store.Store
	Gmail    *gmailx.Factory
	Notifier Notifier
	Opts     Options
	Log      *slog.Logger

	mu         sync.Mutex
	locks      map[int64]*sync.Mutex
	authWarned map[int64]bool
}

// IsAuthError reports whether Google rejected the account's refresh token.
func IsAuthError(err error) bool {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return re.ErrorCode == "invalid_grant" || re.ErrorCode == "unauthorized_client"
	}
	return err != nil && strings.Contains(err.Error(), "invalid_grant")
}

// checkAuth warns the user once per process when an account's token stops working.
func (s *Syncer) checkAuth(ctx context.Context, acc store.Account, user store.User, err error) {
	s.mu.Lock()
	if s.authWarned == nil {
		s.authWarned = map[int64]bool{}
	}
	if err == nil {
		delete(s.authWarned, acc.ID)
		s.mu.Unlock()
		return
	}
	if !IsAuthError(err) || s.authWarned[acc.ID] {
		s.mu.Unlock()
		return
	}
	s.authWarned[acc.ID] = true
	s.mu.Unlock()
	s.Notifier.AuthExpired(ctx, user, acc.Email)
}

func (s *Syncer) lock(accountID int64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[int64]*sync.Mutex{}
	}
	l, ok := s.locks[accountID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[accountID] = l
	}
	return l
}

// Sync processes all INBOX additions since the account's stored history id.
// Concurrent calls for the same account are serialised.
func (s *Syncer) Sync(ctx context.Context, accountID int64) error {
	l := s.lock(accountID)
	l.Lock()
	defer l.Unlock()

	acc, err := s.Store.GetAccount(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // disconnected meanwhile
	}
	if err != nil {
		return err
	}
	user, err := s.Store.GetUser(ctx, acc.TgUserID)
	if err != nil {
		return err
	}
	c, err := s.Gmail.For(ctx, acc)
	if err != nil {
		return err
	}
	log := s.Log.With("account", acc.Email)
	err = s.sync(ctx, c, acc, user, log)
	s.checkAuth(ctx, acc, user, err)
	return err
}

func (s *Syncer) sync(ctx context.Context, c *gmailx.Client, acc store.Account, user store.User, log *slog.Logger) error {
	if acc.HistoryID == 0 {
		return s.resetCursor(ctx, c, acc.ID)
	}
	ids, latest, err := c.InboxAdded(ctx, acc.HistoryID)
	if errors.Is(err, gmailx.ErrHistoryExpired) {
		log.Warn("history id expired; resetting cursor, mail received meanwhile is skipped")
		return s.resetCursor(ctx, c, acc.ID)
	}
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		blacklist, err := s.Store.Blacklist(ctx, user.TgUserID)
		if err != nil {
			return err
		}
		show := user.NotificationsEnabled && acc.NotifyEnabled
		for _, id := range ids {
			if err := s.process(ctx, c, acc, user, show, blacklist, id); err != nil {
				return err // cursor not advanced; already stored mail is skipped next time
			}
		}
	}
	return s.Store.AdvanceHistory(ctx, acc.ID, latest)
}

func (s *Syncer) resetCursor(ctx context.Context, c *gmailx.Client, accountID int64) error {
	_, hist, err := c.Profile(ctx)
	if err != nil {
		return err
	}
	return s.Store.ResetHistory(ctx, accountID, hist)
}

func (s *Syncer) process(ctx context.Context, c *gmailx.Client, acc store.Account, user store.User, show bool, blacklist []string, gmailID string) error {
	log := s.Log.With("account", acc.Email, "gmail_id", gmailID)
	exists, err := s.Store.MessageExists(ctx, acc.ID, gmailID)
	if err != nil || exists {
		return err
	}
	raw, err := c.GetRaw(ctx, gmailID)
	if errors.Is(err, gmailx.ErrGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if !raw.HasLabel("INBOX") || raw.HasLabel("SPAM") || raw.HasLabel("TRASH") || raw.HasLabel("DRAFT") {
		return nil
	}
	p, err := mailparse.Parse(raw.Raw)
	if err != nil {
		log.Warn("unparseable mail; skipping", "err", err)
		return nil
	}
	if Blacklisted(p.FromAddr, blacklist) {
		log.Debug("sender blacklisted", "from", p.FromAddr)
		return nil
	}

	m := store.Message{
		AccountID:    acc.ID,
		GmailID:      raw.ID,
		ThreadID:     raw.ThreadID,
		ChatID:       user.ChatID,
		FromName:     p.FromName,
		FromAddr:     p.FromAddr,
		ReplyTo:      p.ReplyTo,
		To:           strings.Join(p.To, ", "),
		Cc:           strings.Join(p.Cc, ", "),
		Subject:      p.Subject,
		Snippet:      mailparse.Snippet(p.Text, s.snippetLen()),
		BodyText:     p.Text,
		RFCMessageID: p.MessageID,
		References:   p.References,
		DateHdr:      p.DateHdr,
		ReceivedAt:   raw.InternalMs / 1000,
	}
	if m.Snippet == "" {
		m.Snippet = raw.Snippet
	}
	for i, a := range p.Attachments {
		m.Attachments = append(m.Attachments, store.Attachment{Idx: i, Filename: a.Filename, MIME: a.MIME, Size: int64(len(a.Content))})
	}

	// Only copies that will actually be shown claim the dedup key, so a paused account
	// can never hide mail that an active account also received.
	key := ""
	if s.Opts.Dedup && show {
		key = mailparse.DedupKey(p)
	}
	res, err := s.Store.InsertMessage(ctx, user.TgUserID, key, m)
	if err != nil || !res.Inserted {
		return err
	}
	if res.DupOf != 0 {
		log.Info("duplicate of mail already shown; suppressed", "dup_of", res.DupOf)
		if err := s.Notifier.Refresh(ctx, res.DupOf); err != nil {
			log.Warn("refresh winner message", "err", err)
		}
		return nil
	}
	if !show {
		return nil
	}
	tgID, err := s.Notifier.NotifyNew(ctx, user, res.ID)
	if err != nil {
		log.Error("deliver to telegram", "err", err)
		return nil // mail is cached; do not block the cursor on a Telegram outage
	}
	if err := s.Store.SetTelegramID(ctx, res.ID, tgID); err != nil {
		return err
	}
	// A copy from another account may have been stored while we were sending; it could not
	// annotate our message yet because the Telegram id was unknown. Recording the id before this
	// check means one of the two sides always performs the refresh.
	if dups, err := s.Store.Duplicates(ctx, res.ID); err == nil && len(dups) > 0 {
		if err := s.Notifier.Refresh(ctx, res.ID); err != nil {
			log.Warn("refresh after duplicate", "err", err)
		}
	}
	return nil
}

func (s *Syncer) snippetLen() int {
	if s.Opts.SnippetLen > 0 {
		return s.Opts.SnippetLen
	}
	return 300
}

// Blacklisted matches an address against patterns: a full address, "@domain" or "domain".
// Domain patterns also match subdomains.
func Blacklisted(addr string, patterns []string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	domain := ""
	if at >= 0 {
		domain = addr[at+1:]
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.Contains(strings.TrimPrefix(p, "@"), "@") {
			if p == addr {
				return true
			}
			continue
		}
		d := strings.TrimPrefix(p, "@")
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// ---- background loops ----

// Poller syncs every account on a fixed interval with a bounded number of workers.
type Poller struct {
	Syncer   *Syncer
	Interval time.Duration
	Workers  int
}

func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.Round(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Round syncs all accounts once.
func (p *Poller) Round(ctx context.Context) {
	accs, err := p.Syncer.Store.AllAccounts(ctx)
	if err != nil {
		p.Syncer.Log.Error("list accounts", "err", err)
		return
	}
	sem := make(chan struct{}, max(1, p.Workers))
	var wg sync.WaitGroup
	for _, a := range accs {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(a store.Account) {
			defer func() { <-sem; wg.Done() }()
			if err := p.Syncer.Sync(ctx, a.ID); err != nil && ctx.Err() == nil {
				p.Syncer.Log.Error("sync", "account", a.Email, "err", err)
			}
		}(a)
	}
	wg.Wait()
}

// Janitor periodically deletes cached mail older than the TTL.
type Janitor struct {
	Store *store.Store
	TTL   time.Duration
	Log   *slog.Logger
}

func (j *Janitor) Run(ctx context.Context) error {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := j.Store.Cleanup(ctx, j.TTL); err != nil {
			j.Log.Error("cleanup", "err", err)
		} else if n > 0 {
			j.Log.Info("cleanup", "deleted_messages", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
