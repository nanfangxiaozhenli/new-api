package model

import (
	"time"
)

type CanvasChannelHealthCheck struct {
	ID        uint      `gorm:"primaryKey"`
	ChannelID int       `gorm:"not null;index:canvas_health_channel_checked"`
	CheckedAt time.Time `gorm:"not null;index:canvas_health_channel_checked"`
	Status    string    `gorm:"size:24;not null"`
	LatencyMs int       `gorm:"not null;default:0"`
	ErrorCode string    `gorm:"size:128"`
}

func RecordCanvasChannelHealth(channelID int, checkedAt time.Time, status string, latencyMs int, errorCode string) error {
	if channelID <= 0 || status == "" {
		return nil
	}
	if err := DB.Create(&CanvasChannelHealthCheck{
		ChannelID: channelID, CheckedAt: checkedAt.UTC(), Status: status, LatencyMs: max(0, latencyMs), ErrorCode: errorCode,
	}).Error; err != nil {
		return err
	}
	return DB.Where("channel_id = ? AND checked_at < ?", channelID, time.Now().UTC().Add(-30*24*time.Hour)).Delete(&CanvasChannelHealthCheck{}).Error
}

func RecentCanvasChannelHealth(channelID int, since time.Time, limit int) ([]CanvasChannelHealthCheck, error) {
	if limit <= 0 {
		limit = 20
	}
	var checks []CanvasChannelHealthCheck
	err := DB.Where("channel_id = ? AND checked_at >= ?", channelID, since.UTC()).Order("checked_at DESC").Limit(limit).Find(&checks).Error
	return checks, err
}

func CanvasChannelAvailability(channelID int, since time.Time) (*float64, error) {
	var total int64
	if err := DB.Model(&CanvasChannelHealthCheck{}).Where("channel_id = ? AND checked_at >= ?", channelID, since.UTC()).Count(&total).Error; err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	var available int64
	if err := DB.Model(&CanvasChannelHealthCheck{}).Where("channel_id = ? AND checked_at >= ? AND status IN ?", channelID, since.UTC(), []string{"healthy", "degraded"}).Count(&available).Error; err != nil {
		return nil, err
	}
	value := float64(available) / float64(total)
	return &value, nil
}
