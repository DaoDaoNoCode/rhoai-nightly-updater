package cluster

import (
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The updater's own records and the operation time budget
// (internal/record), under the names pkg/api, main.go and this package use.

type (
	// OperationRecord is record.OperationRecord.
	OperationRecord = record.OperationRecord
	// LeaseOwner is record.LeaseOwner.
	LeaseOwner = record.LeaseOwner
	// LockRenewal is record.LockRenewal.
	LockRenewal = record.LockRenewal
)

// The operation locks (see record).
const (
	ClusterLockKey        = record.LeaseKey
	LockTTL               = record.LockTTL
	InterruptedLockMaxAge = record.InterruptedLockMaxAge
)

// The operation time budget (see record).
const (
	OperationDeadline   = record.OperationDeadline
	MarkerClearTimeout  = record.MarkerClearTimeout
	MarkerFlushTimeout  = record.MarkerFlushTimeout
	HTTPShutdownTimeout = record.HTTPShutdownTimeout
	LockLostMessage     = record.LockLostMessage
	MaxRecipeRunBytes   = record.MaxRecipeRunBytes
)

// RecoveryTimeout is record.RecoveryTimeout; the errors are record's
// values, so errors.Is matches either name.
var (
	RecoveryTimeout             = record.RecoveryTimeout
	ErrOperationLockLost        = record.ErrOperationLockLost
	ErrOperationRecordContended = record.ErrOperationRecordContended
	ErrRecipeRunReplaced        = record.ErrRecipeRunReplaced
)

// ShutdownDrainTimeout is record.ShutdownDrainTimeout.
func ShutdownDrainTimeout() time.Duration { return record.ShutdownDrainTimeout() }

// GetActivity is record.GetActivity.
func GetActivity(c *Client) ([]types.ActivityEntry, error) { return record.GetActivity(c) }

// ReadOperationRecord is record.ReadOperationRecord.
func ReadOperationRecord(c *Client) (*OperationRecord, error) { return record.ReadOperationRecord(c) }

// SaveOperationMarker is record.SaveOperationMarker.
func SaveOperationMarker(c *Client, marker *types.OperationMarker) error {
	return record.SaveOperationMarker(c, marker)
}

// SaveCompletedOperation is record.SaveCompletedOperation.
func SaveCompletedOperation(c *Client, done *types.CompletedOperation, owner *LeaseOwner) error {
	return record.SaveCompletedOperation(c, done, owner)
}

// LeaseLive is record.LeaseLive.
func LeaseLive(l *types.OperationLease, now time.Time, ttl time.Duration) bool {
	return record.LeaseLive(l, now, ttl)
}

// LeaseHeartbeat is record.LeaseHeartbeat.
func LeaseHeartbeat(l *types.OperationLease) (time.Time, error) { return record.LeaseHeartbeat(l) }

// AcquireOperationLock is record.AcquireOperationLock.
func AcquireOperationLock(c *Client, key, value string, check func(*OperationRecord) error) error {
	return record.AcquireOperationLock(c, key, value, check)
}

// RenewOperationLocks is record.RenewOperationLocks.
func RenewOperationLocks(c *Client, renewals []LockRenewal) (map[string]*types.OperationLease, error) {
	return record.RenewOperationLocks(c, renewals)
}

// ReleaseOperationLock is record.ReleaseOperationLock.
func ReleaseOperationLock(c *Client, key string, owner LeaseOwner) error {
	return record.ReleaseOperationLock(c, key, owner)
}

// ProjectLockKey is record.ProjectLockKey.
func ProjectLockKey(uid string) string { return record.ProjectLockKey(uid) }

// SaveRecipeRun is record.SaveRecipeRun.
func SaveRecipeRun(c *Client, run *types.RecipeRun, replace bool) error {
	return record.SaveRecipeRun(c, run, replace)
}

// ReadRecipeRun is record.ReadRecipeRun.
func ReadRecipeRun(c *Client) (*types.RecipeRun, error) { return record.ReadRecipeRun(c) }
