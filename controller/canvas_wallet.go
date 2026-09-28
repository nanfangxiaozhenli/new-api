package controller

import (
	"crypto/hmac"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func CanvasWallet(c *gin.Context) {
	secret := os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET")
	if !canvasClientConfigured() || !hmac.Equal([]byte(c.GetHeader("X-Canvas-Client-Secret")), []byte(secret)) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	userID, err := strconv.Atoi(strings.TrimSpace(c.Param("subject")))
	if err != nil || userID <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	user, err := model.GetSelfUserById(userID)
	if err != nil || user.Status != common.UserStatusEnabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	quota, err := model.GetUserQuota(userID, true)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	used, err := model.GetUserUsedQuota(userID)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"available_quota": quota, "used_quota": used, "currency": "new-api-quota"}, "msg": "", "reason": ""})
}
