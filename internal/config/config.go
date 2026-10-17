// Package config loads gmailbot configuration from a YAML file and GMAILBOT_* environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram Telegram `yaml:"telegram"`
	Google   Google   `yaml:"google"`
	OAuth    OAuth    `yaml:"oauth"`
	DB       DB       `yaml:"db"`
	Sync     Sync     `yaml:"sync"`
	PubSub   PubSub   `yaml:"pubsub"`
	Log      Log      `yaml:"log"`
}

type Telegram struct {
	Token          string  `yaml:"token"`
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	APIURL         string  `yaml:"api_url"` // optional, for self-hosted Bot API servers and tests
}

type Google struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

type OAuth struct {
	ListenAddr string `yaml:"listen_addr"`
	PublicURL  string `yaml:"public_url"`
}

type DB struct {
	Path          string `yaml:"path"`
	EncryptionKey string `yaml:"encryption_key"`
}

type Sync struct {
	Mode                  string        `yaml:"mode"` // poll | pubsub
	PollInterval          time.Duration `yaml:"poll_interval"`
	FallbackPollInterval  time.Duration `yaml:"fallback_poll_interval"`
	Workers               int           `yaml:"workers"`
	MaxBodyChars          int           `yaml:"max_body_chars"`
	MaxAttachmentMB       int           `yaml:"max_attachment_mb"`
	CacheTTL              time.Duration `yaml:"cache_ttl"`
	Dedup                 bool          `yaml:"dedup"`
	DedupActionsAllCopies bool          `yaml:"dedup_actions_all_copies"`
}

type PubSub struct {
	ProjectID    string `yaml:"project_id"`
	Topic        string `yaml:"topic"`
	Subscription string `yaml:"subscription"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns a config with every optional value filled in.
func Default() Config {
	return Config{
		OAuth: OAuth{ListenAddr: ":8080", PublicURL: "http://localhost:8080"},
		DB:    DB{Path: "./data/gmailbot.db"},
		Sync: Sync{
			Mode:                  "poll",
			PollInterval:          60 * time.Second,
			FallbackPollInterval:  15 * time.Minute,
			Workers:               4,
			MaxBodyChars:          3500,
			MaxAttachmentMB:       20,
			CacheTTL:              7 * 24 * time.Hour,
			Dedup:                 true,
			DedupActionsAllCopies: true,
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// Load reads path (may be empty) and then applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(&cfg, os.LookupEnv); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// applyEnv maps GMAILBOT_<SECTION>_<KEY> (yaml names upper-cased) onto cfg.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	root := reflect.ValueOf(cfg).Elem()
	for i := 0; i < root.NumField(); i++ {
		sec := root.Field(i)
		secName := strings.ToUpper(root.Type().Field(i).Tag.Get("yaml"))
		for j := 0; j < sec.NumField(); j++ {
			f := sec.Field(j)
			key := "GMAILBOT_" + secName + "_" + strings.ToUpper(sec.Type().Field(j).Tag.Get("yaml"))
			v, ok := lookup(key)
			if !ok {
				continue
			}
			if err := setField(f, v); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

func setField(f reflect.Value, v string) error {
	switch f.Interface().(type) {
	case string:
		f.SetString(v)
	case bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		f.SetInt(int64(n))
	case time.Duration:
		d, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
	case []int64:
		var ids []int64
		for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return err
			}
			ids = append(ids, n)
		}
		f.Set(reflect.ValueOf(ids))
	default:
		return fmt.Errorf("unsupported type %s", f.Type())
	}
	return nil
}

func (c Config) Validate() error {
	var errs []error
	if c.Telegram.Token == "" {
		errs = append(errs, errors.New("telegram.token is required"))
	}
	if c.Google.ClientID == "" || c.Google.ClientSecret == "" {
		errs = append(errs, errors.New("google.client_id and google.client_secret are required"))
	}
	if c.OAuth.PublicURL == "" {
		errs = append(errs, errors.New("oauth.public_url is required"))
	}
	switch c.Sync.Mode {
	case "poll":
	case "pubsub":
		if c.PubSub.ProjectID == "" || c.PubSub.Topic == "" || c.PubSub.Subscription == "" {
			errs = append(errs, errors.New("pubsub.project_id, pubsub.topic and pubsub.subscription are required when sync.mode=pubsub"))
		}
	default:
		errs = append(errs, fmt.Errorf("sync.mode must be poll or pubsub, got %q", c.Sync.Mode))
	}
	if c.Sync.PollInterval < 10*time.Second {
		errs = append(errs, errors.New("sync.poll_interval must be at least 10s"))
	}
	if c.Sync.Workers < 1 {
		errs = append(errs, errors.New("sync.workers must be >= 1"))
	}
	if c.DB.EncryptionKey != "" {
		if _, err := c.EncryptionKeyBytes(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EncryptionKeyBytes decodes db.encryption_key; nil means encryption disabled.
func (c Config) EncryptionKeyBytes() ([]byte, error) {
	if c.DB.EncryptionKey == "" {
		return nil, nil
	}
	k, err := base64.StdEncoding.DecodeString(c.DB.EncryptionKey)
	if err != nil || len(k) != 32 {
		return nil, errors.New("db.encryption_key must be base64 of exactly 32 bytes (openssl rand -base64 32)")
	}
	return k, nil
}

// Allowed reports whether a Telegram user may use the bot.
func (c Config) Allowed(userID int64) bool {
	if len(c.Telegram.AllowedUserIDs) == 0 {
		return true
	}
	for _, id := range c.Telegram.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// RedirectURL is the OAuth callback registered with Google.
func (c Config) RedirectURL() string {
	return strings.TrimRight(c.OAuth.PublicURL, "/") + "/oauth/callback"
}
