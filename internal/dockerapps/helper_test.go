package dockerapps

import "testing"

func TestHelperMutationClassification(t *testing.T) {
	for _, action := range []string{"engine-install", "image-remove", "image-pull", "image-prune", "deploy", "update", "up", "stop", "restart", "down", "validate"} {
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
