package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
)

const (
	CanvasRelayAuthorized = "authorized"
	CanvasRelayUnknown    = "unknown"
	CanvasRelaySubmitted  = "submitted"
	CanvasRelayRejected   = "rejected"
	CanvasRelaySuccess    = "success"
	CanvasRelayFailure    = "failure"
)

type CanvasRelayDelegation struct {
	ID             string    `gorm:"primaryKey;size:64"`
	ClientID       string    `gorm:"size:128;not null;uniqueIndex:canvas_client_task;uniqueIndex:canvas_client_key"`
	IdempotencyKey string    `gorm:"size:128;not null;uniqueIndex:canvas_client_key"`
	ClientTaskID   string    `gorm:"size:128;not null;uniqueIndex:canvas_client_task"`
	UserID         int       `gorm:"not null;index"`
	TokenID        int       `gorm:"not null;uniqueIndex"`
	PluginKey      string    `gorm:"size:64;not null"`
	Capability     string    `gorm:"size:32;not null;default:'task'"`
	OfferingID     string    `gorm:"size:128;index"`
	ChannelID      int       `gorm:"index"`
	Model          string    `gorm:"size:191;not null"`
	PayloadSHA256  string    `gorm:"size:64;not null"`
	MaxQuota       int       `gorm:"not null"`
	ExpiresAt      time.Time `gorm:"not null"`
	State          string    `gorm:"size:24;not null"`
	NativeTaskID   string    `gorm:"size:128;index"`
	UpstreamTaskID string    `gorm:"size:191"`
	CreatedAt      time.Time
}

var ErrCanvasDelegationConflict = errors.New("canvas delegation conflicts with existing task or key")

func (delegation *CanvasRelayDelegation) SameRequest(other *CanvasRelayDelegation) bool {
	return delegation.ClientID == other.ClientID && delegation.IdempotencyKey == other.IdempotencyKey &&
		delegation.ClientTaskID == other.ClientTaskID && delegation.UserID == other.UserID &&
		delegation.PluginKey == other.PluginKey && delegation.Model == other.Model &&
		delegation.Capability == other.Capability && delegation.OfferingID == other.OfferingID &&
		delegation.ChannelID == other.ChannelID &&
		delegation.PayloadSHA256 == other.PayloadSHA256 && delegation.MaxQuota == other.MaxQuota &&
		delegation.ExpiresAt.Equal(other.ExpiresAt)
}

func CreateCanvasRelayDelegation(delegation *CanvasRelayDelegation, token *Token) (*CanvasRelayDelegation, error) {
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(token).Error; err != nil {
			return err
		}
		delegation.TokenID = token.Id
		return tx.Create(delegation).Error
	})
	if err == nil {
		return delegation, nil
	}
	var existing CanvasRelayDelegation
	lookupErr := DB.Where("client_id = ? AND (idempotency_key = ? OR client_task_id = ?)", delegation.ClientID, delegation.IdempotencyKey, delegation.ClientTaskID).First(&existing).Error
	if lookupErr != nil {
		return nil, err
	}
	if !existing.SameRequest(delegation) {
		return nil, ErrCanvasDelegationConflict
	}
	return &existing, nil
}

func GetCanvasRelayDelegation(clientID, id string) (*CanvasRelayDelegation, error) {
	var delegation CanvasRelayDelegation
	err := DB.Where("client_id = ? AND id = ?", clientID, id).First(&delegation).Error
	return &delegation, err
}

func GetCanvasRelayDelegationByKey(clientID, key string) (*CanvasRelayDelegation, error) {
	var delegation CanvasRelayDelegation
	err := DB.Where("client_id = ? AND idempotency_key = ?", clientID, key).First(&delegation).Error
	return &delegation, err
}

func RejectCanvasRelayDelegation(tokenID int) error {
	return DB.Model(&CanvasRelayDelegation{}).Where("token_id = ? AND state = ? AND native_task_id = ?", tokenID, CanvasRelayUnknown, "").Update("state", CanvasRelayRejected).Error
}

func RecordCanvasUpstreamTask(tokenID int, upstreamTaskID string) error {
	if upstreamTaskID == "" {
		return nil
	}
	return DB.Model(&CanvasRelayDelegation{}).Where("token_id = ? AND state = ?", tokenID, CanvasRelayUnknown).Update("upstream_task_id", upstreamTaskID).Error
}

func ClaimCanvasRelayDelegation(clientID, id string, userID int, pluginKey, secret string, payload []byte) (*CanvasRelayDelegation, error) {
	delegation, err := GetCanvasRelayDelegation(clientID, id)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	secretDigest := sha256.Sum256([]byte(secret))
	if delegation.UserID != userID || delegation.PluginKey != pluginKey || delegation.PayloadSHA256 != hex.EncodeToString(digest[:]) ||
		!hmac.Equal([]byte(delegation.ID), []byte(hex.EncodeToString(secretDigest[:]))) || time.Now().After(delegation.ExpiresAt) {
		return nil, ErrCanvasDelegationConflict
	}
	result := DB.Model(&CanvasRelayDelegation{}).Where("id = ? AND client_id = ? AND state = ? AND expires_at > ?", id, clientID, CanvasRelayAuthorized, time.Now()).Update("state", CanvasRelayUnknown)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrCanvasDelegationConflict
	}
	delegation.State = CanvasRelayUnknown
	return delegation, nil
}

func LinkCanvasRelayTask(tokenID int, taskID string) error {
	result := DB.Model(&CanvasRelayDelegation{}).Where("token_id = ? AND state = ? AND native_task_id = ?", tokenID, CanvasRelayUnknown, "").Updates(map[string]any{"native_task_id": taskID, "state": CanvasRelaySubmitted})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCanvasDelegationConflict
	}
	return nil
}

func CompleteCanvasRelay(tokenID int, state string) error {
	if state != CanvasRelaySuccess && state != CanvasRelayFailure {
		return ErrCanvasDelegationConflict
	}
	return DB.Model(&CanvasRelayDelegation{}).Where("token_id = ? AND state = ?", tokenID, CanvasRelayUnknown).Update("state", state).Error
}

func InsertCanvasRelayTask(ctx context.Context, tokenID int, task *Task) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(task).Error; err != nil {
			return err
		}
		result := tx.Model(&CanvasRelayDelegation{}).
			Where("token_id = ? AND user_id = ? AND state = ? AND native_task_id = ?", tokenID, task.UserId, CanvasRelayUnknown, "").
			Updates(map[string]any{"native_task_id": task.TaskID, "state": CanvasRelaySubmitted})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCanvasDelegationConflict
		}
		return nil
	})
}

func CanvasRelayQuotaLimit(tokenID int) (int, bool, error) {
	if tokenID <= 0 {
		return 0, false, nil
	}
	var delegation CanvasRelayDelegation
	err := DB.Select("max_quota").Where("token_id = ?", tokenID).First(&delegation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	return delegation.MaxQuota, err == nil, err
}

func GetCanvasRelayTask(delegation *CanvasRelayDelegation) (*Task, error) {
	if delegation.NativeTaskID == "" {
		return nil, nil
	}
	var task Task
	err := DB.Where("task_id = ? AND user_id = ?", delegation.NativeTaskID, delegation.UserID).First(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func CanvasRelayToken(delegation *CanvasRelayDelegation) (*Token, error) {
	var token Token
	err := DB.Where("id = ? AND user_id = ? AND unlimited_quota = ?", delegation.TokenID, delegation.UserID, false).First(&token).Error
	return &token, err
}

func CheckCanvasRelayTokenAccess(tokenID, claimedTokenID int, path string) (bool, error) {
	var delegation CanvasRelayDelegation
	err := DB.Select("token_id", "plugin_key", "capability", "state").Where("token_id = ?", tokenID).First(&delegation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	allowedPath := path == "/v1/tasks/"+delegation.PluginKey
	if delegation.Capability == "image" {
		allowedPath = path == "/v1/images/generations" || path == "/v1/images/edits"
	}
	return tokenID == claimedTokenID && delegation.State == CanvasRelayUnknown && allowedPath, nil
}

func CanvasRelayRequestQuota(tokenID int, quota int) error {
	limit, found, err := CanvasRelayQuotaLimit(tokenID)
	if err != nil {
		return err
	}
	if found && (quota < 0 || quota > limit) {
		return ErrCanvasDelegationConflict
	}
	return nil
}

func CanvasRelayBoundedQuota(tokenID, quota int) (int, error) {
	limit, found, err := CanvasRelayQuotaLimit(tokenID)
	if err != nil {
		return 0, err
	}
	if found {
		return min(quota, limit), nil
	}
	return quota, nil
}
