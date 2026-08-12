package dockerapps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveDraftAndUpdate(t *testing.T) {
	root := t.TempDir()
	m := New(root, filepath.Join(root, "missing.sock"))
	app, err := m.SaveDraft(context.Background(), Draft{
		Name: "demo", DisplayName: "Demo", Compose: "services:\n  web:\n    image: nginx:latest\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "demo" {
		t.Fatalf("unexpected app: %#v", app)
	}
	if !app.Managed {
		t.Fatal("newly installed app should be marked as managed")
	}
	loaded, err := m.App("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(loaded.Dir, metadataFile)); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateFiles(loaded, "Renamed", "services:\n  web:\n    image: nginx:alpine\n", "TZ=UTC\n"); err != nil {
		t.Fatal(err)
	}
	if env, err := m.Env(loaded); err != nil || env != "TZ=UTC\n" {
		t.Fatalf("env = %q, err = %v", env, err)
	}
}

func TestSlugify(t *testing.T) {
	if got := Slugify(" My Great_App! "); got != "my-great-app" {
		t.Fatalf("Slugify() = %q", got)
	}
}

func TestNewNormalizesRootToAbsolutePath(t *testing.T) {
	m := New("relative-docker-apps", "missing.sock")
	if !filepath.IsAbs(m.Root) {
		t.Fatalf("manager root should be absolute: %q", m.Root)
	}
}

func TestRemoveAppRejectsAdoptedDirectories(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	appDir := filepath.Join(m.Root, "demo")
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		t.Fatal(err)
	}
	app := &App{Name: "demo", Dir: appDir, Managed: false}
	if err := m.RemoveApp(app); err == nil {
		t.Fatal("RemoveApp should reject an adopted app directory")
	}
	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("adopted directory should remain in place: %v", err)
	}
}

func TestRunJobUsesGlobalLockAndRecordsResult(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := m.RunJob(context.Background(), "demo", "正在测试", func(context.Context) (string, error) {
			close(started)
			<-release
			return "ok", nil
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("RunJob did not start")
	}
	if !m.Busy() {
		t.Fatal("manager should be busy while RunJob is running")
	}
	if err := m.StartJob("other", "其它任务", func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Fatal("StartJob should be rejected while RunJob holds the global lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	job := m.Job("demo")
	if job == nil || job.State != JobOK || job.Output != "ok" || m.Busy() {
		t.Fatalf("unexpected completed job: %#v, busy=%v", job, m.Busy())
	}
}

func TestRunJobReleasesGlobalLockAfterFailure(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	want := errors.New("boom")
	_, err := m.RunJob(context.Background(), "demo", "正在测试", func(context.Context) (string, error) {
		return "details", want
	})
	if !errors.Is(err, want) {
		t.Fatalf("RunJob error = %v", err)
	}
	job := m.Job("demo")
	if job == nil || job.State != JobFailed || job.Error != want.Error() || m.Busy() {
		t.Fatalf("unexpected failed job: %#v, busy=%v", job, m.Busy())
	}
}

func TestConcurrentSaveDraftAllowsOnlyOneApp(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	draft := Draft{Name: "demo", DisplayName: "Demo", Compose: "services:\n  web:\n    image: nginx\n"}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := m.SaveDraft(context.Background(), draft)
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent saves = %d, want 1", successes)
	}
	if _, err := m.App("demo"); err != nil {
		t.Fatal(err)
	}
}

func TestStartJobWaitsForDraftMutation(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	m.mutation.Lock()
	started := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		close(started)
		returned <- m.StartJob("demo", "正在测试", func(context.Context) (string, error) { return "", nil })
	}()
	<-started
	select {
	case err := <-returned:
		t.Fatalf("StartJob returned before the mutation completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.mutation.Unlock()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartJob did not continue after the mutation lock was released")
	}
}

func TestStartJobBlocksFileMutationUntilJobFinishes(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	release := make(chan struct{})
	jobStarted := make(chan struct{})
	if err := m.StartJob("demo", "正在测试", func(context.Context) (string, error) {
		close(jobStarted)
		<-release
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	<-jobStarted
	saveDone := make(chan error, 1)
	go func() {
		_, err := m.SaveDraft(context.Background(), Draft{
			Name: "other", DisplayName: "Other", Compose: "services:\n  web:\n    image: nginx\n",
		})
		saveDone <- err
	}()
	select {
	case err := <-saveDone:
		if err == nil {
			t.Fatal("SaveDraft should not succeed while a job is running")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("SaveDraft should reject a running job instead of waiting")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for m.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Busy() {
		t.Fatal("background job did not release the global lock")
	}
}

func TestJobsPruneExpiredAndKeepRunning(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	now := time.Now()
	m.jobs["expired"] = Job{Key: "expired", State: JobOK, StartedAt: now.Add(-48 * time.Hour), FinishedAt: now.Add(-25 * time.Hour)}
	m.jobs["recent"] = Job{Key: "recent", State: JobOK, StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour)}
	m.jobs["running"] = Job{Key: "running", State: JobRunning, StartedAt: now.Add(-48 * time.Hour)}

	m.jobsMu.Lock()
	m.pruneJobsLocked(now)
	m.jobsMu.Unlock()

	if m.Job("expired") != nil {
		t.Fatal("expired completed job should be removed")
	}
	if m.Job("recent") == nil || m.Job("running") == nil {
		t.Fatal("recent and running jobs should be retained")
	}
}

func TestJobsStayWithinRetentionLimit(t *testing.T) {
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	now := time.Now()
	for i := 0; i < maxRetainedJobs+10; i++ {
		key := fmt.Sprintf("job-%02d", i)
		finished := now.Add(time.Duration(i) * time.Minute)
		m.jobs[key] = Job{Key: key, State: JobOK, StartedAt: finished.Add(-time.Minute), FinishedAt: finished}
	}
	m.jobs["running"] = Job{Key: "running", State: JobRunning, StartedAt: now}

	m.jobsMu.Lock()
	m.pruneJobsLocked(now.Add(2 * time.Hour))
	m.jobsMu.Unlock()

	if len(m.Jobs()) != maxRetainedJobs {
		t.Fatalf("retained jobs = %d, want %d", len(m.Jobs()), maxRetainedJobs)
	}
	if m.Job("running") == nil {
		t.Fatal("running job should never be pruned")
	}
	if m.Job("job-00") != nil {
		t.Fatal("oldest completed job should be pruned first")
	}
}
