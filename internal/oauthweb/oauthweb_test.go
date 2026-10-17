package oauthweb

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/kexiChanProjectProxy/gmailbot/internal/crypto"
	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

func TestCallbackLinksAccount(t *testing.T) {
	ctx := context.Background()
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"emailAddress":"me@gmail.com","historyId":"777"}`)
	}))
	defer gsrv.Close()
	st, _ := store.Open(ctx, t.TempDir()+"/db")
	defer st.Close()
	sealer, _ := crypto.New(nil)
	oc := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: gsrv.URL}, RedirectURL: "http://x/oauth/callback", Scopes: []string{gmailx.Scope}}

	var mu sync.Mutex
	var linked []string
	done := make(chan struct{}, 2)
	srv := &Server{
		OAuth: oc, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Gmail: &gmailx.Factory{OAuth: oc, Store: st, Sealer: sealer, Options: []option.ClientOption{option.WithEndpoint(gsrv.URL + "/")}},
		Exchange: func(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
			if code != "good" || verifier != "ver" {
				t.Errorf("exchange got code=%q verifier=%q", code, verifier)
			}
			return &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}, nil
		},
		OnLinked: func(_ context.Context, uid, chat, acc int64, email string, created bool) {
			mu.Lock()
			linked = append(linked, email)
			mu.Unlock()
			done <- struct{}{}
		},
	}
	h := srv.Handler()

	st.PutOAuthState(ctx, store.OAuthState{State: "S1", TgUserID: 9, ChatID: 9, Verifier: "ver"}, time.Minute)

	// /oauth/start redirects to Google with PKCE and offline access.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/start?s=S1", nil))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.Contains(loc, "code_challenge=") || !strings.Contains(loc, "access_type=offline") || !strings.Contains(loc, "state=S1") {
		t.Fatalf("start: %d %s", rec.Code, loc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback?state=S1&code=good", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "me@gmail.com") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	<-done
	accs, _ := st.AccountsByUser(ctx, 9)
	if len(accs) != 1 || accs[0].Email != "me@gmail.com" || accs[0].HistoryID != 777 {
		t.Fatalf("accounts = %+v", accs)
	}

	// State is single-use.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback?state=S1&code=good", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reused state: %d", rec.Code)
	}

	// Relinking keeps the existing cursor.
	st.AdvanceHistory(ctx, accs[0].ID, 900)
	st.PutOAuthState(ctx, store.OAuthState{State: "S2", TgUserID: 9, ChatID: 9, Verifier: "ver"}, time.Minute)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback?state=S2&code=good", nil))
	<-done
	accs, _ = st.AccountsByUser(ctx, 9)
	if len(accs) != 1 || accs[0].HistoryID != 900 {
		t.Errorf("after relink = %+v", accs)
	}
}
