package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
)

type CanvasAuthorization struct {
	ID          string    `gorm:"primaryKey;size:64"`
	UserID      int       `gorm:"index;not null"`
	SessionID   string    `gorm:"size:64;not null"`
	ClientID    string    `gorm:"size:128;not null"`
	RedirectURI string    `gorm:"size:1024;not null"`
	Nonce       string    `gorm:"size:128;not null"`
	ExpiresAt   time.Time `gorm:"index;not null"`
	ConsumedAt  *time.Time
}

func CreateCanvasAuthorization(code string, authorization *CanvasAuthorization) error {
	digest := sha256.Sum256([]byte(code))
	authorization.ID = hex.EncodeToString(digest[:])
	return DB.Create(authorization).Error
}

func ConsumeCanvasAuthorization(code, clientID, redirectURI string) (*CanvasAuthorization, error) {
	digest := sha256.Sum256([]byte(code))
	var authorization CanvasAuthorization
	err := DB.Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&CanvasAuthorization{}).Where("id = ? AND client_id = ? AND redirect_uri = ? AND consumed_at IS NULL AND expires_at > ?", hex.EncodeToString(digest[:]), clientID, redirectURI, time.Now())
		if err := query.First(&authorization).Error; err != nil {
			return err
		}
		now := time.Now()
		result := tx.Model(&CanvasAuthorization{}).Where("id = ? AND consumed_at IS NULL", authorization.ID).Update("consumed_at", &now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("authorization code already consumed")
		}
		return nil
	})
	return &authorization, err
}
