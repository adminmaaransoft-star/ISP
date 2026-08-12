package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// StaffStore serves staff account lookups for the operations console.
type StaffStore struct {
	pool dbPool
}

// NewStaffStore constructs a StaffStore.
func NewStaffStore(pool dbPool) *StaffStore { return &StaffStore{pool: pool} }

// GetStaffByUsername returns an active staff account, or nil when there is no
// such account or it has been deactivated.
//
// Deactivated accounts are indistinguishable from missing ones to the caller
// on purpose: a login form that answered "this account is disabled" would
// confirm to an outsider that the username is real.
func (s *StaffStore) GetStaffByUsername(ctx context.Context, username string) (*staffui.StaffAccount, error) {
	const q = `
		SELECT id, username, password_hash, full_name, role, lea_access
		  FROM staff_users
		 WHERE username = $1 AND active`

	var a staffui.StaffAccount
	err := s.pool.QueryRow(ctx, q, username).Scan(
		&a.ID, &a.Username, &a.PasswordHash, &a.FullName, &a.Role, &a.LeaAccess)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get staff %q: %w", username, err)
	}
	return &a, nil
}

// TouchStaffLogin records a successful sign-in.
//
// Failures are the caller's to ignore: this is an audit convenience, and
// refusing a login because the timestamp could not be written would trade a
// working console for a bookkeeping detail.
func (s *StaffStore) TouchStaffLogin(ctx context.Context, staffID int) error {
	const q = `UPDATE staff_users SET last_login_at = $2 WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, staffID, time.Now()); err != nil {
		return fmt.Errorf("db: touch staff login %d: %w", staffID, err)
	}
	return nil
}

// Staff exposes the staff account store.
func (d *DB) Staff() *StaffStore { return NewStaffStore(d.pool) }
