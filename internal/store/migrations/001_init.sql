CREATE TABLE users (
    tg_user_id            INTEGER PRIMARY KEY,
    chat_id               INTEGER NOT NULL,
    notifications_enabled INTEGER NOT NULL DEFAULT 1,
    default_account_id    INTEGER,
    created_at            INTEGER NOT NULL
);

CREATE TABLE accounts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    tg_user_id       INTEGER NOT NULL REFERENCES users(tg_user_id) ON DELETE CASCADE,
    email            TEXT    NOT NULL,
    token_enc        BLOB    NOT NULL,
    history_id       INTEGER NOT NULL DEFAULT 0,
    watch_expiration INTEGER NOT NULL DEFAULT 0,
    notify_enabled   INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    UNIQUE (tg_user_id, email)
);
CREATE INDEX accounts_email ON accounts(email);

CREATE TABLE messages (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    gmail_id       TEXT    NOT NULL,
    thread_id      TEXT    NOT NULL DEFAULT '',
    chat_id        INTEGER NOT NULL,
    tg_message_id  INTEGER,
    from_name      TEXT    NOT NULL DEFAULT '',
    from_addr      TEXT    NOT NULL DEFAULT '',
    reply_to       TEXT    NOT NULL DEFAULT '',
    to_addrs       TEXT    NOT NULL DEFAULT '',
    cc_addrs       TEXT    NOT NULL DEFAULT '',
    subject        TEXT    NOT NULL DEFAULT '',
    snippet        TEXT    NOT NULL DEFAULT '',
    body_text      TEXT    NOT NULL DEFAULT '',
    rfc_message_id TEXT    NOT NULL DEFAULT '',
    references_hdr TEXT    NOT NULL DEFAULT '',
    date_hdr       TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT '',
    expanded       INTEGER NOT NULL DEFAULT 0,
    dup_of         INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    received_at    INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    UNIQUE (account_id, gmail_id)
);
CREATE INDEX messages_tg ON messages(chat_id, tg_message_id);
CREATE INDEX messages_dup ON messages(dup_of);
CREATE INDEX messages_created ON messages(created_at);

CREATE TABLE attachments (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    idx        INTEGER NOT NULL,
    filename   TEXT    NOT NULL,
    mime       TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    PRIMARY KEY (message_id, idx)
);

CREATE TABLE seen_mail (
    tg_user_id INTEGER NOT NULL REFERENCES users(tg_user_id) ON DELETE CASCADE,
    dedup_key  TEXT    NOT NULL,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    seen_at    INTEGER NOT NULL,
    PRIMARY KEY (tg_user_id, dedup_key)
);

CREATE TABLE blacklist (
    tg_user_id INTEGER NOT NULL REFERENCES users(tg_user_id) ON DELETE CASCADE,
    pattern    TEXT    NOT NULL,
    PRIMARY KEY (tg_user_id, pattern)
);

CREATE TABLE oauth_states (
    state      TEXT PRIMARY KEY,
    tg_user_id INTEGER NOT NULL,
    chat_id    INTEGER NOT NULL,
    verifier   TEXT    NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE conv_state (
    tg_user_id INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL,
    payload    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
