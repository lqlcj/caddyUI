package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDraftFromDockerFormAppliesEasySettings(t *testing.T) {
	compose := "services:\n  app:\n    image: demo:latest\n    ports:\n      - 8080:80\n    environment:\n      TZ: UTC\n"
	form := url.Values{
		"name":              {"demo"},
		"display_name":      {"Demo"},
		"compose":           {compose},
		"env":               {"PASSWORD=change-me\n"},
		"port__0__app":      {"127.0.0.1:18080"},
		"portorig__0__app":  {"8080"},
		"cenv__0__0":        {"Asia/Shanghai"},
		"cenvorig__0__0":    {"UTC"},
		"env__PASSWORD":     {"strong-password"},
		"envorig__PASSWORD": {"change-me"},
	}
	r := httptest.NewRequest("POST", "/docker/install", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	draft, err := draftFromDockerForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft.Compose, "127.0.0.1:18080:80") || !strings.Contains(draft.Compose, "Asia/Shanghai") {
		t.Fatalf("compose changes were not applied:\n%s", draft.Compose)
	}
	if !strings.Contains(draft.Env, "PASSWORD=strong-password") {
		t.Fatalf("env changes were not applied: %q", draft.Env)
	}
}

func TestDockerPreviewSynchronizesEasySettings(t *testing.T) {
	form := url.Values{
		"name":              {"demo"},
		"display_name":      {"Demo"},
		"compose":           {"services:\n  app:\n    image: demo:latest\n    ports:\n      - 127.0.0.1:16688:16688\n"},
		"env":               {"PASSWORD=change-me\n"},
		"port__0__app":      {"127.0.0.1:17777"},
		"portorig__0__app":  {"127.0.0.1:16688"},
		"env__PASSWORD":     {"new-password"},
		"envorig__PASSWORD": {"change-me"},
	}
	r := httptest.NewRequest(http.MethodPost, "/docker/preview", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s := &Server{}
	s.handleDockerPreview(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "127.0.0.1:17777:16688") || !strings.Contains(body, "PASSWORD=new-password") {
		t.Fatalf("preview did not synchronize settings: %s", body)
	}
}
