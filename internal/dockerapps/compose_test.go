package dockerapps

import (
	"strings"
	"testing"
)

func TestAnalyzeAndApplyPortChanges(t *testing.T) {
	compose := `services:
  web:
    image: nginx:latest
    ports:
      - "8080:80"
  db:
    image: postgres:17
`
	hints := Analyze(compose, "")
	if len(hints.Ports) != 1 || hints.Ports[0].Published != "8080" || hints.Ports[0].Target != "80" {
		t.Fatalf("unexpected ports: %#v", hints.Ports)
	}
	updated, err := ApplyPortChanges(compose, map[string]string{"web:0": "18080"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "18080:80") {
		t.Fatalf("updated compose did not contain changed port:\n%s", updated)
	}
}

func TestAnalyzeAndApplyLoopbackPortBinding(t *testing.T) {
	compose := `services:
  app:
    image: demo:latest
    ports:
      - '127.0.0.1:16688:16688'
`
	hints := Analyze(compose, "")
	if len(hints.Ports) != 1 || hints.Ports[0].Listen != "127.0.0.1:16688" || hints.Ports[0].Target != "16688" {
		t.Fatalf("unexpected loopback port hint: %#v", hints.Ports)
	}
	updated, err := ApplyPortChanges(compose, map[string]string{"app:0": "127.0.0.1:17777"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "127.0.0.1:17777:16688") {
		t.Fatalf("updated compose did not preserve loopback binding:\n%s", updated)
	}
	updated, err = ApplyPortChanges(updated, map[string]string{"app:0": "18888"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "127.0.0.1") || !strings.Contains(updated, "18888:16688") {
		t.Fatalf("updated compose did not remove host binding:\n%s", updated)
	}
}

func TestApplyLongPortBinding(t *testing.T) {
	compose := `services:
  app:
    image: demo:latest
    ports:
      - target: 80
        published: "8080"
        protocol: tcp
`
	updated, err := ApplyPortChanges(compose, map[string]string{"app:0": "127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "host_ip: 127.0.0.1") || !strings.Contains(updated, `published: "18080"`) {
		t.Fatalf("long syntax was not updated:\n%s", updated)
	}
}

func TestApplyIPv6LoopbackPortBinding(t *testing.T) {
	compose := `services:
  app:
    image: demo:latest
    ports:
      - '[::1]:8080:80'
`
	hints := Analyze(compose, "")
	if len(hints.Ports) != 1 || hints.Ports[0].Listen != "[::1]:8080" {
		t.Fatalf("unexpected IPv6 port hint: %#v", hints.Ports)
	}
	updated, err := ApplyPortChanges(compose, map[string]string{"app:0": "[::1]:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "[::1]:18080:80") {
		t.Fatalf("IPv6 binding was not updated:\n%s", updated)
	}
}

func TestApplyPortBindingAcceptsDisplayedProtocolSuffix(t *testing.T) {
	compose := `services:
  dns:
    image: demo:latest
    ports:
      - '127.0.0.1:5353:53/udp'
`
	updated, err := ApplyPortChanges(compose, map[string]string{"dns:0": "127.0.0.1:5533/udp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "127.0.0.1:5533:53/udp") {
		t.Fatalf("UDP binding was not updated:\n%s", updated)
	}
}

func TestAnalyzeAndApplyComposeEnvironment(t *testing.T) {
	compose := `services:
  app:
    image: demo:latest
    environment:
      ADMIN_PASSWORD: change-me
      TZ: UTC
  worker:
    image: demo:latest
    environment:
      - MODE=prod
`
	hints := Analyze(compose, "")
	if len(hints.ComposeEnv) != 3 || !hints.ComposeEnv[0].Generated {
		t.Fatalf("unexpected compose env hints: %#v", hints.ComposeEnv)
	}
	updated, err := ApplyComposeEnvChanges(compose, map[string]string{"0:1": "Asia/Shanghai", "1:0": "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "Asia/Shanghai") || !strings.Contains(updated, "MODE=debug") {
		t.Fatalf("unexpected compose env output:\n%s", updated)
	}
}

func TestParseAndApplyEnv(t *testing.T) {
	raw := "# database password\nDB_PASSWORD=change-me\nTZ=UTC\n"
	settings := ParseEnv(raw)
	if len(settings) != 2 || !settings[0].Secret || settings[0].Comment != "database password" {
		t.Fatalf("unexpected env settings: %#v", settings)
	}
	updated, err := ApplyEnvChanges(raw, map[string]string{"TZ": "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "TZ=Asia/Shanghai") || !strings.Contains(updated, "DB_PASSWORD=change-me") {
		t.Fatalf("unexpected env output: %q", updated)
	}
}

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		raw, owner, repo, ref, file string
	}{
		{"https://github.com/louislam/uptime-kuma", "louislam", "uptime-kuma", "", ""},
		{"https://github.com/acme/demo/blob/main/deploy/compose.yaml", "acme", "demo", "main", "deploy/compose.yaml"},
	}
	for _, tt := range tests {
		got, err := parseGitHubURL(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got.Owner != tt.owner || got.Repo != tt.repo || got.Ref != tt.ref || got.ComposePath != tt.file {
			t.Fatalf("parseGitHubURL(%q) = %#v", tt.raw, got)
		}
	}
}

func TestDecodeContainersSupportsJSONArrayAndJSONLines(t *testing.T) {
	for _, raw := range []string{
		`[{"Service":"web","State":"running"}]`,
		"{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"db\",\"State\":\"exited\"}\n",
	} {
		got, err := decodeContainers(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0].Service != "web" {
			t.Fatalf("unexpected containers: %#v", got)
		}
	}
}
