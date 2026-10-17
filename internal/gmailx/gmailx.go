// Package gmailx wraps the Gmail API for a single linked account.
package gmailx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/kexiChanProjectProxy/gmailbot/internal/crypto"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// Scope is the single OAuth scope the bot requests: read, send, trash and relabel.
const Scope = gmail.GmailModifyScope

// ErrHistoryExpired means the stored history id is too old and the cursor must be reset.
var ErrHistoryExpired = errors.New("gmail history id expired")

// OAuthConfig builds the oauth2 config for the installed Google client.
func OAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{Scope, "openid", "email"},
	}
}

// Factory creates per-account clients whose refreshed tokens are written back to the store.
type Factory struct {
	OAuth  *oauth2.Config
	Store  *store.Store
	Sealer *crypto.Sealer
	// Options are extra client options; tests point the endpoint at a fake server.
	Options []option.ClientOption
	// RevokeURL defaults to Google's token revocation endpoint.
	RevokeURL string

	mu      sync.Mutex
	clients map[int64]*Client
}

// Client is a Gmail API handle for one account.
type Client struct {
	AccountID int64
	Email     string
	svc       *gmail.Service
	ts        oauth2.TokenSource
	revokeURL string
}

// EncodeToken seals an oauth2 token for storage.
func (f *Factory) EncodeToken(t *oauth2.Token) ([]byte, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return f.Sealer.Seal(b)
}

func (f *Factory) decodeToken(blob []byte) (*oauth2.Token, error) {
	b, err := f.Sealer.Open(blob)
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	return &t, json.Unmarshal(b, &t)
}

// For returns a cached client for the account, creating it if needed.
func (f *Factory) For(ctx context.Context, acc store.Account) (*Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[acc.ID]; ok {
		return c, nil
	}
	tok, err := f.decodeToken(acc.TokenEnc)
	if err != nil {
		return nil, fmt.Errorf("decode token for %s: %w", acc.Email, err)
	}
	ts := &persistingSource{
		base: oauth2.ReuseTokenSource(tok, f.OAuth.TokenSource(context.Background(), tok)),
		last: tok.AccessToken,
		save: func(t *oauth2.Token) error {
			b, err := f.EncodeToken(t)
			if err != nil {
				return err
			}
			return f.Store.UpdateToken(context.Background(), acc.ID, b)
		},
	}
	c, err := f.NewClient(ctx, acc.ID, acc.Email, ts)
	if err != nil {
		return nil, err
	}
	if f.clients == nil {
		f.clients = map[int64]*Client{}
	}
	f.clients[acc.ID] = c
	return c, nil
}

// NewClient builds an uncached client from any token source (used right after the OAuth exchange).
func (f *Factory) NewClient(ctx context.Context, accountID int64, email string, ts oauth2.TokenSource) (*Client, error) {
	opts := append([]option.ClientOption{option.WithTokenSource(ts)}, f.Options...)
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	rev := f.RevokeURL
	if rev == "" {
		rev = "https://oauth2.googleapis.com/revoke"
	}
	return &Client{AccountID: accountID, Email: email, svc: svc, ts: ts, revokeURL: rev}, nil
}

// Forget drops a cached client (after disconnect or relink).
func (f *Factory) Forget(accountID int64) {
	f.mu.Lock()
	delete(f.clients, accountID)
	f.mu.Unlock()
}

type persistingSource struct {
	mu   sync.Mutex
	base oauth2.TokenSource
	last string
	save func(*oauth2.Token) error
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		if err := p.save(t); err != nil {
			return t, nil //nolint:nilerr // a failed save must not fail the API call
		}
	}
	return t, nil
}

func isStatus(err error, code int) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == code
}

// Profile returns the mailbox address and its current history id.
func (c *Client) Profile(ctx context.Context) (string, uint64, error) {
	p, err := c.svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", 0, err
	}
	return p.EmailAddress, p.HistoryId, nil
}

// InboxAdded lists ids of messages added to INBOX since start, plus the newest history id seen.
func (c *Client) InboxAdded(ctx context.Context, start uint64) ([]string, uint64, error) {
	var ids []string
	seen := map[string]bool{}
	latest := start
	call := c.svc.Users.History.List("me").StartHistoryId(start).HistoryTypes("messageAdded").LabelId("INBOX").MaxResults(500)
	err := call.Pages(ctx, func(r *gmail.ListHistoryResponse) error {
		if r.HistoryId > latest {
			latest = r.HistoryId
		}
		for _, h := range r.History {
			for _, ma := range h.MessagesAdded {
				if ma.Message != nil && !seen[ma.Message.Id] {
					seen[ma.Message.Id] = true
					ids = append(ids, ma.Message.Id)
				}
			}
		}
		return nil
	})
	if isStatus(err, http.StatusNotFound) {
		return nil, 0, ErrHistoryExpired
	}
	return ids, latest, err
}

// RawMessage is a message in RFC 822 form with Gmail metadata.
type RawMessage struct {
	ID         string
	ThreadID   string
	LabelIDs   []string
	Snippet    string
	InternalMs int64
	Raw        []byte
}

func (r RawMessage) HasLabel(l string) bool {
	for _, x := range r.LabelIDs {
		if x == l {
			return true
		}
	}
	return false
}

// ErrGone means the message no longer exists (deleted between notification and fetch).
var ErrGone = errors.New("gmail message gone")

func (c *Client) GetRaw(ctx context.Context, id string) (RawMessage, error) {
	m, err := c.svc.Users.Messages.Get("me", id).Format("raw").Context(ctx).Do()
	if isStatus(err, http.StatusNotFound) {
		return RawMessage{}, ErrGone
	}
	if err != nil {
		return RawMessage{}, err
	}
	raw, err := base64.URLEncoding.DecodeString(m.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(m.Raw)
		if err != nil {
			return RawMessage{}, fmt.Errorf("decode raw: %w", err)
		}
	}
	return RawMessage{ID: m.Id, ThreadID: m.ThreadId, LabelIDs: m.LabelIds, Snippet: m.Snippet, InternalMs: m.InternalDate, Raw: raw}, nil
}

func (c *Client) modify(ctx context.Context, id string, add, remove []string) error {
	_, err := c.svc.Users.Messages.Modify("me", id, &gmail.ModifyMessageRequest{AddLabelIds: add, RemoveLabelIds: remove}).Context(ctx).Do()
	return err
}

func (c *Client) Archive(ctx context.Context, id string) error {
	return c.modify(ctx, id, nil, []string{"INBOX"})
}
func (c *Client) Spam(ctx context.Context, id string) error {
	return c.modify(ctx, id, []string{"SPAM"}, []string{"INBOX"})
}
func (c *Client) MarkRead(ctx context.Context, id string) error {
	return c.modify(ctx, id, nil, []string{"UNREAD"})
}

func (c *Client) Trash(ctx context.Context, id string) error {
	_, err := c.svc.Users.Messages.Trash("me", id).Context(ctx).Do()
	return err
}

const simpleSendLimit = 4 << 20

// Send sends an RFC 822 message, optionally inside an existing thread.
func (c *Client) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	call := c.svc.Users.Messages.Send("me", &gmail.Message{ThreadId: threadID})
	if len(raw) > simpleSendLimit {
		// Large mail goes through the media upload endpoint (up to 35 MB).
		call = call.Media(bytes.NewReader(raw), googleapi.ContentType("message/rfc822"))
	} else {
		call = c.svc.Users.Messages.Send("me", &gmail.Message{Raw: base64.URLEncoding.EncodeToString(raw), ThreadId: threadID})
	}
	m, err := call.Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return m.Id, nil
}

// Watch registers Pub/Sub push for INBOX changes.
func (c *Client) Watch(ctx context.Context, topic string) (uint64, time.Time, error) {
	r, err := c.svc.Users.Watch("me", &gmail.WatchRequest{
		TopicName:           topic,
		LabelIds:            []string{"INBOX"},
		LabelFilterBehavior: "INCLUDE",
	}).Context(ctx).Do()
	if err != nil {
		return 0, time.Time{}, err
	}
	return r.HistoryId, time.UnixMilli(r.Expiration), nil
}

func (c *Client) StopWatch(ctx context.Context) error {
	return c.svc.Users.Stop("me").Context(ctx).Do()
}

// Revoke invalidates the account's refresh token at Google.
func (c *Client) Revoke(ctx context.Context) error {
	t, err := c.ts.Token()
	if err != nil {
		return err
	}
	tok := t.RefreshToken
	if tok == "" {
		tok = t.AccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL,
		strings.NewReader(url.Values{"token": {tok}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusBadRequest { // 400 = already revoked
		return fmt.Errorf("revoke: HTTP %d", resp.StatusCode)
	}
	return nil
}

// WebURL links to the message in Gmail's web UI for the right account.
func WebURL(email, gmailID string) string {
	return "https://mail.google.com/mail/u/?authuser=" + url.QueryEscape(email) + "#all/" + gmailID
}
