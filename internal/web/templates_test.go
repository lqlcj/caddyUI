package web

import (
	"os"
	"testing"
)

func TestAllTemplatesParse(t *testing.T) {
	assets := os.DirFS("../../web")
	s := &Server{assets: assets}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		if s.tmpl[page] == nil {
			t.Fatalf("template %q was not parsed", page)
		}
	}
}
