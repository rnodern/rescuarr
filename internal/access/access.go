// Package access manages the local owner and hashed browser credentials.
package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

const (
	TokenFilename = "setup-token"
	Owner         = domain.ActorID("owner")
	tokenLifetime = 15 * time.Minute
	sessionLimit  = 24 * time.Hour
	idleLimit     = 30 * time.Minute
	passwordKind  = "argon2id-64m-3-2"
)

var (
	ErrDenied   = errors.New("invalid or expired credentials")
	ErrBusy     = errors.New("a password check is already in progress")
	ErrPassword = errors.New("password must contain between 12 and 256 bytes")
	ErrNoOwner  = errors.New("initial owner has not been configured")
)

type Auth struct {
	store     *store.Store
	dir       string
	passwords chan struct{}
	setup     sync.Mutex
	now       func() time.Time
}

// New uses an explicit clock and allows one bounded memory-hard operation at a time.
func New(s *store.Store, dir string, clock func() time.Time) *Auth {
	return &Auth{store: s, dir: dir, passwords: make(chan struct{}, 1), now: clock}
}

func token() string {
	value := make([]byte, 32)
	rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func EqualToken(first, second string) bool {
	return len(first) == 43 && len(second) == 43 && subtle.ConstantTimeCompare([]byte(first), []byte(second)) == 1
}

// BrowserToken makes a random nonce for the pre-authentication form cookie.
func BrowserToken() string { return token() }

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (a *Auth) ownerExists(ctx context.Context) (bool, error) {
	var exists bool
	err := a.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM owner)").Scan(&exists)
	})
	return exists, err
}

func (a *Auth) SetupRequired(ctx context.Context) (bool, error) {
	exists, err := a.ownerExists(ctx)
	return !exists, err
}

func (a *Auth) PrepareSetup(ctx context.Context) error {
	a.setup.Lock()
	defer a.setup.Unlock()
	var expiry string
	var hash []byte
	err := a.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT token_hash, expires_at FROM setup_token WHERE singleton = 1").
			Scan(&hash, &expiry)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && expiry > stamp(a.now()) && a.hasTokenFile(hash) {
		return nil
	}
	exists, err := a.ownerExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return a.removeTokenFile()
	}
	return a.issue(ctx, "setup")
}

func (a *Auth) IssueRecovery(ctx context.Context) error {
	a.setup.Lock()
	defer a.setup.Unlock()
	return a.issue(ctx, "recovery")
}

func (a *Auth) issue(ctx context.Context, purpose string) error {
	value := token()
	now := a.now()
	err := a.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM owner)").Scan(&exists); err != nil {
			return err
		}
		if purpose == "recovery" {
			if !exists {
				return ErrNoOwner
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM owner_sessions"); err != nil {
				return err
			}
			if err := appendAudit(ctx, tx, "owner_recovery_requested", "local-cli", now); err != nil {
				return err
			}
		} else if exists {
			return ErrDenied
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO setup_token (singleton, token_hash, purpose, expires_at) VALUES (1, ?, ?, ?)
			ON CONFLICT (singleton) DO UPDATE SET token_hash = excluded.token_hash,
				purpose = excluded.purpose, expires_at = excluded.expires_at`,
			digest(value), purpose, stamp(now.Add(tokenLifetime)))
		return err
	})
	if err != nil {
		return err
	}
	return a.writeTokenFile(value)
}

func (a *Auth) hasTokenFile(hash []byte) bool {
	path := filepath.Join(a.dir, TokenFilename)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 44 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 44))
	return err == nil && subtle.ConstantTimeCompare(digest(strings.TrimSuffix(string(value), "\n")), hash) == 1
}

func (a *Auth) writeTokenFile(value string) error {
	f, err := os.CreateTemp(a.dir, ".setup-token-")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	_, writeErr := f.WriteString(value + "\n")
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(a.dir, TokenFilename))
}

func (a *Auth) removeTokenFile() error {
	err := os.Remove(filepath.Join(a.dir, TokenFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *Auth) SetOwner(ctx context.Context, presented, password string) error {
	a.setup.Lock()
	defer a.setup.Unlock()
	if len(presented) != 43 {
		return ErrDenied
	}
	var saved []byte
	var expires string
	err := a.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT token_hash, expires_at FROM setup_token WHERE singleton = 1").
			Scan(&saved, &expires)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	hash := digest(presented)
	if expires <= stamp(a.now()) || subtle.ConstantTimeCompare(hash, saved) != 1 {
		return ErrDenied
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	passwordHash, err := a.hashPassword(password, salt)
	if err != nil {
		return err
	}
	now := a.now()
	err = a.store.Write(ctx, func(tx *sql.Tx) error {
		var purpose string
		err := tx.QueryRowContext(ctx, "DELETE FROM setup_token WHERE token_hash = ? AND expires_at > ? RETURNING purpose",
			hash, stamp(now)).Scan(&purpose)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDenied
		}
		if err != nil {
			return err
		}
		if purpose == "setup" {
			if _, err := tx.ExecContext(ctx, "INSERT INTO owner VALUES (1, ?, ?, ?, 1)", passwordHash, salt, passwordKind); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, "UPDATE owner SET password_hash = ?, password_salt = ?, password_scheme = ?, auth_revision = auth_revision + 1", passwordHash, salt, passwordKind); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM owner_sessions"); err != nil {
				return err
			}
		}
		return appendAudit(ctx, tx, "owner_"+purpose+"_completed", Owner, now)
	})
	if err != nil {
		return err
	}
	// A completed transaction makes the token unusable even if file removal fails.
	return a.removeTokenFile()
}

func (a *Auth) hashPassword(password string, salt []byte) ([]byte, error) {
	if len(password) < 12 || len(password) > 256 {
		return nil, ErrPassword
	}
	select {
	case a.passwords <- struct{}{}:
		defer func() { <-a.passwords }()
	default:
		return nil, ErrBusy
	}
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32), nil
}

func appendAudit(ctx context.Context, tx *sql.Tx, kind string, actor domain.ActorID, now time.Time) error {
	return store.AppendEvent(ctx, tx, domain.Event{
		ID: domain.EventID(store.NewID()), Kind: domain.EventKind(kind), Actor: actor, OccurredAt: now,
	})
}
