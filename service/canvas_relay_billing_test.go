package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCanvasRelayNativeLedgerChargesOnceAndRefundsFailure(t *testing.T) {
	require.NoError(t, model.DB.AutoMigrate(&model.CanvasRelayDelegation{}))
	user := model.User{Username: "canvas-ledger-" + common.GetRandomString(8), Quota: 200, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "canvas-ledger-token-" + common.GetRandomString(8), Name: "canvas-relay", RemainQuota: 100, Status: common.TokenStatusEnabled, ModelLimitsEnabled: true, ModelLimits: "task-model"}
	require.NoError(t, model.DB.Create(&token).Error)
	channel := model.Channel{Name: "canvas-ledger", Key: "unused", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&channel).Error)
	delegation := model.CanvasRelayDelegation{ID: "canvas-ledger-" + common.GetRandomString(16), ClientID: "canvas", IdempotencyKey: "ledger-" + common.GetRandomString(16), ClientTaskID: "task-" + common.GetRandomString(16), UserID: user.Id, TokenID: token.Id, PluginKey: "video", Model: "task-model", PayloadSHA256: "not-used", MaxQuota: 100, ExpiresAt: time.Now().Add(time.Minute), State: model.CanvasRelayUnknown}
	require.NoError(t, model.DB.Create(&delegation).Error)
	t.Cleanup(func() {
		model.DB.Where("user_id = ?", user.Id).Delete(&model.Log{})
		model.DB.Where("user_id = ?", user.Id).Delete(&model.Task{})
		model.DB.Delete(&delegation)
		model.DB.Unscoped().Delete(&token)
		model.DB.Delete(&channel)
		model.DB.Unscoped().Delete(&user)
	})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/tasks/video", nil)
	info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, ForcePreConsume: true, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}}
	apiErr := PreConsumeBilling(ctx, 101, info)
	require.NotNil(t, apiErr)
	quota, err := model.GetUserQuota(user.Id, true)
	require.NoError(t, err)
	require.Equal(t, 200, quota)

	apiErr = PreConsumeBilling(ctx, 60, info)
	require.Nil(t, apiErr)
	require.NoError(t, SettleBilling(ctx, info, 120))
	require.NoError(t, SettleBilling(ctx, info, 120))
	quota, err = model.GetUserQuota(user.Id, true)
	require.NoError(t, err)
	require.Equal(t, 100, quota)
	storedToken, err := model.GetTokenById(token.Id)
	require.NoError(t, err)
	require.Equal(t, 0, storedToken.RemainQuota)

	task := model.Task{TaskID: "native-" + common.GetRandomString(16), UserId: user.Id, ChannelId: channel.Id, Quota: 100, Status: model.TaskStatusFailure, PrivateData: model.TaskPrivateData{TokenId: token.Id}}
	require.NoError(t, model.DB.Create(&task).Error)
	require.True(t, RefundTaskQuota(context.Background(), &task, "provider failed"))
	require.True(t, RefundTaskQuota(context.Background(), &task, "duplicate notification"))
	quota, err = model.GetUserQuota(user.Id, true)
	require.NoError(t, err)
	require.Equal(t, 200, quota)
	storedToken, err = model.GetTokenById(token.Id)
	require.NoError(t, err)
	require.Equal(t, 100, storedToken.RemainQuota)
}
