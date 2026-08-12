package web

import (
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
		"port__0__app":      {"18080"},
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
	if !strings.Contains(draft.Compose, "18080:80") || !strings.Contains(draft.Compose, "Asia/Shanghai") {
		t.Fatalf("compose changes were not applied:\n%s", draft.Compose)
	}
	if !strings.Contains(draft.Env, "PASSWORD=strong-password") {
		t.Fatalf("env changes were not applied: %q", draft.Env)
	}
}
