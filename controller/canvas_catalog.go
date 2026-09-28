package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

type canvasCatalogResponse struct {
	Version             string                  `json:"version"`
	GeneratedAt         time.Time               `json:"generatedAt"`
	ExpiresAt           time.Time               `json:"expiresAt"`
	RefreshAfterSeconds int                     `json:"refreshAfterSeconds"`
	Offerings           []canvasCatalogOffering `json:"offerings"`
}

type canvasCatalogOffering struct {
	OfferingID    string                   `json:"offeringId"`
	DisplayName   string                   `json:"displayName"`
	Provider      string                   `json:"provider"`
	Group         string                   `json:"group"`
	Model         string                   `json:"model"`
	Capabilities  []string                 `json:"capabilities"`
	Protocol      string                   `json:"protocol"`
	Enabled       bool                     `json:"enabled"`
	Visible       bool                     `json:"visible"`
	ConfigVersion string                   `json:"configVersion"`
	Status        string                   `json:"status"`
	Health        canvasCatalogHealth      `json:"health"`
	Billing       canvasCatalogBilling     `json:"billing"`
	InputSchema   canvasCatalogInputSchema `json:"inputSchema"`
}

type canvasCatalogHealth struct {
	Status         string                     `json:"status"`
	CheckedAt      *time.Time                 `json:"checkedAt,omitempty"`
	LatencyMs      int                        `json:"latencyMs,omitempty"`
	EndpointPingMs int                        `json:"endpointPingMs,omitempty"`
	Availability7d *float64                   `json:"availability7d,omitempty"`
	RecentChecks   []canvasCatalogHealthCheck `json:"recentChecks"`
}

type canvasCatalogHealthCheck struct {
	CheckedAt time.Time `json:"checkedAt"`
	Status    string    `json:"status"`
	LatencyMs int       `json:"latencyMs,omitempty"`
	ErrorCode string    `json:"errorCode,omitempty"`
}

type canvasCatalogBilling struct {
	Currency    string  `json:"currency"`
	Mode        string  `json:"mode"`
	InputPrice  float64 `json:"inputPrice"`
	OutputPrice float64 `json:"outputPrice"`
	MaxQuota    int     `json:"maxQuota"`
}

type canvasCatalogInputSchema struct {
	Version                string `json:"version"`
	SupportsPrompt         bool   `json:"supportsPrompt"`
	SupportsReferenceImage bool   `json:"supportsReferenceImage"`
	SupportsReferenceVideo bool   `json:"supportsReferenceVideo"`
	SupportsAspectRatio    bool   `json:"supportsAspectRatio"`
	SupportsSize           bool   `json:"supportsSize"`
	SupportsBatch          bool   `json:"supportsBatch"`
}

func CanvasCatalog(c *gin.Context) {
	clientID, ok := canvasRelayClient(c)
	if !ok {
		return
	}
	userID, ok := canvasCatalogUser(c)
	if !ok {
		return
	}
	capability := strings.TrimSpace(c.Query("capability"))
	status := strings.TrimSpace(c.Query("status"))
	if status == "" {
		status = "all"
	}
	if status != "all" && status != "healthy" && status != "degraded" && status != "unhealthy" && status != "unknown" && status != "disabled" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	offerings, err := buildCanvasCatalog(clientID, userID, capability, status)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	now := time.Now().UTC()
	versionInput, err := json.Marshal(offerings)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	versionBytes := sha256.Sum256(append([]byte(clientID+"\x00"), versionInput...))
	version := "catalog-" + hex.EncodeToString(versionBytes[:8])
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": canvasCatalogResponse{
		Version: version, GeneratedAt: now, ExpiresAt: now.Add(time.Minute), RefreshAfterSeconds: 60, Offerings: offerings,
	}, "msg": "", "reason": ""})
}

func CanvasCatalogOffering(c *gin.Context) {
	clientID, ok := canvasRelayClient(c)
	if !ok {
		return
	}
	userID, ok := canvasCatalogUser(c)
	if !ok {
		return
	}
	offerings, err := buildCanvasCatalog(clientID, userID, "", "all")
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	for _, offering := range offerings {
		if offering.OfferingID == c.Param("offeringId") {
			c.JSON(http.StatusOK, gin.H{"code": 0, "data": offering, "msg": "", "reason": ""})
			return
		}
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func CanvasCatalogRefresh(c *gin.Context) {
	if _, ok := canvasRelayClient(c); !ok {
		return
	}
	if _, ok := canvasCatalogUser(c); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"state": "refreshed"}, "msg": "", "reason": ""})
}

func canvasCatalogUser(c *gin.Context) (int, bool) {
	userID, err := strconv.Atoi(strings.TrimSpace(c.GetHeader("X-Canvas-User-ID")))
	if err != nil || !canvasRelayUser(c, userID) {
		return 0, false
	}
	return userID, true
}

func buildCanvasCatalog(clientID string, userID int, capability, requestedStatus string) ([]canvasCatalogOffering, error) {
	abilities, err := model.GetAllEnableAbilityWithChannels()
	if err != nil {
		return nil, err
	}
	userGroup, err := model.GetUserGroup(userID, true)
	if err != nil {
		return nil, err
	}
	usableGroups := service.GetUserUsableGroups(userGroup)
	groupFilter := strings.TrimSpace(os.Getenv("NEWAPI_CANVAS_GROUP"))
	maxQuota, _ := strconv.Atoi(os.Getenv("NEWAPI_CANVAS_MAX_QUOTA"))
	offerings := make([]canvasCatalogOffering, 0, len(abilities))
	for _, ability := range abilities {
		if groupFilter != "" && ability.Group != groupFilter {
			continue
		}
		if _, visible := usableGroups[ability.Group]; !visible {
			continue
		}
		endpoints := common.GetEndpointTypesByChannelType(ability.ChannelType, ability.Model)
		if !containsEndpoint(endpoints, constant.EndpointTypeImageGeneration) {
			continue
		}
		channel, err := model.GetChannelById(ability.ChannelId, false)
		if err != nil {
			continue
		}
		health, healthErr := canvasHealthForChannel(channel)
		if healthErr != nil {
			return nil, healthErr
		}
		if requestedStatus != "all" && health.Status != requestedStatus {
			continue
		}
		offering := canvasOfferingForAbility(clientID, ability, channel, health, maxQuota)
		if capability != "" && !containsString(offering.Capabilities, capability) {
			continue
		}
		offerings = append(offerings, offering)
	}
	return offerings, nil
}

func canvasOfferingChannel(clientID string, userID int, offeringID, modelName string) (int, string, bool, error) {
	offerings, err := buildCanvasCatalog(clientID, userID, "image", "all")
	if err != nil {
		return 0, "", false, err
	}
	for _, offering := range offerings {
		if offering.OfferingID == offeringID && offering.Model == modelName && offering.Enabled && offering.Visible && offering.Status != "disabled" {
			abilities, abilityErr := model.GetAllEnableAbilityWithChannels()
			if abilityErr != nil {
				return 0, "", false, abilityErr
			}
			for _, ability := range abilities {
				if canvasCatalogID(clientID, ability.ChannelId, ability.Group, ability.Model) == offeringID {
					return ability.ChannelId, ability.Group, true, nil
				}
			}
			return 0, "", false, nil
		}
	}
	return 0, "", false, nil
}

func canvasOfferingForAbility(clientID string, ability model.AbilityWithChannel, channel *model.Channel, health canvasCatalogHealth, maxQuota int) canvasCatalogOffering {
	offeringID := canvasCatalogID(clientID, ability.ChannelId, ability.Group, ability.Model)
	configHash := sha256.Sum256([]byte(strconv.Itoa(ability.ChannelId) + "\x00" + ability.Group + "\x00" + ability.Model + "\x00" + channel.GetModelMapping() + "\x00" + strconv.Itoa(channel.Status)))
	provider := constant.ChannelTypeNames[channel.Type]
	if provider == "" {
		provider = "channel"
	}
	return canvasCatalogOffering{
		OfferingID: offeringID, DisplayName: ability.Model, Provider: provider, Group: ability.Group, Model: ability.Model,
		Capabilities: []string{"image"}, Protocol: "openai-images", Enabled: channel.Status == common.ChannelStatusEnabled, Visible: channel.Status == common.ChannelStatusEnabled,
		ConfigVersion: hex.EncodeToString(configHash[:8]), Status: health.Status, Health: health,
		Billing:     canvasCatalogBilling{Currency: "new-api-quota", Mode: "ratio", OutputPrice: imagePrice(ability.Model), MaxQuota: maxQuota},
		InputSchema: canvasCatalogInputSchema{Version: "1", SupportsPrompt: true, SupportsReferenceImage: channel.Type == constant.ChannelTypeOpenAI, SupportsAspectRatio: false, SupportsSize: true, SupportsBatch: false},
	}
}

func canvasHealthForChannel(channel *model.Channel) (health canvasCatalogHealth, err error) {
	health.RecentChecks = []canvasCatalogHealthCheck{}
	if channel.Status != common.ChannelStatusEnabled {
		health.Status = "disabled"
		return health, nil
	}
	checks, err := model.RecentCanvasChannelHealth(channel.Id, time.Now().UTC().Add(-7*24*time.Hour), 20)
	if err != nil {
		return health, err
	}
	if len(checks) == 0 {
		health.Status = "unknown"
		return health, nil
	}
	checkedAt := checks[0].CheckedAt
	health.CheckedAt = &checkedAt
	latest := checks[0]
	health.LatencyMs = latest.LatencyMs
	health.EndpointPingMs = latest.LatencyMs
	health.Status = latest.Status
	for _, check := range checks {
		health.RecentChecks = append(health.RecentChecks, canvasCatalogHealthCheck{CheckedAt: check.CheckedAt, Status: check.Status, LatencyMs: check.LatencyMs, ErrorCode: check.ErrorCode})
	}
	availability, availabilityErr := model.CanvasChannelAvailability(channel.Id, time.Now().UTC().Add(-7*24*time.Hour))
	if availabilityErr != nil {
		return health, availabilityErr
	}
	health.Availability7d = availability
	if time.Since(checkedAt) > 7*24*time.Hour {
		health.Status = "unknown"
		return health, nil
	}
	return health, nil
}

func imagePrice(modelName string) float64 {
	price, ok := ratio_setting.GetImageRatio(modelName)
	if !ok {
		return 0
	}
	return price
}

func canvasCatalogID(clientID string, channelID int, group, modelName string) string {
	digest := sha256.Sum256([]byte(clientID + "\x00" + strconv.Itoa(channelID) + "\x00" + group + "\x00" + modelName))
	return "canvas-offering-" + hex.EncodeToString(digest[:16])
}

func containsEndpoint(endpoints []constant.EndpointType, wanted constant.EndpointType) bool {
	for _, endpoint := range endpoints {
		if endpoint == wanted {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
