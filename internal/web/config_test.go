package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"caddyui/internal/app"
	"caddyui/internal/caddy"
	"caddyui/internal/store"
)

func TestHandleSettingsACMEIgnoresPostedEmail(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "caddyui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const contactEmail = "owner@example.com"
	if err := db.SetSetting(app.SettingACMEEmail, contactEmail); err != nil {
		t.Fatal(err)
	}

	caddyAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/load" {
			t.Fatalf("unexpected Caddy request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(caddyAPI.Close)

	s := &Server{svc: &app.Service{
		Store: db,
		Caddy: caddy.New(strings.TrimPrefix(caddyAPI.URL, "http://")),
	}}
	form := url.Values{
		"acme_email": {"changed@example.com"},
		"acme_ca":    {"https://acme.example.com/directory"},
	}
	r := httptest.NewRequest(http.MethodPost, "/settings/acme", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	s.handleSettingsACME(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if got := db.Setting(app.SettingACMEEmail, ""); got != contactEmail {
		t.Fatalf("contact email changed to %q", got)
	}
	if got := db.Setting(app.SettingACMECA, ""); got != form.Get("acme_ca") {
		t.Fatalf("ACME CA = %q, want %q", got, form.Get("acme_ca"))
	}
}
