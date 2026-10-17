package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Account struct {
	ID              int64
	TgUserID        int64
	Email           string
	TokenEnc        []byte
	HistoryID       uint64
	WatchExpiration time.Time
	NotifyEnabled   bool
}

const accountCols = `id, tg_user_id, email, token_enc, history_id, watch_expiration, notify_enabled`

func scanAccount(sc interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var hist int64
	var exp int64
	err := sc.Scan(&a.ID, &a.TgUserID, &a.Email, &a.TokenEnc, &hist, &exp, &a.NotifyEnabled)
	a.HistoryID = uint64(hist)
	if exp > 0 {
		a.WatchExpiration = time.UnixMilli(exp)
	}
	return a, err
}

func (s *Store) queryAccounts(ctx context.Context, q string, args ...any) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAccount links (or relinks) an email to a Telegram user and returns its id and
// whether it was newly created. historyID is only applied to new links so relinking keeps the cursor.
func (s *Store) UpsertAccount(ctx context.Context, tgUserID int64, email string, tokenEnc []byte, historyID uint64) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE tg_user_id = ? AND email = ?`, tgUserID, email).Scan(&id)
	created := false
	switch {
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET token_enc = ? WHERE id = ?`, tokenEnc, id)
	case errors.Is(err, sql.ErrNoRows):
		created = true
		err = tx.QueryRowContext(ctx, `
			INSERT INTO accounts (tg_user_id, email, token_enc, history_id, created_at) VALUES (?, ?, ?, ?, ?)
			RETURNING id`, tgUserID, email, tokenEnc, int64(historyID), s.now().Unix()).Scan(&id)
	}
	if err != nil {
		return 0, false, err
	}
	return id, created, tx.Commit()
}

func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
	return a, notFound(err)
}

func (s *Store) AccountsByUser(ctx context.Context, tgUserID int64) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountCols+` FROM accounts WHERE tg_user_id = ? ORDER BY id`, tgUserID)
}

func (s *Store) AccountsByEmail(ctx context.Context, email string) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountCols+` FROM accounts WHERE email = ? COLLATE NOCASE`, email)
}

func (s *Store) AllAccounts(ctx context.Context) ([]Account, error) {
	return s.queryAccounts(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY id`)
}

func (s *Store) UpdateToken(ctx context.Context, id int64, tokenEnc []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET token_enc = ? WHERE id = ?`, tokenEnc, id)
	return err
}

// AdvanceHistory moves the cursor forward only (concurrent syncs never rewind it).
func (s *Store) AdvanceHistory(ctx context.Context, id int64, historyID uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET history_id = MAX(history_id, ?) WHERE id = ?`, int64(historyID), id)
	return err
}

// ResetHistory forces the cursor, used when Gmail reports the old one expired.
func (s *Store) ResetHistory(ctx context.Context, id int64, historyID uint64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET history_id = ? WHERE id = ?`, int64(historyID), id)
	return err
}

func (s *Store) SetWatchExpiration(ctx context.Context, id int64, exp time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET watch_expiration = ? WHERE id = ?`, exp.UnixMilli(), id)
	return err
}

func (s *Store) SetAccountNotify(ctx context.Context, id int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET notify_enabled = ? WHERE id = ?`, b2i(on), id)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET default_account_id = NULL WHERE default_account_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
