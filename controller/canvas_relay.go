package controller

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func canvasRelayClient(c *gin.Context) (string, bool) {
	clientID := os.Getenv("NEWAPI_CANVAS_CLIENT_ID")
	secret := os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET")
	if !canvasClientConfigured() || !hmac.Equal([]byte(c.GetHeader("X-Canvas-Client-Secret")), []byte(secret)) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return "", false
	}
	c.Header("Cache-Control", "no-store")
	return clientID, true
}

func canvasRelayUser(c *gin.Context, id int) bool {
	if id <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return false
	}
	user, err := model.GetSelfUserById(id)
	if err != nil || user.Status != common.UserStatusEnabled {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	return true
}

func CanvasRelayAuthorize(c *gin.Context) {
	clientID, ok := canvasRelayClient(c)
	if !ok {
		return
	}
	maxQuota, maxErr := strconv.Atoi(os.Getenv("NEWAPI_CANVAS_MAX_QUOTA"))
	if maxErr != nil || maxQuota <= 0 {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	var request struct {
		UserID        int       `json:"user_id"`
		TaskID        string    `json:"task_id"`
		PluginKey     string    `json:"plugin_key"`
		Capability    string    `json:"capability"`
		OfferingID    string    `json:"offering_id"`
		Model         string    `json:"model"`
		PayloadSHA256 string    `json:"payload_sha256"`
		MaxQuota      int       `json:"max_quota"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if c.ShouldBindJSON(&request) != nil || len(c.GetHeader("Idempotency-Key")) < 16 || len(c.GetHeader("Idempotency-Key")) > 128 ||
		len(request.TaskID) < 8 || len(request.TaskID) > 128 || len(request.PluginKey) == 0 || len(request.PluginKey) > 30 ||
		(request.Capability != "" && request.Capability != "image" && request.Capability != "task") ||
		len(request.Model) == 0 || len(request.Model) > 191 || len(request.PayloadSHA256) != 64 ||
		request.MaxQuota <= 0 || request.MaxQuota > maxQuota ||
		strings.ContainsAny(request.PluginKey, "/\\ ?#") {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if _, err := hex.DecodeString(request.PayloadSHA256); err != nil || request.PayloadSHA256 != strings.ToLower(request.PayloadSHA256) || !canvasRelayUser(c, request.UserID) {
		if err != nil || request.PayloadSHA256 != strings.ToLower(request.PayloadSHA256) {
			c.AbortWithStatus(http.StatusBadRequest)
		}
		return
	}
	if request.Capability == "" {
		request.Capability = "task"
	}
	channelID := 0
	channelGroup := ""
	if request.Capability == "image" {
		if request.OfferingID == "" || (request.PluginKey != "image" && request.PluginKey != "image-edit") {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		resolvedChannelID, resolvedGroup, found, resolveErr := canvasOfferingChannel(clientID, request.UserID, request.OfferingID, request.Model)
		if resolveErr != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !found || resolvedChannelID <= 0 {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		channelID = resolvedChannelID
		channelGroup = resolvedGroup
		if request.PluginKey == "image-edit" {
			channel, channelErr := model.GetChannelById(channelID, false)
			if channelErr != nil || channel.Type != constant.ChannelTypeOpenAI {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
	} else if request.OfferingID != "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	request.ExpiresAt = request.ExpiresAt.UTC().Truncate(time.Second)
	requestBytes, err := common.Marshal(request)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	mac := hmac.New(sha256.New, []byte(os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET")))
	mac.Write([]byte("canvas-relay-v1\x00" + clientID + "\x00" + c.GetHeader("Idempotency-Key") + "\x00"))
	mac.Write(requestBytes)
	secret := hex.EncodeToString(mac.Sum(nil))
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, tokenBytes); err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256([]byte(secret))
	delegation := &model.CanvasRelayDelegation{
		ID: hex.EncodeToString(digest[:]), ClientID: clientID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		ClientTaskID: request.TaskID, UserID: request.UserID, PluginKey: request.PluginKey,
		Capability: request.Capability, OfferingID: request.OfferingID, ChannelID: channelID,
		Model: request.Model, PayloadSHA256: request.PayloadSHA256, MaxQuota: request.MaxQuota,
		ExpiresAt: request.ExpiresAt, State: model.CanvasRelayAuthorized,
	}
	if stored, lookupErr := model.GetCanvasRelayDelegationByKey(clientID, delegation.IdempotencyKey); lookupErr == nil {
		if !stored.SameRequest(delegation) || stored.ID != delegation.ID {
			c.AbortWithStatus(http.StatusConflict)
			return
		}
		response := gin.H{"id": stored.ID, "state": stored.State, "expires_at": stored.ExpiresAt}
		if stored.State == model.CanvasRelayAuthorized && time.Now().Before(stored.ExpiresAt) {
			response["delegation_secret"] = secret
		}
		c.JSON(http.StatusOK, response)
		return
	} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if request.ExpiresAt.Before(time.Now().Add(5*time.Second)) || request.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	token := &model.Token{
		UserId: request.UserID, Key: hex.EncodeToString(tokenBytes), Status: common.TokenStatusEnabled,
		Name: "canvas-relay", CreatedTime: time.Now().Unix(), ExpiredTime: request.ExpiresAt.Unix(),
		RemainQuota: request.MaxQuota, ModelLimitsEnabled: true, ModelLimits: request.Model, Group: channelGroup,
		CanvasRelayOnly: true,
	}
	stored, err := model.CreateCanvasRelayDelegation(delegation, token)
	if err != nil {
		if errors.Is(err, model.ErrCanvasDelegationConflict) {
			c.AbortWithStatus(http.StatusConflict)
		} else {
			c.AbortWithStatus(http.StatusInternalServerError)
		}
		return
	}
	if stored.ID != delegation.ID {
		c.AbortWithStatus(http.StatusConflict)
		return
	}
	status := http.StatusOK
	if stored.TokenID == token.Id {
		status = http.StatusCreated
	}
	if stored.State != model.CanvasRelayAuthorized || !time.Now().Before(stored.ExpiresAt) {
		c.JSON(status, gin.H{"id": stored.ID, "state": stored.State, "expires_at": stored.ExpiresAt})
		return
	}
	c.JSON(status, gin.H{"id": stored.ID, "state": stored.State, "delegation_secret": secret, "expires_at": stored.ExpiresAt})
}

func CanvasRelayStatus(c *gin.Context) {
	clientID, ok := canvasRelayClient(c)
	if !ok {
		return
	}
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	delegation, err := model.GetCanvasRelayDelegation(clientID, c.Param("id"))
	if err != nil || delegation.UserID != userID {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	result := gin.H{"id": delegation.ID, "state": delegation.State, "task_id": delegation.ClientTaskID, "native_task_id": delegation.NativeTaskID, "upstream_task_id": delegation.UpstreamTaskID}
	if delegation.NativeTaskID != "" {
		task, err := model.GetCanvasRelayTask(delegation)
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		result["native_status"] = task.Status
		result["native_quota"] = task.Quota
		if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			result["state"] = strings.ToLower(string(task.Status))
		}
	}
	c.JSON(http.StatusOK, result)
}

// CanvasRelayClaim consumes a delegation before any channel or upstream work.
func CanvasRelayClaim(c *gin.Context) {
	clientID, ok := canvasRelayClient(c)
	if !ok {
		return
	}
	userID, err := strconv.Atoi(c.GetHeader("X-Canvas-User-ID"))
	if err != nil || !canvasRelayUser(c, userID) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<20)
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil || len(payload) == 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	configured, err := model.GetCanvasRelayDelegation(clientID, c.Param("id"))
	if err != nil || configured.UserID != userID {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	isImageRoute := strings.Contains(c.FullPath(), "/image/")
	if (configured.Capability == "image") != isImageRoute {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	if isImageRoute {
		channelID, group, found, resolveErr := canvasOfferingChannel(clientID, userID, configured.OfferingID, configured.Model)
		if resolveErr != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !found || channelID != configured.ChannelID || group == "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}
	delegation, err := model.ClaimCanvasRelayDelegation(clientID, c.Param("id"), userID, c.Param("key"), c.GetHeader("X-Canvas-Delegation"), payload)
	if err != nil || delegation.PluginKey != c.Param("key") {
		c.AbortWithStatus(http.StatusConflict)
		return
	}
	token, err := model.CanvasRelayToken(delegation)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.Set("canvas_relay_token_id", token.Id)
	c.Set("canvas_relay_model", delegation.Model)
	if delegation.ChannelID > 0 {
		service.GetChannelConstraints(c).AddPin(taskdto.ChannelPin{
			ChannelId: delegation.ChannelID, Source: taskdto.PinSourceToken, Rank: taskdto.PinRankToken, RetryMode: taskdto.PinRetrySingleAttempt,
		})
	}
	if isImageRoute {
		c.Set("canvas_relay_endpoint", "image")
		if delegation.PluginKey == "image-edit" {
			c.Request.URL.Path = "/v1/images/edits"
		} else {
			c.Request.URL.Path = "/v1/images/generations"
		}
	} else {
		c.Request.URL.Path = "/v1/tasks/" + delegation.PluginKey
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(payload))
	c.Request.ContentLength = int64(len(payload))
	c.Request.Header.Set("Authorization", "Bearer "+token.Key)
	c.Next()
	if !c.GetBool("canvas_relay_attempted") {
		_ = model.RejectCanvasRelayDelegation(token.Id)
	} else if c.GetString("canvas_relay_endpoint") == "image" {
		state := model.CanvasRelayFailure
		if c.Writer.Status() >= http.StatusOK && c.Writer.Status() < http.StatusMultipleChoices {
			state = model.CanvasRelaySuccess
		}
		_ = model.CompleteCanvasRelay(token.Id, state)
	}
}

func CanvasRelayModel(c *gin.Context) {
	resolvedModel := c.GetString("resolved_task_model")
	if c.GetString("canvas_relay_endpoint") == "image" {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(c.Request.URL.Path, "/edits") {
			mediaType, params, parseErr := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
			if parseErr != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
			for {
				part, nextErr := reader.NextPart()
				if errors.Is(nextErr, io.EOF) {
					break
				}
				if nextErr != nil {
					c.AbortWithStatus(http.StatusBadRequest)
					return
				}
				if part.FormName() == "model" {
					modelBytes, readErr := io.ReadAll(io.LimitReader(part, 256))
					if readErr != nil {
						c.AbortWithStatus(http.StatusBadRequest)
						return
					}
					resolvedModel = strings.TrimSpace(string(modelBytes))
				}
			}
		} else {
			var request struct {
				Model string `json:"model"`
			}
			if common.Unmarshal(body, &request) != nil {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			resolvedModel = request.Model
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
	}
	if resolvedModel != c.GetString("canvas_relay_model") {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	c.Next()
}
