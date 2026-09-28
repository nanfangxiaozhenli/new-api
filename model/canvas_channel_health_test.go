package model

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCanvasChannelAvailabilityUsesAllChecksInSevenDays(t *testing.T) {
	previous := DB
	t.Cleanup(func() { DB = previous })
	var err error
	DB, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "health.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, DB.AutoMigrate(&CanvasChannelHealthCheck{}))
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	availability, err := CanvasChannelAvailability(10, cutoff)
	require.NoError(t, err)
	require.Nil(t, availability)
	for index := range 30 {
		status := "unhealthy"
		if index < 6 {
			status = "healthy"
		}
		require.NoError(t, DB.Create(&CanvasChannelHealthCheck{ChannelID: 10, CheckedAt: cutoff.Add(time.Hour), Status: status}).Error)
	}
	require.NoError(t, DB.Create(&CanvasChannelHealthCheck{ChannelID: 10, CheckedAt: cutoff.Add(-time.Hour), Status: "healthy"}).Error)
	require.NoError(t, DB.Create(&CanvasChannelHealthCheck{ChannelID: 11, CheckedAt: cutoff.Add(time.Hour), Status: "healthy"}).Error)
	availability, err = CanvasChannelAvailability(10, cutoff)
	require.NoError(t, err)
	require.InDelta(t, 0.2, *availability, 0.001)
}
