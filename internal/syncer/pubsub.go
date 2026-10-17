package syncer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Debouncer coalesces bursts of notifications per account into one Sync call.
type Debouncer struct {
	Syncer *Syncer
	Delay  time.Duration

	mu     sync.Mutex
	timers map[int64]*time.Timer
}

func (d *Debouncer) Trigger(ctx context.Context, accountID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timers == nil {
		d.timers = map[int64]*time.Timer{}
	}
	if t, ok := d.timers[accountID]; ok {
		t.Reset(d.Delay)
		return
	}
	d.timers[accountID] = time.AfterFunc(d.Delay, func() {
		d.mu.Lock()
		delete(d.timers, accountID)
		d.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err := d.Syncer.Sync(ctx, accountID); err != nil && ctx.Err() == nil {
			d.Syncer.Log.Error("sync (push)", "account_id", accountID, "err", err)
		}
	})
}

// PushConsumer receives Gmail notifications from a Pub/Sub pull subscription.
type PushConsumer struct {
	Syncer       *Syncer
	ProjectID    string
	Subscription string
	Debounce     *Debouncer
}

type gmailNotification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    uint64 `json:"historyId"`
}

func (p *PushConsumer) Run(ctx context.Context) error {
	client, err := pubsub.NewClient(ctx, p.ProjectID)
	if err != nil {
		return err
	}
	defer client.Close()
	sub := client.Subscriber(p.Subscription)
	p.Syncer.Log.Info("listening for gmail push notifications", "subscription", p.Subscription)
	return sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		p.Handle(ctx, m.Data)
		m.Ack() // Sync is idempotent and the fallback poll covers anything missed.
	})
}

// Handle processes one decoded notification payload.
func (p *PushConsumer) Handle(ctx context.Context, data []byte) {
	var n gmailNotification
	if err := json.Unmarshal(data, &n); err != nil || n.EmailAddress == "" {
		p.Syncer.Log.Warn("ignoring malformed pubsub message", "err", err)
		return
	}
	accs, err := p.Syncer.Store.AccountsByEmail(ctx, n.EmailAddress)
	if err != nil {
		p.Syncer.Log.Error("lookup account", "email", n.EmailAddress, "err", err)
		return
	}
	for _, a := range accs {
		p.Debounce.Trigger(context.WithoutCancel(ctx), a.ID)
	}
}

// WatchRenewer keeps a Gmail watch alive on every account (they expire after 7 days).
type WatchRenewer struct {
	Syncer *Syncer
	Topic  string
}

func (w *WatchRenewer) Run(ctx context.Context) error {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		w.RenewDue(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RenewDue renews watches that expire within a day.
func (w *WatchRenewer) RenewDue(ctx context.Context) {
	accs, err := w.Syncer.Store.AllAccounts(ctx)
	if err != nil {
		w.Syncer.Log.Error("list accounts", "err", err)
		return
	}
	for _, a := range accs {
		if time.Until(a.WatchExpiration) > 24*time.Hour {
			continue
		}
		if err := w.Renew(ctx, a.ID); err != nil {
			w.Syncer.Log.Error("renew watch", "account", a.Email, "err", err)
		}
	}
}

// Renew (re)creates the watch for one account.
func (w *WatchRenewer) Renew(ctx context.Context, accountID int64) error {
	acc, err := w.Syncer.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	c, err := w.Syncer.Gmail.For(ctx, acc)
	if err != nil {
		return err
	}
	_, exp, err := c.Watch(ctx, w.Topic)
	if err != nil {
		return err
	}
	return w.Syncer.Store.SetWatchExpiration(ctx, acc.ID, exp)
}
