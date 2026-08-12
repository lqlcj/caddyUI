package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestThemeFrom(t *testing.T) {
	tests := []struct {
		name   string
		cookie *http.Cookie
		want   string
	}{
		{name: "no cookie defaults to dark", want: themeDark},
		{name: "light preference", cookie: &http.Cookie{Name: themeCookie, Value: themeLight}, want: themeLight},
		{name: "dark preference", cookie: &http.Cookie{Name: themeCookie, Value: themeDark}, want: themeDark},
		{name: "invalid preference defaults to dark", cookie: &http.Cookie{Name: themeCookie, Value: "system"}, want: themeDark},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.cookie != nil {
				r.AddCookie(tt.cookie)
			}

			if got := themeFrom(r); got != tt.want {
				t.Fatalf("themeFrom() = %q, want %q", got, tt.want)
			}
		})
	}
}
