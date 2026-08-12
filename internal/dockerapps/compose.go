package dockerapps

import (
	"crypto/rand"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PortSetting and EnvSetting are best-effort hints for the beginner-friendly
// editor. The raw Compose and .env text remain editable and authoritative.
type PortSetting struct {
	Service   string
	Index     int
	Raw       string
	HostIP    string
	Published string
	Listen    string
	Target    string
	Protocol  string
	Simple    bool
}

type EnvSetting struct {
	Key       string
	Value     string
	Comment   string
	Secret    bool
	Generated bool
}

type ComposeEnvSetting struct {
	Service      string
	ServiceIndex int
	EntryIndex   int
	Key          string
	Value        string
	Secret       bool
	Generated    bool
}

type ComposeHints struct {
	Ports       []PortSetting
	Env         []EnvSetting
	ComposeEnv  []ComposeEnvSetting
	Images      []string
	HasBuild    bool
	UsesSecrets bool
}

func Analyze(compose, env string) ComposeHints {
	var hints ComposeHints
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &root); err == nil {
		doc := documentMap(&root)
		services := mapValue(doc, "services")
		if services != nil && services.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(services.Content); i += 2 {
				serviceIndex := i / 2
				service := services.Content[i].Value
				cfg := services.Content[i+1]
				if image := scalarValue(mapValue(cfg, "image")); image != "" {
					hints.Images = append(hints.Images, image)
				}
				if mapValue(cfg, "build") != nil {
					hints.HasBuild = true
				}
				if mapValue(cfg, "secrets") != nil {
					hints.UsesSecrets = true
				}
				hints.ComposeEnv = append(hints.ComposeEnv, parseComposeEnv(service, serviceIndex, mapValue(cfg, "environment"))...)
				ports := mapValue(cfg, "ports")
				if ports != nil && ports.Kind == yaml.SequenceNode {
					for index, p := range ports.Content {
						hint := parsePortNode(service, index, p)
						if hint.Raw != "" {
							hints.Ports = append(hints.Ports, hint)
						}
					}
				}
			}
		}
	}
	hints.Env = ParseEnv(env)
	for i := range hints.Env {
		if hints.Env[i].Secret && placeholderSecret(hints.Env[i].Value) {
			hints.Env[i].Value = randomSecret(24)
			hints.Env[i].Generated = true
		}
	}
	sort.Strings(hints.Images)
	return hints
}

func parseComposeEnv(service string, serviceIndex int, env *yaml.Node) []ComposeEnvSetting {
	if env == nil {
		return nil
	}
	var out []ComposeEnvSetting
	appendSetting := func(entryIndex int, key, value string) {
		key = strings.TrimSpace(key)
		if !envLineRE.MatchString(key) || strings.Contains(value, "${") {
			return
		}
		secret := isSecretKey(key)
		generated := secret && placeholderSecret(value)
		if generated {
			value = randomSecret(24)
		}
		out = append(out, ComposeEnvSetting{
			Service: service, ServiceIndex: serviceIndex, EntryIndex: entryIndex,
			Key: key, Value: value, Secret: secret, Generated: generated,
		})
	}
	switch env.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(env.Content); i += 2 {
			value := env.Content[i+1]
			if value.Kind == yaml.ScalarNode {
				appendSetting(i/2, env.Content[i].Value, value.Value)
			}
		}
	case yaml.SequenceNode:
		for i, item := range env.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			key, value, ok := strings.Cut(item.Value, "=")
			if ok {
				appendSetting(i, key, value)
			}
		}
	}
	return out
}

func isSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	return strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "PASSWD") ||
		strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") ||
		strings.Contains(upper, "API_KEY") || strings.HasSuffix(upper, "_KEY")
}

func placeholderSecret(value string) bool {
	v := strings.ToLower(strings.TrimSpace(strings.Trim(value, "\"'")))
	return v == "" || v == "changeme" || v == "change-me" || v == "password" ||
		v == "your-password" || v == "your_password" || v == "replace-me" || v == "replace_me"
}

func randomSecret(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	if n < 16 {
		n = 16
	}
	b := make([]byte, n)
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "please-change-this-password"
	}
	for i := range b {
		b[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(b)
}

func documentMap(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}
	return root
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func parsePortNode(service string, index int, node *yaml.Node) PortSetting {
	h := PortSetting{Service: service, Index: index, Protocol: "tcp"}
	if node.Kind == yaml.ScalarNode {
		h.Raw = node.Value
		if published, target, hostIP, proto, ok := splitShortPort(node.Value); ok {
			h.Published, h.Target, h.HostIP, h.Protocol, h.Simple = published, target, hostIP, proto, true
		}
	} else if node.Kind == yaml.MappingNode {
		h.Raw = "long syntax"
		h.Published = scalarValue(mapValue(node, "published"))
		h.Target = scalarValue(mapValue(node, "target"))
		h.HostIP = scalarValue(mapValue(node, "host_ip"))
		if p := scalarValue(mapValue(node, "protocol")); p != "" {
			h.Protocol = p
		}
		h.Simple = h.Published != "" && h.Target != ""
	}
	if h.Simple {
		h.Listen = formatPortBinding(h.HostIP, h.Published)
	}
	return h
}

func formatPortBinding(hostIP, published string) string {
	hostIP = strings.TrimSpace(strings.Trim(hostIP, "[]"))
	published = strings.TrimSpace(published)
	if hostIP == "" {
		return published
	}
	if strings.Contains(hostIP, ":") {
		hostIP = "[" + hostIP + "]"
	}
	return hostIP + ":" + published
}

func splitShortPort(raw string) (published, target, hostIP, protocol string, ok bool) {
	protocol = "tcp"
	value := strings.TrimSpace(raw)
	if base, proto, found := strings.Cut(value, "/"); found {
		value, protocol = base, strings.ToLower(proto)
	}
	if strings.ContainsAny(value, "${}") {
		return "", "", "", protocol, false
	}
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 || end+2 >= len(value) || value[end+1] != ':' {
			return "", "", "", protocol, false
		}
		hostIP = strings.Trim(value[:end+1], "[]")
		parts := strings.Split(value[end+2:], ":")
		if len(parts) != 2 {
			return "", "", "", protocol, false
		}
		published, target = parts[0], parts[1]
	} else {
		parts := strings.Split(value, ":")
		switch len(parts) {
		case 2:
			published, target = parts[0], parts[1]
		case 3:
			hostIP, published, target = parts[0], parts[1], parts[2]
		default:
			return "", "", "", protocol, false
		}
	}
	if strings.TrimSpace(published) == "" || strings.TrimSpace(target) == "" {
		return "", "", "", protocol, false
	}
	return strings.TrimSpace(published), strings.TrimSpace(target), strings.TrimSpace(hostIP), protocol, true
}

// ApplyPortChanges safely edits only recognized ports nodes in Compose YAML.
// Re-marshalling may normalize formatting, but preserves comments carried by
// yaml.Node. Users can always use the raw editor for complex port expressions.
func ApplyPortChanges(compose string, changes map[string]string) (string, error) {
	if len(changes) == 0 {
		return compose, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &root); err != nil {
		return "", fmt.Errorf("Compose YAML 解析失败: %w", err)
	}
	doc := documentMap(&root)
	services := mapValue(doc, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return "", errorsNew("Compose 中没有 services")
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		service := services.Content[i].Value
		ports := mapValue(services.Content[i+1], "ports")
		if ports == nil || ports.Kind != yaml.SequenceNode {
			continue
		}
		for index, node := range ports.Content {
			key := fmt.Sprintf("%s:%d", service, index)
			want, exists := changes[key]
			if !exists {
				continue
			}
			hostIP, published, err := parsePortBinding(want)
			if err != nil {
				return "", fmt.Errorf("服务 %s 的访问地址: %w", service, err)
			}
			hint := parsePortNode(service, index, node)
			if !hint.Simple {
				continue
			}
			if node.Kind == yaml.ScalarNode {
				value := formatPortBinding(hostIP, published) + ":" + hint.Target
				if hint.Protocol != "" && hint.Protocol != "tcp" {
					value += "/" + hint.Protocol
				}
				node.Value = value
			} else {
				setMapScalar(node, "published", published)
				setMapScalar(node, "host_ip", hostIP)
			}
		}
	}
	b, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parsePortBinding(value string) (hostIP, published string, err error) {
	value = strings.TrimSpace(value)
	if base, protocol, found := strings.Cut(value, "/"); found {
		if strings.EqualFold(strings.TrimSpace(protocol), "tcp") || strings.EqualFold(strings.TrimSpace(protocol), "udp") {
			value = strings.TrimSpace(base)
		}
	}
	if value == "" {
		return "", "", errorsNew("请填写端口，例如 16688 或 127.0.0.1:16688")
	}
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 || end+2 > len(value) || value[end+1] != ':' {
			return "", "", errorsNew("IPv6 地址请写成 [::1]:16688")
		}
		hostIP, published = value[1:end], value[end+2:]
	} else {
		switch strings.Count(value, ":") {
		case 0:
			published = value
		case 1:
			hostIP, published, _ = strings.Cut(value, ":")
		default:
			return "", "", errorsNew("IPv6 地址请写成 [::1]:16688")
		}
	}
	hostIP, published = strings.TrimSpace(hostIP), strings.TrimSpace(published)
	if hostIP != "" && net.ParseIP(hostIP) == nil {
		return "", "", errorsNew("监听地址必须是 IP，例如 127.0.0.1:16688")
	}
	if err := validatePublishedPort(published); err != nil {
		return "", "", err
	}
	return hostIP, published, nil
}

func setMapScalar(m *yaml.Node, key, value string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		if value == "" {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
		m.Content[i+1].Kind = yaml.ScalarNode
		m.Content[i+1].Tag = "!!str"
		m.Content[i+1].Value = value
		return
	}
	if value == "" {
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func validatePublishedPort(value string) error {
	if strings.Contains(value, "-") {
		parts := strings.Split(value, "-")
		if len(parts) != 2 {
			return errorsNew("端口范围格式不正确")
		}
		for _, part := range parts {
			if err := validatePublishedPort(part); err != nil {
				return err
			}
		}
		return nil
	}
	p, err := strconv.Atoi(value)
	if err != nil || p < 1 || p > 65535 {
		return errorsNew("端口必须是 1~65535 的数字")
	}
	return nil
}

func errorsNew(s string) error { return fmt.Errorf("%s", s) }

var envLineRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ParseEnv(raw string) []EnvSetting {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	result := make([]EnvSetting, 0, len(lines))
	comment := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			comment = strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			continue
		}
		if trimmed == "" {
			comment = ""
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || !envLineRE.MatchString(key) {
			comment = ""
			continue
		}
		secret := isSecretKey(key)
		result = append(result, EnvSetting{Key: key, Value: value, Comment: comment, Secret: secret})
		comment = ""
	}
	return result
}

// ApplyComposeEnvChanges edits simple literal environment entries inside
// service definitions. Interpolated expressions such as ${PASSWORD} stay in
// the raw editor and are normally backed by the .env form instead.
func ApplyComposeEnvChanges(compose string, changes map[string]string) (string, error) {
	if len(changes) == 0 {
		return compose, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &root); err != nil {
		return "", fmt.Errorf("Compose YAML 解析失败: %w", err)
	}
	services := mapValue(documentMap(&root), "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return "", errorsNew("Compose 中没有 services")
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceIndex := i / 2
		env := mapValue(services.Content[i+1], "environment")
		if env == nil {
			continue
		}
		switch env.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(env.Content); j += 2 {
				field := fmt.Sprintf("%d:%d", serviceIndex, j/2)
				value, exists := changes[field]
				if !exists {
					continue
				}
				if strings.ContainsAny(value, "\r\n\x00") {
					return "", errorsNew("环境变量不能包含换行")
				}
				env.Content[j+1].Kind = yaml.ScalarNode
				env.Content[j+1].Tag = "!!str"
				env.Content[j+1].Value = value
			}
		case yaml.SequenceNode:
			for j, item := range env.Content {
				field := fmt.Sprintf("%d:%d", serviceIndex, j)
				value, exists := changes[field]
				if !exists || item.Kind != yaml.ScalarNode {
					continue
				}
				if strings.ContainsAny(value, "\r\n\x00") {
					return "", errorsNew("环境变量不能包含换行")
				}
				key, _, ok := strings.Cut(item.Value, "=")
				if ok {
					item.Value = key + "=" + value
				}
			}
		}
	}
	b, err := yaml.Marshal(&root)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func ApplyEnvChanges(raw string, changes map[string]string) (string, error) {
	if len(changes) == 0 {
		return raw, nil
	}
	for key := range changes {
		if !envLineRE.MatchString(key) {
			return "", fmt.Errorf("环境变量名 %q 不正确", key)
		}
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	seen := make(map[string]bool)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		key, _, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || !envLineRE.MatchString(key) {
			continue
		}
		if value, exists := changes[key]; exists {
			if strings.ContainsAny(value, "\r\n\x00") {
				return "", fmt.Errorf("环境变量 %s 不能包含换行", key)
			}
			lines[i] = key + "=" + value
			seen[key] = true
		}
	}
	keys := make([]string, 0, len(changes))
	for key := range changes {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := changes[key]
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", fmt.Errorf("环境变量 %s 不能包含换行", key)
		}
		lines = append(lines, key+"="+value)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
}

func IsLoopbackBinding(hostIP string) bool {
	hostIP = strings.TrimSpace(hostIP)
	if hostIP == "localhost" {
		return true
	}
	ip := net.ParseIP(hostIP)
	return ip != nil && ip.IsLoopback()
}
