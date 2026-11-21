package storage

import (
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Test hooks for the storage waits and the bucket creation. Each sets what
// a test needs and returns the function that puts the previous value back;
// each panics outside a test binary.

// SetS3TimingsForTest sets the storage's readiness wait and poll and its
// deletion wait and poll.
func SetS3TimingsForTest(readyTimeout, readyPoll, deleteTimeout, deletePoll time.Duration) (restore func()) {
	mustBeTest("SetS3TimingsForTest")
	prevReady, prevReadyPoll, prevDelete, prevDeletePoll := S3ReadyTimeout, S3ReadyPoll, S3DeleteTimeout, S3DeletePoll
	S3ReadyTimeout, S3ReadyPoll, S3DeleteTimeout, S3DeletePoll = readyTimeout, readyPoll, deleteTimeout, deletePoll
	return func() {
		S3ReadyTimeout, S3ReadyPoll, S3DeleteTimeout, S3DeletePoll = prevReady, prevReadyPoll, prevDelete, prevDeletePoll
	}
}

// SetS3ReadyTimeoutForTest sets the storage's readiness wait.
func SetS3ReadyTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetS3ReadyTimeoutForTest")
	prev := S3ReadyTimeout
	S3ReadyTimeout = d
	return func() { S3ReadyTimeout = prev }
}

// SetBucketCreatorForTest makes create the creation of the pipelines bucket.
func SetBucketCreatorForTest(create func(c *kube.Client, bucket string) error) (restore func()) {
	mustBeTest("SetBucketCreatorForTest")
	prev := s3BucketCreator
	s3BucketCreator = create
	return func() { s3BucketCreator = prev }
}

// SetBucketRetryDelayForTest sets the delay between bucket creation tries.
func SetBucketRetryDelayForTest(d time.Duration) (restore func()) {
	mustBeTest("SetBucketRetryDelayForTest")
	prev := s3BucketRetryDelay
	s3BucketRetryDelay = d
	return func() { s3BucketRetryDelay = prev }
}

// SetMigrateTimingsForTest sets the migration's copy wait, its poll and the
// wait for a moved pipeline server.
func SetMigrateTimingsForTest(copyTimeout, poll, repointTimeout time.Duration) (restore func()) {
	mustBeTest("SetMigrateTimingsForTest")
	prevCopy, prevPoll, prevRepoint := MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout
	MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout = copyTimeout, poll, repointTimeout
	return func() { MigrateCopyTimeout, MigratePoll, MigrateRepointTimeout = prevCopy, prevPoll, prevRepoint }
}

// SetMigrateRepointTimeoutForTest sets the wait for a moved pipeline server.
func SetMigrateRepointTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetMigrateRepointTimeoutForTest")
	prev := MigrateRepointTimeout
	MigrateRepointTimeout = d
	return func() { MigrateRepointTimeout = prev }
}

// SetRemoveAfterCopyReserveForTest sets the time the final copy leaves for
// the deletions after it.
func SetRemoveAfterCopyReserveForTest(d time.Duration) (restore func()) {
	mustBeTest("SetRemoveAfterCopyReserveForTest")
	prev := removeAfterCopyReserve
	removeAfterCopyReserve = d
	return func() { removeAfterCopyReserve = prev }
}

// SetPipelineServerDeleteTimingForTest sets the pipeline server deletion
// wait and its poll.
func SetPipelineServerDeleteTimingForTest(timeout, poll time.Duration) (restore func()) {
	mustBeTest("SetPipelineServerDeleteTimingForTest")
	prevTimeout, prevPoll := PipelineServerDeleteTimeout, PipelineServerDeletePoll
	PipelineServerDeleteTimeout, PipelineServerDeletePoll = timeout, poll
	return func() { PipelineServerDeleteTimeout, PipelineServerDeletePoll = prevTimeout, prevPoll }
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
