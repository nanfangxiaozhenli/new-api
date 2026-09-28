package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type canvasAssertion struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	Nonce     string `json:"nonce"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
}

func canvasClientConfigured() bool {
	return len(os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET")) >= 32 && os.Getenv("NEWAPI_CANVAS_CLIENT_ID") != "" && os.Getenv("NEWAPI_CANVAS_REDIRECT_URI") != "" && os.Getenv("NEWAPI_CANVAS_ISSUER") != ""
}

func validCanvasRedirect(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

// CanvasAuthorizationStart establishes a same-origin navigation before the strict refresh cookie is read.
func CanvasAuthorizationStart(c *gin.Context) {
	if !canvasClientConfigured() || !validCanvasRedirect(os.Getenv("NEWAPI_CANVAS_REDIRECT_URI")) {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	query := c.Request.URL.RawQuery
	target, _ := json.Marshal("/api/user/auth/canvas/authorize?" + query)
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; base-uri 'none'")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<!doctype html><script>location.replace("+string(target)+")</script>"))
}

func CanvasAuthorize(c *gin.Context) {
	if !canvasClientConfigured() || !validCanvasRedirect(os.Getenv("NEWAPI_CANVAS_REDIRECT_URI")) {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	clientID, redirectURI := c.Query("client_id"), c.Query("redirect_uri")
	state, nonce := c.Query("state"), c.Query("nonce")
	if clientID != os.Getenv("NEWAPI_CANVAS_CLIENT_ID") || redirectURI != os.Getenv("NEWAPI_CANVAS_REDIRECT_URI") || len(state) < 32 || len(state) > 128 || len(nonce) < 32 || len(nonce) > 128 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	refresh, err := c.Cookie(service.RefreshCookieName)
	if err != nil {
		c.Redirect(http.StatusFound, "/sign-in?redirect="+url.QueryEscape("/api/user/auth/canvas/start?"+c.Request.URL.RawQuery))
		return
	}
	bundle, user, err := service.RefreshLoginSession(refresh, "", c.ClientIP(), c.Request.UserAgent())
	if err != nil || user == nil || user.Status != common.UserStatusEnabled {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	service.WriteRefreshCookie(c, bundle.RefreshToken)
	codeBytes := make([]byte, 32)
	if _, err := rand.Read(codeBytes); err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(codeBytes)
	if err := model.CreateCanvasAuthorization(code, &model.CanvasAuthorization{UserID: user.Id, SessionID: bundle.Session.SID, ClientID: clientID, RedirectURI: redirectURI, Nonce: nonce, ExpiresAt: time.Now().Add(90 * time.Second)}); err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	redirect, _ := url.Parse(redirectURI)
	params := redirect.Query()
	params.Set("code", code)
	params.Set("state", state)
	redirect.RawQuery = params.Encode()
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, redirect.String())
}

func CanvasExchange(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !canvasClientConfigured() {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	var request struct {
		Code         string `json:"code"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RedirectURI  string `json:"redirect_uri"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if c.ShouldBindJSON(&request) != nil || request.ClientID != os.Getenv("NEWAPI_CANVAS_CLIENT_ID") || request.RedirectURI != os.Getenv("NEWAPI_CANVAS_REDIRECT_URI") || !hmac.Equal([]byte(request.ClientSecret), []byte(os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET"))) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	authorization, err := model.ConsumeCanvasAuthorization(request.Code, request.ClientID, request.RedirectURI)
	if err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if _, err := service.ValidateSessionReference(authorization.UserID, authorization.SessionID); err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	user, err := model.GetSelfUserById(authorization.UserID)
	if err != nil || user.Status != common.UserStatusEnabled {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	now := time.Now()
	claims := canvasAssertion{Issuer: strings.TrimRight(os.Getenv("NEWAPI_CANVAS_ISSUER"), "/"), Audience: request.ClientID, Subject: strconv.Itoa(user.Id), Nonce: authorization.Nonce, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Email: user.Email, Name: user.DisplayName}
	payload, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(os.Getenv("NEWAPI_CANVAS_CLIENT_SECRET")))
	_, _ = mac.Write([]byte(encoded))
	assertion := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"assertion": assertion}, "msg": "", "reason": ""})
}
