package dockerapps

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHelperMutationClassification(t *testing.T) {
	for _, action := range []string{"engine-install", "engine-update", "image-remove", "image-pull", "image-prune", "deploy", "update", "up", "stop", "restart", "down", "uninstall", "validate"} {
		if !helperMutation(action) {
			t.Fatalf("%q should be serialized", action)
		}
	}
	for _, action := range []string{"info", "images", "statuses", "ps", "logs"} {
		if helperMutation(action) {
			t.Fatalf("%q should remain read-only", action)
		}
	}
}

func TestLimitedBufferTailRemainsValidUTF8(t *testing.T) {
	b := &limitedBuffer{limit: 4, keepTail: true}
	_, _ = b.Write([]byte("甲乙丙"))
	if got := b.String(); !utf8.ValidString(got) || !strings.HasSuffix(got, "丙") {
		t.Fatalf("tail buffer returned invalid UTF-8 or lost tail: %q", got)
	}
}

func TestLimitedBufferCanKeepLatestLogBytes(t *testing.T) {
	b := &limitedBuffer{limit: 8, keepTail: true}
	for _, part := range []string{"abc", "defg", "hijkl"} {
		if _, err := b.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.String(), "efghijkl"; got != want {
		t.Fatalf("tail buffer = %q, want %q", got, want)
	}
	if !b.truncated || strings.Contains(b.String(), "abcd") {
		t.Fatalf("tail buffer did not discard old output: %#v", b)
	}
}

func TestUninstallComposeArgsDeleteProjectOwnedResources(t *testing.T) {
	h := &HelperServer{}
	ref := HelperAppRef{
		Name: "demo", ProjectDir: "/apps/demo", ComposeFile: "/apps/demo/compose.yaml",
	}
	got := h.composeArgs(ref, "down", "--volumes", "--remove-orphans", "--rmi", "local")
	wantTail := []string{"down", "--volumes", "--remove-orphans", "--rmi", "local"}
	if !reflect.DeepEqual(got[len(got)-len(wantTail):], wantTail) {
		t.Fatalf("uninstall args = %#v, want suffix %#v", got, wantTail)
	}
}
