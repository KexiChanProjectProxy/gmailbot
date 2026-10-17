package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFileAndEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`
telegram: { token: "file-token", allowed_user_ids: [1, 2] }
google: { client_id: "id", client_secret: "secret" }
sync: { poll_interval: 30s }
`), 0o600)
	t.Setenv("GMAILBOT_TELEGRAM_TOKEN", "env-token")
	t.Setenv("GMAILBOT_SYNC_DEDUP", "false")
	t.Setenv("GMAILBOT_TELEGRAM_ALLOWED_USER_IDS", "5,6")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.Token != "env-token" || c.Sync.PollInterval != 30*time.Second || c.Sync.Dedup {
		t.Errorf("config = %+v", c)
	}
	if !c.Allowed(5) || c.Allowed(1) {
		t.Errorf("allowlist = %v", c.Telegram.AllowedUserIDs)
	}
	if c.RedirectURL() != "http://localhost:8080/oauth/callback" {
		t.Errorf("redirect = %s", c.RedirectURL())
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if c.Validate() == nil {
		t.Error("empty config must fail")
	}
	c.Telegram.Token, c.Google.ClientID, c.Google.ClientSecret = "t", "i", "s"
	if err := c.Validate(); err != nil {
		t.Errorf("minimal config: %v", err)
	}
	c.Sync.Mode = "pubsub"
	if c.Validate() == nil {
		t.Error("pubsub without project must fail")
	}
	c.Sync.Mode = "poll"
	c.DB.EncryptionKey = "short"
	if c.Validate() == nil {
		t.Error("bad key must fail")
	}
	if !Default().Allowed(42) {
		t.Error("empty allowlist must allow everyone")
	}
}
