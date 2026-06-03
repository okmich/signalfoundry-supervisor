package notify

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // bad token
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	tg := NewTelegram("tok", "chat")
	tg.baseURL = srv.URL
	if err := tg.Send("hi"); err == nil {
		t.Error("a non-2xx response must surface as an error, not be swallowed")
	}
}

func TestSendOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	tg := NewTelegram("tok", "chat")
	tg.baseURL = srv.URL
	if err := tg.Send("hi"); err != nil {
		t.Errorf("a 200 should be success, got %v", err)
	}
}

func TestSendNoCredsIsNoop(t *testing.T) {
	if err := (&Telegram{}).Send("hi"); err != nil {
		t.Errorf("missing creds should no-op (alerting disabled), got %v", err)
	}
}
