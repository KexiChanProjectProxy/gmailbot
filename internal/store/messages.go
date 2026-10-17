package store

import (
	"context"
	"database/sql"
)

type Message struct {
	ID           int64
	AccountID    int64
	GmailID      string
	ThreadID     string
	ChatID       int64
	TgMessageID  int // 0 when not (yet) shown in Telegram
	FromName     string
	FromAddr     string
	ReplyTo      string
	To           string
	Cc           string
	Subject      string
	Snippet      string
	BodyText     string
	RFCMessageID string
	References   string
	DateHdr      string
	Status       string // "", archived, deleted, spam, replied, forwarded
	Expanded     bool
	DupOf        int64
	ReceivedAt   int64
	Attachments  []Attachment
}

type Attachment struct {
	Idx      int
	Filename string
	MIME     string
	Size     int64
}

const msgCols = `id, account_id, gmail_id, thread_id, chat_id, tg_message_id, from_name, from_addr, reply_to,
	to_addrs, cc_addrs, subject, snippet, body_text, rfc_message_id, references_hdr, date_hdr, status, expanded,
	dup_of, received_at`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var tg, dup sql.NullInt64
	err := sc.Scan(&m.ID, &m.AccountID, &m.GmailID, &m.ThreadID, &m.ChatID, &tg, &m.FromName, &m.FromAddr, &m.ReplyTo,
		&m.To, &m.Cc, &m.Subject, &m.Snippet, &m.BodyText, &m.RFCMessageID, &m.References, &m.DateHdr, &m.Status,
		&m.Expanded, &dup, &m.ReceivedAt)
	m.TgMessageID = int(tg.Int64)
	m.DupOf = dup.Int64
	return m, err
}

// MessageExists reports whether a Gmail message was already processed for an account.
func (s *Store) MessageExists(ctx context.Context, accountID int64, gmailID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE account_id = ? AND gmail_id = ?`, accountID, gmailID).Scan(&n)
	return n > 0, err
}

// InsertResult describes the outcome of InsertMessage.
type InsertResult struct {
	ID       int64
	Inserted bool  // false when (account, gmail id) already existed
	DupOf    int64 // non-zero when another account of the same user already showed this mail
}

// InsertMessage stores a mail and atomically claims its dedup key for the Telegram user.
// If dedupKey is empty no dedup is attempted. The whole operation is one transaction, so two
// accounts syncing the same mail concurrently produce exactly one winner.
func (s *Store) InsertMessage(ctx context.Context, tgUserID int64, dedupKey string, m Message) (InsertResult, error) {
	var res InsertResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
		INSERT INTO messages (account_id, gmail_id, thread_id, chat_id, from_name, from_addr, reply_to, to_addrs, cc_addrs,
			subject, snippet, body_text, rfc_message_id, references_hdr, date_hdr, received_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, gmail_id) DO NOTHING
		RETURNING id`,
		m.AccountID, m.GmailID, m.ThreadID, m.ChatID, m.FromName, m.FromAddr, m.ReplyTo, m.To, m.Cc,
		m.Subject, m.Snippet, m.BodyText, m.RFCMessageID, m.References, m.DateHdr, m.ReceivedAt, s.now().Unix(),
	).Scan(&res.ID)
	if err == sql.ErrNoRows {
		return res, tx.Commit() // already processed
	}
	if err != nil {
		return res, err
	}
	res.Inserted = true

	for _, a := range m.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments (message_id, idx, filename, mime, size) VALUES (?, ?, ?, ?, ?)`,
			res.ID, a.Idx, a.Filename, a.MIME, a.Size); err != nil {
			return res, err
		}
	}

	if dedupKey != "" {
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO seen_mail (tg_user_id, dedup_key, message_id, seen_at) VALUES (?, ?, ?, ?)`,
			tgUserID, dedupKey, res.ID, s.now().Unix())
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT message_id FROM seen_mail WHERE tg_user_id = ? AND dedup_key = ?`,
				tgUserID, dedupKey).Scan(&res.DupOf); err != nil {
				return res, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET dup_of = ? WHERE id = ?`, res.DupOf, res.ID); err != nil {
				return res, err
			}
		}
	}
	return res, tx.Commit()
}

func (s *Store) loadAttachments(ctx context.Context, m *Message) error {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, filename, mime, size FROM attachments WHERE message_id = ? ORDER BY idx`, m.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	m.Attachments = nil
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.Idx, &a.Filename, &a.MIME, &a.Size); err != nil {
			return err
		}
		m.Attachments = append(m.Attachments, a)
	}
	return rows.Err()
}

func (s *Store) GetMessage(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messages WHERE id = ?`, id))
	if err != nil {
		return m, notFound(err)
	}
	return m, s.loadAttachments(ctx, &m)
}

// MessageByTelegram resolves the mail shown as a given Telegram message.
func (s *Store) MessageByTelegram(ctx context.Context, chatID int64, tgMessageID int) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messages WHERE chat_id = ? AND tg_message_id = ?`,
		chatID, tgMessageID))
	if err != nil {
		return m, notFound(err)
	}
	return m, s.loadAttachments(ctx, &m)
}

// Duplicates returns the suppressed copies of a shown message.
func (s *Store) Duplicates(ctx context.Context, id int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages WHERE dup_of = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetTelegramID(ctx context.Context, id int64, tgMessageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET tg_message_id = ? WHERE id = ?`, tgMessageID, id)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) SetExpanded(ctx context.Context, id int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET expanded = ? WHERE id = ?`, b2i(on), id)
	return err
}
