package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplicationWiresGuardedMediaImportBehindAdminAuthentication(t *testing.T) {
	app := newLifecycleApplication(t)
	call := func(path, body, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		app.server.Handler.ServeHTTP(response, request)
		return response
	}
	response := call("/api/admin/v1/media/inputs/import", `{"url":"http://127.0.0.1/private"}`, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated import=%d", response.Code)
	}
	login := call("/api/admin/v1/auth/login", `{"username":"test-admin","password":"fixture-admin-password"}`, "")
	var session struct {
		Data struct {
			Tokens struct {
				AccessToken string `json:"accessToken"`
			} `json:"tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil || login.Code != http.StatusOK || session.Data.Tokens.AccessToken == "" {
		t.Fatalf("fixture admin login failed: status=%d err=%v", login.Code, err)
	}
	response = call("/api/admin/v1/media/inputs/import", `{"url":"http://127.0.0.1/private"}`, session.Data.Tokens.AccessToken)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"imageURLBlocked"`)) {
		t.Fatalf("production importer not wired or guard bypassed: %d %s", response.Code, response.Body.String())
	}
}
