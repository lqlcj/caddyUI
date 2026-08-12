// Package dockerapps provides the lightweight Docker Compose application
// manager used by CaddyUI. It deliberately manages Compose projects instead
// of exposing a general-purpose shell or the raw Docker socket to the web
// process.
package dockerapps

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	metadataFile = ".caddyui-app.json"
	metadataV1   = 1

	DefaultSocketPath = "/run/caddyui-docker.sock"
)

// Source describes where an imported Compose project came from. GitHub
// imports keep enough information to download the whole repository archive,
// so projects that use a Dockerfile or nearby config files also work.
type Source struct {
	URL         string `json:"url,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Ref         string `json:"ref,omitempty"`
	ComposePath string `json:"compose_path,omitempty"`
}

func (s Source) IsGitHub() bool {
	return s.Owner != "" && s.Repo != "" && s.Ref != "" && s.ComposePath != ""
}

// App is one CaddyUI-managed Compose project on disk.
type App struct {
	Version     int    `json:"version"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Source      Source `json:"source,omitempty"`
	ComposeRel  string `json:"compose_rel"`
	EnvRel      string `json:"env_rel"`
	Managed     bool   `json:"managed,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`

	Dir string `json:"-"`
}

func (a *App) Title() string {
	if strings.TrimSpace(a.DisplayName) != "" {
		return a.DisplayName
	}
	return a.Name
}

// HelperAppRef is the small, explicit set of paths sent to the privileged
// helper. The helper re-validates all of them against its root-owned config.
type HelperAppRef struct {
	Name        string `json:"name"`
	AppDir      string `json:"app_dir"`
	ProjectDir  string `json:"project_dir"`
	ComposeFile string `json:"compose_file"`
	EnvFile     string `json:"env_file,omitempty"`
}

// DockerInfo is shown at the top of the Docker pages.
type DockerInfo struct {
	Available      bool   `json:"available"`
	DockerVersion  string `json:"docker_version,omitempty"`
	ComposeVersion string `json:"compose_version,omitempty"`
	AppsRoot       string `json:"apps_root,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Publisher is one host-to-container port mapping reported by Compose.
type Publisher struct {
	URL           string `json:"URL,omitempty"`
	TargetPort    int    `json:"TargetPort,omitempty"`
	PublishedPort int    `json:"PublishedPort,omitempty"`
	Protocol      string `json:"Protocol,omitempty"`
}

// Container is the intentionally small subset of `docker compose ps` that
// the UI needs.
type Container struct {
	Service    string      `json:"Service,omitempty"`
	Name       string      `json:"Name,omitempty"`
	State      string      `json:"State,omitempty"`
	Status     string      `json:"Status,omitempty"`
	Image      string      `json:"Image,omitempty"`
	Publishers []Publisher `json:"Publishers,omitempty"`
}

func (c Container) Running() bool {
	return strings.EqualFold(c.State, "running") || strings.HasPrefix(strings.ToLower(c.Status), "up ")
}

// Image is one row from `docker image ls`.
type Image struct {
	Repository   string `json:"Repository,omitempty"`
	Tag          string `json:"Tag,omitempty"`
	ID           string `json:"ID,omitempty"`
	CreatedSince string `json:"CreatedSince,omitempty"`
	Size         string `json:"Size,omitempty"`
}

func (i Image) Name() string {
	if i.Repository == "<none>" || i.Repository == "" {
		return i.ID
	}
	if i.Tag == "<none>" || i.Tag == "" {
		return i.Repository
	}
	return i.Repository + ":" + i.Tag
}

type JobState string

const (
	JobRunning JobState = "running"
	JobOK      JobState = "ok"
	JobFailed  JobState = "failed"
)

// Job tracks a long Docker operation in memory. Compose files and containers
// remain the source of truth, so losing this transient status on restart is
// harmless.
type Job struct {
	Key        string
	Action     string
	State      JobState
	Output     string
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

func (j Job) Running() bool { return j.State == JobRunning }

type HelperRequest struct {
	Action   string         `json:"action"`
	AppsRoot string         `json:"apps_root,omitempty"`
	App      *HelperAppRef  `json:"app,omitempty"`
	Apps     []HelperAppRef `json:"apps,omitempty"`
	Image    string         `json:"image,omitempty"`
	Tail     int            `json:"tail,omitempty"`
}

type HelperResponse struct {
	OK           bool                   `json:"ok"`
	Error        string                 `json:"error,omitempty"`
	Output       string                 `json:"output,omitempty"`
	Info         *DockerInfo            `json:"info,omitempty"`
	Containers   []Container            `json:"containers,omitempty"`
	Statuses     map[string][]Container `json:"statuses,omitempty"`
	StatusErrors map[string]string      `json:"status_errors,omitempty"`
	Images       []Image                `json:"images,omitempty"`
}

var appNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

func ValidAppName(name string) bool { return appNameRE.MatchString(name) }

// Slugify turns a repository/display name into a valid Compose project name.
func Slugify(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastSep := false
	for _, r := range raw {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastSep = false
			continue
		}
		if b.Len() > 0 && !lastSep {
			b.WriteByte('-')
			lastSep = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	if out == "" {
		return "docker-app"
	}
	return out
}

func sortApps(apps []*App) {
	sort.Slice(apps, func(i, j int) bool {
		return strings.ToLower(apps[i].Title()) < strings.ToLower(apps[j].Title())
	})
}
