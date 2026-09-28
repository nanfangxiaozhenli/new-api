package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func setupCanvasRelayDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "canvas-relay.db")
	previous := DB
	t.Cleanup(func() { DB = previous })
	var err error
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, DB.AutoMigrate(&Token{}, &CanvasRelayDelegation{}, &Task{}))
	require.NoError(t, DB.AutoMigrate(&Token{}, &CanvasRelayDelegation{}, &Task{}))
	return path
}

func canvasRelayFixture() (*CanvasRelayDelegation, *Token, string, []byte) {
	secret := "one-use-delegation-secret"
	request := []byte(`{"model":"task-model","prompt":"test"}`)
	secretDigest := sha256.Sum256([]byte(secret))
	payloadDigest := sha256.Sum256(request)
	return &CanvasRelayDelegation{
		ID: hex.EncodeToString(secretDigest[:]), ClientID: "canvas", IdempotencyKey: "request-0000000001",
		ClientTaskID: "canvas-task-0001", UserID: 42, PluginKey: "video", Model: "task-model",
		PayloadSHA256: hex.EncodeToString(payloadDigest[:]), MaxQuota: 100,
		ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Second), State: CanvasRelayAuthorized,
	}, &Token{UserId: 42, Key: "native-task-token", Status: 1, RemainQuota: 100, ModelLimitsEnabled: true, ModelLimits: "task-model"}, secret, request
}

func TestCanvasRelayDuplicateAndConflict(t *testing.T) {
	setupCanvasRelayDB(t)
	delegation, token, _, _ := canvasRelayFixture()
	created, err := CreateCanvasRelayDelegation(delegation, token)
	require.NoError(t, err)
	require.Equal(t, token.Id, created.TokenID)
	copyRequest := *delegation
	copyRequest.TokenID = 0
	duplicate, err := CreateCanvasRelayDelegation(&copyRequest, &Token{UserId: 42, Key: "unused-key"})
	require.NoError(t, err)
	require.Equal(t, created.TokenID, duplicate.TokenID)
	var count int64
	require.NoError(t, DB.Model(&Token{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	copyRequest.MaxQuota++
	_, err = CreateCanvasRelayDelegation(&copyRequest, &Token{UserId: 42, Key: "unused-key-2"})
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	copyRequest = *delegation
	copyRequest.IdempotencyKey = "different-key-0001"
	_, err = CreateCanvasRelayDelegation(&copyRequest, &Token{UserId: 42, Key: "unused-key-3"})
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	require.NoError(t, DB.Model(&Token{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestCanvasRelaySingleClaimRestartAndUnknown(t *testing.T) {
	path := setupCanvasRelayDB(t)
	delegation, token, secret, payload := canvasRelayFixture()
	_, err := CreateCanvasRelayDelegation(delegation, token)
	require.NoError(t, err)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 43, "video", secret, payload)
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "video", secret, []byte("changed"))
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "wrong-plugin", secret, payload)
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "video", secret, payload)
	require.NoError(t, err)
	allowed, err := CheckCanvasRelayTokenAccess(token.Id, 0, "/v1/tasks/video")
	require.NoError(t, err)
	require.False(t, allowed)
	allowed, err = CheckCanvasRelayTokenAccess(token.Id, token.Id, "/v1/tasks/video")
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = CheckCanvasRelayTokenAccess(token.Id, token.Id, "/v1/chat/completions")
	require.NoError(t, err)
	require.False(t, allowed)
	require.NoError(t, RecordCanvasUpstreamTask(token.Id, "upstream-001"))
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	stored, err := GetCanvasRelayDelegation("canvas", delegation.ID)
	require.NoError(t, err)
	require.Equal(t, CanvasRelayUnknown, stored.State)
	require.Equal(t, "upstream-001", stored.UpstreamTaskID)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "video", secret, payload)
	require.ErrorIs(t, err, ErrCanvasDelegationConflict)
	require.NoError(t, CanvasRelayRequestQuota(token.Id, 100))
	require.ErrorIs(t, CanvasRelayRequestQuota(token.Id, 101), ErrCanvasDelegationConflict)
	bounded, err := CanvasRelayBoundedQuota(token.Id, 120)
	require.NoError(t, err)
	require.Equal(t, 100, bounded)
}

func TestCanvasRelayAsyncFailureAndExpiry(t *testing.T) {
	setupCanvasRelayDB(t)
	delegation, token, secret, payload := canvasRelayFixture()
	_, err := CreateCanvasRelayDelegation(delegation, token)
	require.NoError(t, err)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "video", secret, payload)
	require.NoError(t, err)
	require.NoError(t, DB.Create(&Task{TaskID: "native-001", UserId: 42, Quota: 100, Status: TaskStatusInProgress}).Error)
	require.NoError(t, LinkCanvasRelayTask(token.Id, "native-001"))
	stored, err := GetCanvasRelayDelegation("canvas", delegation.ID)
	require.NoError(t, err)
	task, err := GetCanvasRelayTask(stored)
	require.NoError(t, err)
	require.EqualValues(t, TaskStatusInProgress, task.Status)
	require.NoError(t, DB.Model(&Task{}).Where("task_id = ?", "native-001").Updates(map[string]any{"status": TaskStatusFailure, "quota": 0}).Error)
	task, err = GetCanvasRelayTask(stored)
	require.NoError(t, err)
	require.EqualValues(t, TaskStatusFailure, task.Status)
	require.Zero(t, task.Quota)
	require.ErrorIs(t, LinkCanvasRelayTask(token.Id, "native-002"), ErrCanvasDelegationConflict)
	require.NoError(t, DB.Model(&CanvasRelayDelegation{}).Where("id = ?", delegation.ID).Update("expires_at", time.Now().Add(-time.Second)).Error)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, 42, "video", secret, payload)
	require.True(t, errors.Is(err, ErrCanvasDelegationConflict))
}

func TestCanvasRelayTaskInsertionIsAtomic(t *testing.T) {
	setupCanvasRelayDB(t)
	delegation, token, secret, payload := canvasRelayFixture()
	_, err := CreateCanvasRelayDelegation(delegation, token)
	require.NoError(t, err)
	task := &Task{TaskID: "native-atomic", UserId: delegation.UserID, Status: TaskStatusSubmitted}
	require.ErrorIs(t, InsertCanvasRelayTask(context.Background(), token.Id, task), ErrCanvasDelegationConflict)
	var count int64
	require.NoError(t, DB.Model(&Task{}).Where("task_id = ?", task.TaskID).Count(&count).Error)
	require.Zero(t, count)
	_, err = ClaimCanvasRelayDelegation("canvas", delegation.ID, delegation.UserID, delegation.PluginKey, secret, payload)
	require.NoError(t, err)
	require.NoError(t, InsertCanvasRelayTask(context.Background(), token.Id, task))
	stored, err := GetCanvasRelayDelegation("canvas", delegation.ID)
	require.NoError(t, err)
	require.Equal(t, CanvasRelaySubmitted, stored.State)
	require.Equal(t, task.TaskID, stored.NativeTaskID)
	require.ErrorIs(t, InsertCanvasRelayTask(context.Background(), token.Id, &Task{TaskID: "duplicate-native", UserId: delegation.UserID}), ErrCanvasDelegationConflict)
	require.NoError(t, DB.Model(&Task{}).Where("task_id = ?", "duplicate-native").Count(&count).Error)
	require.Zero(t, count)
}

func TestCanvasRelayDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct {
		name string
		env  string
	}{
		{"mysql", "TEST_CANVAS_MYSQL_DSN"},
		{"postgres", "TEST_CANVAS_POSTGRES_DSN"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dsn == "" {
				t.Skip(dialect.env + " not configured")
			}
			var driver gorm.Dialector
			if dialect.name == "mysql" {
				driver = mysql.Open(dsn)
			} else {
				driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			previous := DB
			DB = db
			t.Cleanup(func() { DB = previous })
			for range 2 {
				require.NoError(t, db.AutoMigrate(&Token{}, &CanvasRelayDelegation{}))
			}
			delegation, token, secret, payload := canvasRelayFixture()
			created, err := CreateCanvasRelayDelegation(delegation, token)
			require.NoError(t, err)
			t.Cleanup(func() {
				db.Delete(created)
				db.Unscoped().Delete(token)
			})
			_, err = ClaimCanvasRelayDelegation("canvas", created.ID, 42, "video", secret, payload)
			require.NoError(t, err)
			_, err = ClaimCanvasRelayDelegation("canvas", created.ID, 42, "video", secret, payload)
			require.ErrorIs(t, err, ErrCanvasDelegationConflict)
			require.NoError(t, RecordCanvasUpstreamTask(token.Id, "provider-task"))
			require.NoError(t, LinkCanvasRelayTask(token.Id, "native-task"))
			stored, err := GetCanvasRelayDelegation("canvas", created.ID)
			require.NoError(t, err)
			require.Equal(t, CanvasRelaySubmitted, stored.State)
			require.Equal(t, "provider-task", stored.UpstreamTaskID)
			require.Equal(t, "native-task", stored.NativeTaskID)
		})
	}
}
