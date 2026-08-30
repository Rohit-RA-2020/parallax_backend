package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMediaCookieRoundTripAndTampering(t *testing.T) {
	a := &Authenticator{mediaSecret: []byte("0123456789abcdef0123456789abcdef"), cookieSecure: true}
	w := httptest.NewRecorder()
	wanted := User{ID: uuid.New(), SessionID: "session-1"}
	expires := a.SetMediaCookie(w, wanted)
	if time.Until(expires) < 29*time.Minute {
		t.Fatalf("unexpected expiry: %v", expires)
	}
	response := w.Result()
	if len(response.Cookies()) != 1 {
		t.Fatal("media cookie was not issued")
	}
	cookie := response.Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/v1/media" {
		t.Fatalf("unsafe cookie attributes: %#v", cookie)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/media/version", nil)
	req.AddCookie(cookie)
	got, err := a.AuthenticateMedia(req)
	if err != nil || got.ID != wanted.ID || got.SessionID != wanted.SessionID {
		t.Fatalf("round trip got=%+v err=%v", got, err)
	}
	cookie.Value += "tampered"
	tampered := httptest.NewRequest(http.MethodGet, "/v1/media/version", nil)
	tampered.AddCookie(cookie)
	if _, err := a.AuthenticateMedia(tampered); err == nil {
		t.Fatal("tampered cookie was accepted")
	}
}

func TestClearMediaCookie(t *testing.T) {
	a := &Authenticator{mediaSecret: []byte("0123456789abcdef0123456789abcdef")}
	w := httptest.NewRecorder()
	a.ClearMediaCookie(w)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Path != "/v1/media" {
		t.Fatalf("cookie was not cleared: %#v", cookies)
	}
}
