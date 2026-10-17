package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/kexiChanProjectProxy/gmailbot/internal/config"
	"github.com/kexiChanProjectProxy/gmailbot/internal/crypto"
	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/mailparse"
	"github.com/kexiChanProjectProxy/gmailbot/internal/notify"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
	"github.com/kexiChanProjectProxy/gmailbot/internal/syncer"
)

// ---- fake Gmail API: one mailbox per bearer token ----

type gcall struct {
	Token, Op, ID string
	Body          map[string]any
}

type mailbox struct {
	added []string
	raw   map[string]string
}

type fakeGmail struct {
	mu    sync.Mutex
	boxes map[string]*mailbox
	calls []gcall
}

func (g *fakeGmail) record(c gcall) {
	g.mu.Lock()
	g.calls = append(g.calls, c)
	g.mu.Unlock()
}

func (g *fakeGmail) callsOf(op string) []gcall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gcall
	for _, c := range g.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeGmail) handler() http.Handler {
	mux := http.NewServeMux()
	token := func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }
	box := func(r *http.Request) *mailbox {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.boxes[token(r)]
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	readBody := func(r *http.Request) map[string]any {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	mux.HandleFunc("GET /gmail/v1/users/me/history", func(w http.ResponseWriter, r *http.Request) {
		b := box(r)
		var added []map[string]any
		for _, id := range b.added {
			added = append(added, map[string]any{"message": map[string]any{"id": id, "threadId": "t-" + id}})
		}
		writeJSON(w, map[string]any{"historyId": "5", "history": []any{map[string]any{"id": "5", "messagesAdded": added}}})
	})
	mux.HandleFunc("GET /gmail/v1/users/me/profile", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"emailAddress": token(r) + "@gmail.com", "historyId": "10"})
	})
	mux.HandleFunc("GET /gmail/v1/users/me/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		raw, ok := box(r).raw[id]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, 404)
			return
		}
		writeJSON(w, map[string]any{"id": id, "threadId": "t-" + id, "labelIds": []string{"INBOX", "UNREAD"},
			"raw": base64.URLEncoding.EncodeToString([]byte(raw)), "internalDate": "1760000000000"})
	})
	for _, op := range []string{"modify", "trash"} {
		op := op
		mux.HandleFunc("POST /gmail/v1/users/me/messages/{id}/"+op, func(w http.ResponseWriter, r *http.Request) {
			g.record(gcall{Token: token(r), Op: op, ID: r.PathValue("id"), Body: readBody(r)})
			writeJSON(w, map[string]any{"id": r.PathValue("id")})
		})
	}
	mux.HandleFunc("POST /gmail/v1/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		g.record(gcall{Token: token(r), Op: "send", Body: readBody(r)})
		writeJSON(w, map[string]any{"id": "sent-1"})
	})
	return mux
}

// ---- fake Telegram Bot API ----

type tcall struct {
	Method string
	Fields map[string]string
}

type fakeTelegram struct {
	mu     sync.Mutex
	calls  []tcall
	nextID int
}

func (f *fakeTelegram) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := path.Base(r.URL.Path)
		fields := map[string]string{}
		if err := r.ParseMultipartForm(10 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				fields[k] = v[0]
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, tcall{Method: method, Fields: fields})
		f.nextID++
		id := f.nextID + 1000
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage", "editMessageText", "sendDocument", "editMessageReplyMarkup":
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":100,"type":"private"}}}`, id)
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	})
}

func (f *fakeTelegram) callsOf(method string) []tcall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tcall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeTelegram) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// ---- harness ----

type harness struct {
	t    *testing.T
	ctx  context.Context
	st   *store.Store
	app  *App
	sy   *syncer.Syncer
	gm   *fakeGmail
	tg   *fakeTelegram
	accA int64
	accB int64
	uid  int64
	chat int64
}

const mailRaw = "From: Alice <alice@example.com>\r\nTo: b@gmail.com\r\nSubject: Hello there\r\n" +
	"Date: Mon, 12 Oct 2026 09:30:00 +0000\r\nMessage-ID: <same-id@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\nShall we meet?\r\n"

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	gm := &fakeGmail{boxes: map[string]*mailbox{
		"tokA": {added: []string{"a1"}, raw: map[string]string{"a1": mailRaw}},
		"tokB": {added: []string{"b1"}, raw: map[string]string{"b1": mailRaw}},
	}}
	gsrv := httptest.NewServer(gm.handler())
	t.Cleanup(gsrv.Close)
	tg := &fakeTelegram{}
	tsrv := httptest.NewServer(tg.handler())
	t.Cleanup(tsrv.Close)

	st, err := store.Open(ctx, t.TempDir()+"/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sealer, _ := crypto.New(nil)
	factory := &gmailx.Factory{
		OAuth:   &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: gsrv.URL + "/token"}},
		Store:   st,
		Sealer:  sealer,
		Options: []option.ClientOption{option.WithEndpoint(gsrv.URL + "/")},
	}
	cfg := config.Default()
	cfg.Telegram.Token = "TEST"
	cfg.Telegram.AllowedUserIDs = []int64{1}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := New(cfg, st, factory, log, bot.WithServerURL(tsrv.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	sy := &syncer.Syncer{Store: st, Gmail: factory, Notifier: app, Log: log, Opts: syncer.Options{Dedup: true}}

	h := &harness{t: t, ctx: ctx, st: st, app: app, sy: sy, gm: gm, tg: tg, uid: 1, chat: 100}
	st.UpsertUser(ctx, h.uid, h.chat)
	h.accA = h.link("a@gmail.com", "tokA")
	h.accB = h.link("b@gmail.com", "tokB")
	return h
}

func (h *harness) link(email, access string) int64 {
	blob, _ := h.app.Gmail.EncodeToken(&oauth2.Token{AccessToken: access, RefreshToken: "r", Expiry: time.Now().Add(time.Hour)})
	id, _, err := h.st.UpsertAccount(h.ctx, h.uid, email, blob, 1)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) update(u *models.Update) { h.app.guard(h.app.route)(h.ctx, h.app.B, u) }

func (h *harness) text(text string, replyTo int) {
	m := &models.Message{ID: 900, From: &models.User{ID: h.uid}, Chat: models.Chat{ID: h.chat, Type: models.ChatTypePrivate}, Text: text}
	if replyTo != 0 {
		m.ReplyToMessage = &models.Message{ID: replyTo}
	}
	h.update(&models.Update{Message: m})
}

func (h *harness) press(data string, onMsg int) {
	h.update(&models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: h.uid}, Data: data,
		Message: models.MaybeInaccessibleMessage{Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: onMsg, Chat: models.Chat{ID: h.chat, Type: models.ChatTypePrivate}}},
	}})
}

func (h *harness) syncBoth() {
	var wg sync.WaitGroup
	for _, id := range []int64{h.accA, h.accB} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := h.sy.Sync(h.ctx, id); err != nil {
				h.t.Error(err)
			}
		}(id)
	}
	wg.Wait()
}

// shownMessage returns the single cached mail that was delivered to Telegram.
func (h *harness) shownMessage() store.Message {
	for id := int64(1); id < 10; id++ {
		m, err := h.st.GetMessage(h.ctx, id)
		if err == nil && m.TgMessageID != 0 {
			return m
		}
	}
	h.t.Fatal("no message shown")
	return store.Message{}
}

// ---- tests ----

func TestDedupAcrossAccounts(t *testing.T) {
	h := newHarness(t)
	h.syncBoth()

	sends := h.tg.callsOf("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage calls = %d, want exactly 1", len(sends))
	}
	if !strings.Contains(sends[0].Fields["text"], "Hello there") {
		t.Errorf("notification text = %q", sends[0].Fields["text"])
	}
	// Depending on interleaving, "also in" is either rendered into the first send or added by
	// an edit (Telegram ignores identical re-edits). The final visible text must carry it.
	win := h.shownMessage()
	other := map[int64]string{h.accA: "b@gmail.com", h.accB: "a@gmail.com"}[win.AccountID]
	final := sends[0].Fields["text"]
	if edits := h.tg.callsOf("editMessageText"); len(edits) > 0 {
		final = edits[len(edits)-1].Fields["text"]
	}
	if !strings.Contains(final, "also in "+other) {
		t.Fatalf("final text = %q, want 'also in %s'", final, other)
	}

	// Running sync again must not resend anything.
	h.tg.reset()
	h.syncBoth()
	if n := len(h.tg.callsOf("sendMessage")); n != 0 {
		t.Errorf("re-sync sent %d messages", n)
	}

	// Archive applies to both copies.
	h.press(notify.CB(notify.ActArchive, win.ID), win.TgMessageID)
	mods := h.gm.callsOf("modify")
	if len(mods) != 2 {
		t.Fatalf("modify calls = %+v, want 2", mods)
	}
	seen := map[string]bool{}
	for _, c := range mods {
		seen[c.Token+"/"+c.ID] = true
		if fmt.Sprint(c.Body["removeLabelIds"]) != "[INBOX]" {
			t.Errorf("modify body = %v", c.Body)
		}
	}
	if !seen["tokA/a1"] || !seen["tokB/b1"] {
		t.Errorf("archived copies = %v", seen)
	}
	for _, id := range []int64{1, 2} {
		if m, _ := h.st.GetMessage(h.ctx, id); m.Status != "archived" {
			t.Errorf("message %d status = %q", id, m.Status)
		}
	}
}

func TestPausedAccountDoesNotSwallowMail(t *testing.T) {
	h := newHarness(t)
	h.st.SetAccountNotify(h.ctx, h.accB, false)
	// The paused account syncs first; the active one must still show the mail.
	if err := h.sy.Sync(h.ctx, h.accB); err != nil {
		t.Fatal(err)
	}
	if err := h.sy.Sync(h.ctx, h.accA); err != nil {
		t.Fatal(err)
	}
	if n := len(h.tg.callsOf("sendMessage")); n != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", n)
	}
	if m := h.shownMessage(); m.AccountID != h.accA {
		t.Errorf("shown from account %d, want A", m.AccountID)
	}
}

func TestNativeReplyIsThreaded(t *testing.T) {
	h := newHarness(t)
	h.syncBoth()
	win := h.shownMessage()

	h.text("See you at noon", win.TgMessageID)
	sends := h.gm.callsOf("send")
	if len(sends) != 1 {
		t.Fatalf("gmail send calls = %d", len(sends))
	}
	c := sends[0]
	wantTok := map[int64]string{h.accA: "tokA", h.accB: "tokB"}[win.AccountID]
	if c.Token != wantTok || c.Body["threadId"] != win.ThreadID {
		t.Errorf("sent via %s thread %v, want %s thread %s", c.Token, c.Body["threadId"], wantTok, win.ThreadID)
	}
	raw, _ := base64.URLEncoding.DecodeString(c.Body["raw"].(string))
	p, err := mailparse.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.InReplyTo != "<same-id@example.com>" || p.Subject != "Re: Hello there" || !strings.Contains(p.Text, "See you at noon") {
		t.Errorf("reply = %+v", p)
	}
	if !strings.Contains(strings.Join(p.To, ","), "alice@example.com") {
		t.Errorf("reply to = %v", p.To)
	}
}

func TestComposeWizard(t *testing.T) {
	h := newHarness(t)
	h.st.SetDefaultAccount(h.ctx, h.uid, h.accB)
	h.text("/new", 0)
	h.text("carol@example.com", 0)
	h.text("Plans", 0)
	h.text("Dinner on Friday?", 0)

	c, err := h.st.GetConv(h.ctx, h.uid)
	if err != nil || c.Kind != convCompose {
		t.Fatalf("conv = %+v %v", c, err)
	}
	var d draft
	json.Unmarshal([]byte(c.Payload), &d)
	if d.To != "carol@example.com" || d.Subject != "Plans" || d.Body != "Dinner on Friday?" || d.Awaiting != "" {
		t.Fatalf("draft = %+v", d)
	}
	h.press("c:send", d.WizardID)

	sends := h.gm.callsOf("send")
	if len(sends) != 1 || sends[0].Token != "tokB" {
		t.Fatalf("send calls = %+v (want one from default account B)", sends)
	}
	raw, _ := base64.URLEncoding.DecodeString(sends[0].Body["raw"].(string))
	p, _ := mailparse.Parse(raw)
	if p.Subject != "Plans" || p.FromAddr != "b@gmail.com" || !strings.Contains(strings.Join(p.To, ","), "carol@example.com") {
		t.Errorf("composed = %+v", p)
	}
	if _, err := h.st.GetConv(h.ctx, h.uid); err != store.ErrNotFound {
		t.Error("conversation not cleared after send")
	}
}

func TestAllowlist(t *testing.T) {
	h := newHarness(t)
	h.update(&models.Update{Message: &models.Message{ID: 1, From: &models.User{ID: 666},
		Chat: models.Chat{ID: 666, Type: models.ChatTypePrivate}, Text: "/start"}})
	sends := h.tg.callsOf("sendMessage")
	if len(sends) != 1 || !strings.Contains(sends[0].Fields["text"], "private") {
		t.Fatalf("stranger got %+v", sends)
	}
	if _, err := h.st.GetUser(h.ctx, 666); err != store.ErrNotFound {
		t.Error("stranger was registered")
	}
}
