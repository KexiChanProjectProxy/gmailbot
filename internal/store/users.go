package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type User struct {
	TgUserID             int64
	ChatID               int64
	NotificationsEnabled bool
	DefaultAccountID     int64 // 0 when unset
}

// UpsertUser creates the user or refreshes its chat id.
func (s *Store) UpsertUser(ctx context.Context, tgUserID, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (tg_user_id, chat_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (tg_user_id) DO UPDATE SET chat_id = excluded.chat_id`,
		tgUserID, chatID, s.now().Unix())
	return err
}

func (s *Store) GetUser(ctx context.Context, tgUserID int64) (User, error) {
	var u User
	var def sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT tg_user_id, chat_id, notifications_enabled, default_account_id FROM users WHERE tg_user_id = ?`,
		tgUserID).Scan(&u.TgUserID, &u.ChatID, &u.NotificationsEnabled, &def)
	u.DefaultAccountID = def.Int64
	return u, notFound(err)
}

func (s *Store) SetNotifications(ctx context.Context, tgUserID int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET notifications_enabled = ? WHERE tg_user_id = ?`, b2i(on), tgUserID)
	return err
}

func (s *Store) SetDefaultAccount(ctx context.Context, tgUserID, accountID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET default_account_id = ? WHERE tg_user_id = ?`, accountID, tgUserID)
	return err
}

// DeleteUser removes the user and, through cascades, all their accounts and cached data.
func (s *Store) DeleteUser(ctx context.Context, tgUserID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM conv_state WHERE tg_user_id = ?`,
		`DELETE FROM oauth_states WHERE tg_user_id = ?`,
		`DELETE FROM users WHERE tg_user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, tgUserID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- blacklist ----

func (s *Store) Blacklist(ctx context.Context, tgUserID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pattern FROM blacklist WHERE tg_user_id = ? ORDER BY pattern`, tgUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) AddBlacklist(ctx context.Context, tgUserID int64, pattern string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO blacklist (tg_user_id, pattern) VALUES (?, ?)`,
		tgUserID, strings.ToLower(strings.TrimSpace(pattern)))
	return err
}

func (s *Store) RemoveBlacklist(ctx context.Context, tgUserID int64, pattern string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM blacklist WHERE tg_user_id = ? AND pattern = ?`, tgUserID, pattern)
	return err
}

// ---- oauth states ----

type OAuthState struct {
	State    string
	TgUserID int64
	ChatID   int64
	Verifier string
}

func (s *Store) PutOAuthState(ctx context.Context, st OAuthState, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_states (state, tg_user_id, chat_id, verifier, expires_at) VALUES (?, ?, ?, ?, ?)`,
		st.State, st.TgUserID, st.ChatID, st.Verifier, s.now().Add(ttl).Unix())
	return err
}

// TakeOAuthState returns and deletes a still-valid state (single use).
func (s *Store) TakeOAuthState(ctx context.Context, state string) (OAuthState, error) {
	var st OAuthState
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM oauth_states WHERE state = ? AND expires_at > ?
		RETURNING state, tg_user_id, chat_id, verifier`, state, s.now().Unix()).
		Scan(&st.State, &st.TgUserID, &st.ChatID, &st.Verifier)
	return st, notFound(err)
}

// PeekOAuthState returns a valid state without consuming it.
func (s *Store) PeekOAuthState(ctx context.Context, state string) (OAuthState, error) {
	var st OAuthState
	err := s.db.QueryRowContext(ctx, `
		SELECT state, tg_user_id, chat_id, verifier FROM oauth_states WHERE state = ? AND expires_at > ?`,
		state, s.now().Unix()).Scan(&st.State, &st.TgUserID, &st.ChatID, &st.Verifier)
	return st, notFound(err)
}

// ---- conversation state ----

type Conv struct {
	Kind    string
	Payload string
}

func (s *Store) SetConv(ctx context.Context, tgUserID int64, c Conv) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conv_state (tg_user_id, kind, payload, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (tg_user_id) DO UPDATE SET kind = excluded.kind, payload = excluded.payload, updated_at = excluded.updated_at`,
		tgUserID, c.Kind, c.Payload, s.now().Unix())
	return err
}

func (s *Store) GetConv(ctx context.Context, tgUserID int64) (Conv, error) {
	var c Conv
	err := s.db.QueryRowContext(ctx, `SELECT kind, payload FROM conv_state WHERE tg_user_id = ?`, tgUserID).Scan(&c.Kind, &c.Payload)
	return c, notFound(err)
}

func (s *Store) ClearConv(ctx context.Context, tgUserID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM conv_state WHERE tg_user_id = ?`, tgUserID)
	return err
}

// Cleanup removes cached mail older than ttl and stale transient state.
func (s *Store) Cleanup(ctx context.Context, ttl time.Duration) (int64, error) {
	now := s.now()
	cutoff := now.Add(-ttl).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM seen_mail WHERE seen_at < ?`, cutoff); err != nil {
		return n, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < ?`, now.Unix()); err != nil {
		return n, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM conv_state WHERE updated_at < ?`, now.Add(-24*time.Hour).Unix())
	return n, err
}
