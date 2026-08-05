package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/webdavspace"
)

// SaveWebDAVAccount inserts a new WebDAV delivery account. If an account
// with the same username already exists it returns an error so the caller
// can surface a 409 Conflict rather than silently overwriting credentials.
// To rotate a password, DeleteWebDAVAccount first, then SaveWebDAVAccount.
//
// passwordHash must already be a bcrypt hash produced by webdavspace
// (the plaintext never reaches the repository).
func (r *Repository) SaveWebDAVAccount(ctx context.Context, username, passwordHash string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO webdav_accounts(username,password_hash,created_at) VALUES(?,?,?)`,
		username, passwordHash, formatTime(time.Now().UTC()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return errors.New("account already exists")
	}
	return err
}

// GetWebDAVAccount returns the stored account, or (nil, false) when unknown.
func (r *Repository) GetWebDAVAccount(ctx context.Context, username string) (*webdavspace.Account, bool, error) {
	var hash, created string
	err := r.db.QueryRowContext(ctx, `SELECT password_hash,created_at FROM webdav_accounts WHERE username=?`, username).Scan(&hash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &webdavspace.Account{Username: username, PasswordHash: hash}, true, nil
}

// ListWebDAVAccounts returns every account username (sorted) for admin UI.
func (r *Repository) ListWebDAVAccounts(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT username FROM webdav_accounts ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteWebDAVAccount removes an account.
func (r *Repository) DeleteWebDAVAccount(ctx context.Context, username string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM webdav_accounts WHERE username=?`, username)
	return err
}

// WebDAVAccountStore adapts the repository to webdavspace.AccountStore for
// Basic-Auth lookups at request time.
type WebDAVAccountStore struct{ Repo *Repository }

// GetAccount implements webdavspace.AccountStore.
func (s WebDAVAccountStore) GetAccount(ctx context.Context, username string) (*webdavspace.Account, bool, error) {
	return s.Repo.GetWebDAVAccount(ctx, username)
}
