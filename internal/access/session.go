package access

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"
)

type Session struct {
	Token     string
	CSRF      string
	ExpiresAt time.Time
}

func (a *Auth) Login(ctx context.Context, password, existing string) (Session, error) {
	var saved, salt []byte
	var scheme string
	var revision int
	err := a.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT password_hash, password_salt, password_scheme, auth_revision FROM owner").
			Scan(&saved, &salt, &scheme, &revision)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrDenied
	}
	if err != nil {
		return Session{}, err
	}
	if scheme != passwordKind || len(salt) != 16 || len(saved) != 32 {
		return Session{}, ErrDenied
	}
	hash, err := a.hashPassword(password, salt)
	if err != nil {
		if errors.Is(err, ErrPassword) {
			return Session{}, ErrDenied
		}
		return Session{}, err
	}
	if subtle.ConstantTimeCompare(hash, saved) != 1 {
		return Session{}, ErrDenied
	}
	now := a.now()
	session := Session{Token: token(), CSRF: token(), ExpiresAt: now.Add(sessionLimit)}
	err = a.store.Write(ctx, func(tx *sql.Tx) error {
		if len(existing) == 43 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM owner_sessions WHERE token_hash = ?", digest(existing)); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO owner_sessions (token_hash, revision, csrf_secret, expires_at, idle_until)
			SELECT ?, auth_revision, ?, ?, ? FROM owner WHERE auth_revision = ?
				AND NOT EXISTS (SELECT 1 FROM setup_token WHERE purpose = 'recovery' AND expires_at > ?)`,
			digest(session.Token), []byte(session.CSRF), stamp(session.ExpiresAt), stamp(now.Add(idleLimit)), revision, stamp(now))
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrDenied
		}
		return nil
	})
	return session, err
}

func (a *Auth) Authenticate(ctx context.Context, presented string, extend bool) (Session, error) {
	if len(presented) != 43 {
		return Session{}, ErrDenied
	}
	now := a.now()
	hash := digest(presented)
	var csrf []byte
	var expiry, idle string
	err := a.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT csrf_secret, expires_at, idle_until FROM owner_sessions
			WHERE token_hash = ? AND revision = (SELECT auth_revision FROM owner)
				AND expires_at > ? AND idle_until > ?`, hash, stamp(now), stamp(now)).Scan(&csrf, &expiry, &idle)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrDenied
	}
	if err != nil {
		return Session{}, err
	}
	expiration, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil {
		return Session{}, err
	}
	if extend && idle < stamp(now.Add(idleLimit-time.Minute)) {
		err := a.store.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE owner_sessions SET idle_until = ? WHERE token_hash = ? AND idle_until = ?",
				stamp(now.Add(idleLimit)), hash, idle)
			return err
		})
		if err != nil {
			return Session{}, err
		}
	}
	return Session{Token: presented, CSRF: string(csrf), ExpiresAt: expiration}, nil
}

func (a *Auth) Logout(ctx context.Context, presented string) error {
	if len(presented) != 43 {
		return ErrDenied
	}
	return a.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM owner_sessions WHERE token_hash = ?", digest(presented))
		return err
	})
}

func (a *Auth) PruneSessions(ctx context.Context) error {
	now := stamp(a.now())
	return a.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM owner_sessions WHERE token_hash IN (
				SELECT token_hash FROM owner_sessions WHERE idle_until <= ? OR expires_at <= ? LIMIT 128)`, now, now)
		return err
	})
}
