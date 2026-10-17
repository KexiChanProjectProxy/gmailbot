// Package oauthweb serves the Google OAuth redirect flow used to link Gmail accounts.
package oauthweb

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
)

// LinkedFunc is called after an account has been stored.
type LinkedFunc func(ctx context.Context, tgUserID, chatID, accountID int64, email string, created bool)

type Server struct {
	OAuth    *oauth2.Config
	Store    *store.Store
	Gmail    *gmailx.Factory
	OnLinked LinkedFunc
	Log      *slog.Logger
	// Exchange is overridable for tests; defaults to OAuth.Exchange.
	Exchange func(ctx context.Context, code, verifier string) (*oauth2.Token, error)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/start", s.start)
	mux.HandleFunc("GET /oauth/callback", s.callback)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// Run serves on addr until ctx is cancelled.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	s.Log.Info("oauth server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.PeekOAuthState(r.Context(), r.URL.Query().Get("s"))
	if err != nil {
		page(w, http.StatusBadRequest, "Link expired", "This link has expired or was already used. Send /start to the bot again.")
		return
	}
	url := s.OAuth.AuthCodeURL(st.State,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce, // always return a refresh token
		oauth2.S256ChallengeOption(st.Verifier),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		page(w, http.StatusBadRequest, "Authorization cancelled", "Google returned: "+e+". You can close this tab and try /start again.")
		return
	}
	st, err := s.Store.TakeOAuthState(ctx, q.Get("state"))
	if err != nil {
		page(w, http.StatusBadRequest, "Link expired", "This authorization request expired. Send /start to the bot again.")
		return
	}
	if !strings.Contains(q.Get("scope"), gmailx.Scope) && q.Get("scope") != "" {
		page(w, http.StatusBadRequest, "Permission missing", "Gmail access was not granted. Run /start again and tick the Gmail checkbox on Google's consent screen.")
		return
	}
	exchange := s.Exchange
	if exchange == nil {
		exchange = func(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
			return s.OAuth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		}
	}
	tok, err := exchange(ctx, q.Get("code"), st.Verifier)
	if err != nil {
		s.Log.Error("oauth exchange", "err", err)
		page(w, http.StatusBadGateway, "Authorization failed", "Could not exchange the code with Google. Please try /start again.")
		return
	}
	if tok.RefreshToken == "" {
		page(w, http.StatusBadRequest, "No refresh token", "Google did not return a refresh token. Remove the app at myaccount.google.com/permissions and try /start again.")
		return
	}

	c, err := s.Gmail.NewClient(ctx, 0, "", s.OAuth.TokenSource(context.Background(), tok))
	if err != nil {
		s.fail(w, err)
		return
	}
	email, hist, err := c.Profile(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	blob, err := s.Gmail.EncodeToken(tok)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.UpsertUser(ctx, st.TgUserID, st.ChatID); err != nil {
		s.fail(w, err)
		return
	}
	id, created, err := s.Store.UpsertAccount(ctx, st.TgUserID, email, blob, hist)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Gmail.Forget(id) // drop any cached client holding the old token
	s.Log.Info("account linked", "tg_user", st.TgUserID, "email", email, "created", created)
	if s.OnLinked != nil {
		go s.OnLinked(context.WithoutCancel(ctx), st.TgUserID, st.ChatID, id, email, created)
	}
	page(w, http.StatusOK, "Connected", email+" is now linked. You can close this tab and return to Telegram.")
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("oauth callback", "err", err)
	page(w, http.StatusInternalServerError, "Something went wrong", "Please try /start again.")
}

var tmpl = template.Must(template.New("p").Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · Gmail Bot</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#222;background:#fff}
@media(prefers-color-scheme:dark){body{color:#eee;background:#111}}h1{font-size:1.4rem}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Body}}</p></body></html>`))

func page(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	tmpl.Execute(w, struct{ Title, Body string }{title, body})
}
