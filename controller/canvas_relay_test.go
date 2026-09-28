package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCanvasRelayHTTPAuthorizationAndReplay(t *testing.T) {
	previous := model.DB
	t.Cleanup(func() { model.DB = previous })
	var err error
	model.DB, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "relay.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.CanvasRelayDelegation{}, &model.Task{}))
	user := model.User{Username: "canvas-test-user", Status: common.UserStatusEnabled, Quota: 1000}
	require.NoError(t, model.DB.Create(&user).Error)
	t.Setenv("NEWAPI_CANVAS_CLIENT_ID", "canvas")
	t.Setenv("NEWAPI_CANVAS_CLIENT_SECRET", "canvas-relay-test-secret-longer-than-32-bytes")
	t.Setenv("NEWAPI_CANVAS_REDIRECT_URI", "https://example.test/callback")
	t.Setenv("NEWAPI_CANVAS_ISSUER", "https://issuer.example.test")
	t.Setenv("NEWAPI_CANVAS_MAX_QUOTA", "200")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/authorize", CanvasRelayAuthorize)
	router.GET("/status/:id", CanvasRelayStatus)
	router.POST("/invoke/:id/tasks/:key", CanvasRelayClaim, func(c *gin.Context) {
		c.Set("canvas_relay_attempted", true)
		c.Status(http.StatusAccepted)
	})
	payload := []byte(`{"model":"doubao-seedance-1-5-pro-251215","content":[{"type":"text","text":"a scene"}],"duration":5}`)
	digest := sha256.Sum256(payload)
	expiresAt := time.Now().Add(time.Minute).UTC()
	requestBody, err := common.Marshal(gin.H{
		"user_id": user.Id, "task_id": "canvas-task-1234", "plugin_key": "doubao", "model": "doubao-seedance-1-5-pro-251215",
		"payload_sha256": hex.EncodeToString(digest[:]), "max_quota": 100, "expires_at": expiresAt,
	})
	require.NoError(t, err)
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		call := httptest.NewRequest(method, path, bytes.NewReader(body))
		call.Header.Set("X-Canvas-Client-Secret", "canvas-relay-test-secret-longer-than-32-bytes")
		call.Header.Set("Idempotency-Key", "canvas-idempotency-0001")
		call.Header.Set("X-Canvas-User-ID", ""+strconv.Itoa(user.Id))
		call.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, call)
		return response
	}
	created := request(http.MethodPost, "/authorize", requestBody)
	require.Equal(t, http.StatusCreated, created.Code)
	var issued struct {
		ID     string `json:"id"`
		Secret string `json:"delegation_secret"`
	}
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &issued))
	require.NotEmpty(t, issued.Secret)
	response := request(http.MethodPost, "/authorize", requestBody)
	require.Equal(t, http.StatusOK, response.Code)
	var duplicate struct {
		ID     string `json:"id"`
		Secret string `json:"delegation_secret"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &duplicate))
	require.Equal(t, issued, duplicate)
	var tokens int64
	require.NoError(t, model.DB.Model(&model.Token{}).Count(&tokens).Error)
	require.EqualValues(t, 1, tokens)
	var internalToken model.Token
	require.NoError(t, model.DB.First(&internalToken).Error)
	require.True(t, internalToken.CanvasRelayOnly)
	require.False(t, internalToken.UnlimitedQuota)
	require.Equal(t, 100, internalToken.RemainQuota)
	keyRecorder := httptest.NewRecorder()
	keyContext, _ := gin.CreateTestContext(keyRecorder)
	keyContext.Request = httptest.NewRequest(http.MethodPost, "/token/"+strconv.Itoa(internalToken.Id)+"/key", nil)
	keyContext.Params = gin.Params{{Key: "id", Value: strconv.Itoa(internalToken.Id)}}
	keyContext.Set("id", user.Id)
	GetTokenKey(keyContext)
	require.Equal(t, http.StatusForbidden, keyRecorder.Code)
	require.NotContains(t, keyRecorder.Body.String(), internalToken.Key)
	batchRecorder := httptest.NewRecorder()
	batchContext, _ := gin.CreateTestContext(batchRecorder)
	batchBody, err := common.Marshal(gin.H{"ids": []int{internalToken.Id}})
	require.NoError(t, err)
	batchContext.Request = httptest.NewRequest(http.MethodPost, "/token/batch/keys", bytes.NewReader(batchBody))
	batchContext.Request.Header.Set("Content-Type", "application/json")
	batchContext.Set("id", user.Id)
	GetTokenKeysBatch(batchContext)
	require.Equal(t, http.StatusForbidden, batchRecorder.Code)
	require.NotContains(t, batchRecorder.Body.String(), internalToken.Key)
	wrongKey := httptest.NewRequest(http.MethodPost, "/invoke/"+issued.ID+"/tasks/other", bytes.NewReader(payload))
	wrongKey.Header.Set("X-Canvas-Client-Secret", "canvas-relay-test-secret-longer-than-32-bytes")
	wrongKey.Header.Set("X-Canvas-User-ID", strconv.Itoa(user.Id))
	wrongKey.Header.Set("X-Canvas-Delegation", issued.Secret)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, wrongKey)
	require.Equal(t, http.StatusConflict, response.Code)
	tampered := httptest.NewRequest(http.MethodPost, "/invoke/"+issued.ID+"/tasks/doubao", bytes.NewReader([]byte(`{"model":"doubao-seedance-1-5-pro-251215","content":[{"type":"text","text":"changed"}],"duration":5}`)))
	tampered.Header = wrongKey.Header.Clone()
	response = httptest.NewRecorder()
	router.ServeHTTP(response, tampered)
	require.Equal(t, http.StatusConflict, response.Code)
	invoke := httptest.NewRequest(http.MethodPost, "/invoke/"+issued.ID+"/tasks/doubao", bytes.NewReader(payload))
	invoke.Header = wrongKey.Header.Clone()
	response = httptest.NewRecorder()
	router.ServeHTTP(response, invoke)
	require.Equal(t, http.StatusAccepted, response.Code)
	replay := httptest.NewRequest(http.MethodPost, "/invoke/"+issued.ID+"/tasks/doubao", bytes.NewReader(payload))
	replay.Header = wrongKey.Header.Clone()
	response = httptest.NewRecorder()
	router.ServeHTTP(response, replay)
	require.Equal(t, http.StatusConflict, response.Code)
	status := request(http.MethodGet, "/status/"+issued.ID+"?user_id="+strconv.Itoa(user.Id), nil)
	require.Equal(t, http.StatusOK, status.Code)
	require.Contains(t, status.Body.String(), `"state":"unknown"`)
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, "/status/"+issued.ID+"?user_id=999", nil).Code)
	require.NoError(t, model.DB.Create(&model.Task{TaskID: "native-async", UserId: user.Id, Status: model.TaskStatusInProgress, Quota: 100}).Error)
	require.NoError(t, model.LinkCanvasRelayTask(internalToken.Id, "native-async"))
	status = request(http.MethodGet, "/status/"+issued.ID+"?user_id="+strconv.Itoa(user.Id), nil)
	require.Equal(t, http.StatusOK, status.Code)
	require.Contains(t, status.Body.String(), `"native_status":"IN_PROGRESS"`)
	require.NoError(t, model.DB.Model(&model.Task{}).Where("task_id = ?", "native-async").Update("status", model.TaskStatusSuccess).Error)
	status = request(http.MethodGet, "/status/"+issued.ID+"?user_id="+strconv.Itoa(user.Id), nil)
	require.Contains(t, status.Body.String(), `"state":"success"`)
}
