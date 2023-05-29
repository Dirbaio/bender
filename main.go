package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/go-github/v52/github"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sqlbunny/errors"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DataDir     string       `yaml:"data_dir"`
	ExternalURL string       `yaml:"external_url"`
	ListenPort  int          `yaml:"listen_port"`
	Image       string       `yaml:"image"`
	Github      GithubConfig `yaml:"github"`
}

type GithubConfig struct {
	WebhookSecret string `yaml:"webhook_secret"`
	AppID         int64  `yaml:"app_id"`
	PrivateKey    string `yaml:"private_key"`
}

type Service struct {
	config     Config
	containerd *containerd.Client

	runningJobsMutex sync.Mutex
	runningJobs      map[string]struct{}
}

func (s *Service) getRepoToken(ctx context.Context, installationID int64, repositoryID int64) (string, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, s.config.Github.AppID, installationID, []byte(s.config.Github.PrivateKey))
	itr.InstallationTokenOptions = &github.InstallationTokenOptions{
		RepositoryIDs: []int64{repositoryID},
		Permissions: &github.InstallationPermissions{
			Metadata: github.String("read"),
			Contents: github.String("read"),
		},
	}

	if err != nil {
		return "", errors.Errorf("Failed to create ghinstallation: %w", err)
	}

	token, err := itr.Token(ctx)
	if err != nil {
		return "", errors.Errorf("Failed to get repo token: %w", err)
	}

	return token, nil
}

type Event struct {
	Event      string // "push", "pull_request"
	Attributes map[string]string

	Repo           *github.Repository
	CloneURL       string
	SHA            string
	InstallationID int64
}

type Job struct {
	*Event
	ID     string
	Name   string
	Script string
}

func getRepoFromPushEvent(e *github.PushEvent) *github.Repository {
	return &github.Repository{
		ID:              e.Repo.ID,
		NodeID:          e.Repo.NodeID,
		Name:            e.Repo.Name,
		FullName:        e.Repo.FullName,
		Owner:           e.Repo.Owner,
		Private:         e.Repo.Private,
		Description:     e.Repo.Description,
		Fork:            e.Repo.Fork,
		CreatedAt:       e.Repo.CreatedAt,
		PushedAt:        e.Repo.PushedAt,
		UpdatedAt:       e.Repo.UpdatedAt,
		Homepage:        e.Repo.Homepage,
		PullsURL:        e.Repo.PullsURL,
		Size:            e.Repo.Size,
		StargazersCount: e.Repo.StargazersCount,
		WatchersCount:   e.Repo.WatchersCount,
		Language:        e.Repo.Language,
		HasIssues:       e.Repo.HasIssues,
		HasDownloads:    e.Repo.HasDownloads,
		HasWiki:         e.Repo.HasWiki,
		HasPages:        e.Repo.HasPages,
		ForksCount:      e.Repo.ForksCount,
		Archived:        e.Repo.Archived,
		Disabled:        e.Repo.Disabled,
		OpenIssuesCount: e.Repo.OpenIssuesCount,
		DefaultBranch:   e.Repo.DefaultBranch,
		MasterBranch:    e.Repo.MasterBranch,
		Organization:    e.Organization,
		URL:             e.Repo.URL,
		ArchiveURL:      e.Repo.ArchiveURL,
		HTMLURL:         e.Repo.HTMLURL,
		StatusesURL:     e.Repo.StatusesURL,
		GitURL:          e.Repo.GitURL,
		SSHURL:          e.Repo.SSHURL,
		CloneURL:        e.Repo.CloneURL,
		SVNURL:          e.Repo.SVNURL,
		Topics:          e.Repo.Topics,
	}
}

func is404(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response.StatusCode == 404
}

func removeExtension(s string) string {
	n := strings.LastIndexByte(s, '.')
	if n == -1 {
		return s
	}
	return s[:n]
}

func makeJobID() string {
	b := make([]byte, 6)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Service) handleWebhook(r *http.Request) error {
	payload, err := github.ValidatePayload(r, []byte(s.config.Github.WebhookSecret))
	defer r.Body.Close()
	if err != nil {
		log.Printf("error validating request body: err=%s\n", err)
		return nil
	}

	ee, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Printf("could not parse webhook: err=%s\n", err)
		return nil
	}

	var events []*Event

	switch e := ee.(type) {
	case *github.PushEvent:
		branch, ok := strings.CutPrefix(*e.Ref, "refs/heads/")
		if !ok {
			log.Printf("unknown ref '%s'", *e.Ref)
			return nil
		}
		events = append(events, &Event{
			Event: "push",
			Attributes: map[string]string{
				"branch": branch,
			},
			Repo:           getRepoFromPushEvent(e),
			SHA:            *e.HeadCommit.ID,
			InstallationID: *e.Installation.ID,
		})
	case *github.PullRequestEvent:
		if *e.Action == "opened" || *e.Action == "synchronize" {
			events = append(events, &Event{
				Event: "pull_request",
				Attributes: map[string]string{
					"branch": *e.PullRequest.Base.Ref,
				},
				Repo:           e.Repo,
				CloneURL:       *e.PullRequest.Head.Repo.CloneURL,
				SHA:            *e.PullRequest.Head.SHA,
				InstallationID: *e.Installation.ID,
			})
		}
	}

	if len(events) == 0 {
		return nil
	}

	gh, err := s.githubClient(events[0].InstallationID)
	if err != nil {
		return err
	}

	ctx := context.Background()
	for _, event := range events {
		if event.CloneURL == "" {
			event.CloneURL = *event.Repo.CloneURL
		}
		if event.Attributes == nil {
			event.Attributes = map[string]string{}
		}

		err = s.handleEvent(ctx, gh, event)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) handleEvent(ctx context.Context, gh *github.Client, event *Event) error {
	getOpts := &github.RepositoryContentGetOptions{
		Ref: event.SHA,
	}
	_, dir, _, err := gh.Repositories.GetContents(ctx, *event.Repo.Owner.Login, *event.Repo.Name, ".github/ci", getOpts)
	if is404(err) {
		log.Printf("`.github/ci` directory does not exist")
		return nil
	} else if err != nil {
		return err
	} else if dir == nil {
		log.Printf("`.github/ci` is not a directory")
		return nil
	}

	var jobs []*Job

	for _, f := range dir {
		if *f.Type != "file" {
			continue
		}

		file, _, _, err := gh.Repositories.GetContents(ctx, *event.Repo.Owner.Login, *event.Repo.Name, *f.Path, getOpts)
		if err != nil {
			return err
		}

		content, err := file.GetContent()
		if err != nil {
			return err
		}

		matched := false
	line:
		for _, line := range strings.Split(content, "\n") {
			if directive, ok := strings.CutPrefix(line, "##"); ok {
				fields := strings.Fields(directive)
				if len(fields) == 0 {
					continue
				}

				switch fields[0] {
				case "on":
					if len(fields) < 2 {
						log.Printf("warning: missing event in 'on' directive '%s'", directive)
						continue line
					}

					if fields[1] != event.Event {
						continue line
					}

					for _, condition := range fields[2:] {
						if !conditionMatches(condition, event.Attributes) {
							continue line
						}
					}

					matched = true

				default:
					log.Printf("warning: unknown directive '%s'", fields[0])
				}
			}
		}

		if matched {
			jobs = append(jobs, &Job{
				ID:     makeJobID(),
				Event:  event,
				Name:   removeExtension(*f.Name),
				Script: *f.Path,
			})
		}
	}

	for _, job := range jobs {
		go s.runJob(context.Background(), job)
	}

	return nil
}

func conditionMatches(condition string, attributes map[string]string) bool {
	re := regexp.MustCompile("^([a-zA-Z0-9_]+)(=|!=|~=|!~=)(.*)$")
	m := re.FindSubmatch([]byte(condition))
	if m == nil {
		log.Printf("warning: invalid condition '%s'", condition)
	}

	key := string(m[1])
	op := string(m[2])
	val := string(m[3])

	log.Print(key, op, val)

	switch op {
	case "=":
		return attributes[key] == val
	case "!=":
		return attributes[key] != val
	case "~=":
		ok, err := regexp.MatchString(fmt.Sprintf("^%s$", val), attributes[key])
		if err != nil {
			log.Printf("warning: invalid regexp in condition '%s': %v", condition, err)
			return false
		}
		return ok
	case "!~=":
		ok, err := regexp.MatchString(fmt.Sprintf("^%s$", val), attributes[key])
		if err != nil {
			log.Printf("warning: invalid regexp in condition '%s': %v", condition, err)
			return false
		}
		return !ok
	default:
		panic("unreachable")
	}
}

func (s *Service) githubClient(installationID int64) (*github.Client, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, s.config.Github.AppID, installationID, []byte(s.config.Github.PrivateKey))
	if err != nil {
		return nil, err
	}

	gh := github.NewClient(&http.Client{Transport: itr})
	return gh, nil
}

func (s *Service) setStatus(ctx context.Context, gh *github.Client, j *Job, state string) error {
	url := fmt.Sprintf("%s/jobs/%s", s.config.ExternalURL, j.ID)
	_, _, err := gh.Repositories.CreateStatus(ctx,
		*j.Repo.Owner.Login,
		*j.Repo.Name,
		j.SHA,
		&github.RepoStatus{
			State:     github.String(state),
			Context:   github.String(fmt.Sprintf("ci/%s", j.Name)),
			TargetURL: &url,
		})
	return err
}

func (s *Service) runJob(ctx context.Context, job *Job) {
	s.runningJobsMutex.Lock()
	s.runningJobs[job.ID] = struct{}{}
	s.runningJobsMutex.Unlock()

	defer func() {
		s.runningJobsMutex.Lock()
		delete(s.runningJobs, job.ID)
		s.runningJobsMutex.Unlock()
	}()

	logs, err := os.Create(filepath.Join(s.config.DataDir, "logs", job.ID))
	if err != nil {
		log.Printf("error creating log file: %v", err)
		return
	}

	gh, err := s.githubClient(job.InstallationID)
	if err != nil {
		log.Printf("error creating github client: %v", err)
		return
	}

	err = s.setStatus(ctx, gh, job, "pending")
	if err != nil {
		log.Printf("error creating pending status: %v", err)
	}

	err = nopanic(func() error {
		return s.runJobInner(ctx, job, gh, logs)
	})

	result := "success"
	if err != nil {
		fmt.Fprintf(logs, "run failed: %v\n", err)
		log.Printf("job run failed: %v", err)
		result = "failure"
	}

	err = s.setStatus(ctx, gh, job, result)
	if err != nil {
		log.Printf("error creating result status: %v", err)
	}

	err = os.RemoveAll(filepath.Join(s.config.DataDir, "jobs", job.ID))
	if err != nil {
		log.Printf("error deleting job homedir: %v", err)
	}
}

func nopanic(fn func() error) (err error) {
	// This very convoluted code is because there's no way to distinguish
	// between `panic(nil)` and no panic with just `recover()` (both return nil)
	// https://github.com/golang/go/issues/25448
	panicked := true
	err = nil
	defer func() {
		if panicked {
			rvr := recover()
			if rvre, ok := rvr.(error); ok {
				err = errors.Errorf("panic: %w", rvre)
			} else {
				err = errors.Errorf("panic: %+v", rvr)
			}
		}
	}()

	err = fn()

	panicked = false
	return
}

func (s *Service) runJobInner(ctx context.Context, job *Job, gh *github.Client, logs *os.File) error {
	token, err := s.getRepoToken(ctx, job.InstallationID, *job.Repo.ID)
	if err != nil {
		return err
	}
	log.Printf("repo token: %s", token)

	ctx = namespaces.WithNamespace(ctx, "bender")

	image, err := s.containerd.GetImage(ctx, s.config.Image)
	if err != nil {
		log.Println("Image not found. pulling it. ", err)
		image, err = s.containerd.Pull(ctx, s.config.Image, containerd.WithPullUnpack)
		if err != nil {
			return err
		}
	}

	// Read image imageConfig.
	var imageConfig ocispec.Image
	configDesc, err := image.Config(ctx) // aware of img.platform
	if err != nil {
		return err
	}
	p, err := content.ReadBlob(ctx, image.ContentStore(), configDesc)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(p, &imageConfig); err != nil {
		return err
	}

	spew.Dump(imageConfig.Config.Env)

	log.Println("creating container")

	// Create home dir
	home := filepath.Join(s.config.DataDir, "jobs", job.ID)
	err = os.MkdirAll(home, 0700)
	if err != nil {
		return err
	}

	buf := bytes.NewBuffer(nil)
	buf.WriteString("machine github.com\nlogin x-access-token\npassword ")
	buf.WriteString(token)
	err = os.WriteFile(filepath.Join(home, ".netrc"), buf.Bytes(), 0600)
	if err != nil {
		return err
	}

	buf = bytes.NewBuffer(nil)
	buf.WriteString("#!/bin/bash\n")
	buf.WriteString("set -euxo pipefail\n")
	buf.WriteString(fmt.Sprintf("git clone -n %s code\n", job.CloneURL))
	buf.WriteString("cd code\n")
	buf.WriteString(fmt.Sprintf("git checkout %s\n", job.SHA))
	buf.WriteString(fmt.Sprintf("exec %s\n", job.Script))
	err = os.WriteFile(filepath.Join(home, "entrypoint.sh"), buf.Bytes(), 0700)
	if err != nil {
		return err
	}

	container, err := s.containerd.NewContainer(ctx, fmt.Sprintf("job-%s", job.ID),
		containerd.WithNewSnapshot(fmt.Sprintf("job-%s-rootfs", job.ID), image),
		containerd.WithNewSpec(
			oci.WithProcessArgs("/bin/bash", "-c", "./entrypoint.sh 2>&1"),
			oci.WithProcessCwd("/ci"),
			oci.WithUIDGID(1000, 1000),
			oci.WithDefaultPathEnv,
			oci.WithEnv(imageConfig.Config.Env),
			oci.WithEnv([]string{
				"HOME=/ci",
			}),
			oci.WithHostNamespace(specs.NetworkNamespace), // TODO network sandboxing
			oci.WithMounts([]specs.Mount{
				{
					Type:        "none",
					Source:      home,
					Destination: "/ci",
					Options:     []string{"rbind"},
				},
				{
					Type:        "none",
					Source:      filepath.Join(s.config.DataDir, "cache"),
					Destination: "/ci/cache",
					Options:     []string{"rbind"},
				},
				{
					Type:        "none",
					Source:      "/etc/resolv.conf",
					Destination: "/etc/resolv.conf",
					Options:     []string{"rbind", "ro"},
				},
			}),
		),
	)
	if err != nil {
		return err
	}
	defer container.Delete(ctx)

	log.Println("creating task")

	// create a new task
	task, err := container.NewTask(ctx, cio.NewCreator(
		cio.WithFIFODir(filepath.Join(s.config.DataDir, "fifo")),
		cio.WithStreams(nil, logs, logs),
	))
	if err != nil {
		return err
	}
	defer task.Delete(ctx)
	defer task.Kill(ctx, syscall.SIGKILL)

	// the task is now running and has a pid that can be used to setup networking
	// or other runtime settings outside of containerd
	pid := task.Pid()
	log.Printf("pid: %d", pid)

	log.Println("starting task")

	// start the redis-server process inside the container
	err = task.Start(ctx)
	if err != nil {
		return err
	}

	// wait for the task to exit and get the exit status
	statusC, err := task.Wait(ctx)
	if err != nil {
		return err
	}

	status := <-statusC

	if err := status.Error(); err != nil {
		return err
	}
	if status.ExitCode() != 0 {
		return errors.Errorf("exited with code %d", status.ExitCode())
	}

	return nil
}

func (s *Service) isJobRunning(id string) bool {
	s.runningJobsMutex.Lock()
	_, isRunning := s.runningJobs[id]
	s.runningJobsMutex.Unlock()
	return isRunning
}

func validJobID(id string) bool {
	ok, err := regexp.MatchString("^[a-z0-9]+$", id)
	return err == nil && ok
}

func (s *Service) HandleJobLogs(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	if !validJobID(jobID) {
		log.Printf("invalid job ID: '%s'", jobID)
		http.Error(w, http.StatusText(404), 404)
		return
	}

	f, err := os.Open(filepath.Join(s.config.DataDir, "logs", jobID))
	if err != nil {
		log.Printf("failed to open log file: %v", err)
		http.Error(w, http.StatusText(404), 404)
		return
	}

	w.Header().Add("Content-Type", "text/plain")

	for {
		n, err := io.Copy(w, f)
		if err != nil {
			log.Printf("failed to send logs: %v", err)
			http.Error(w, http.StatusText(500), 500)
			return
		}

		if !s.isJobRunning(jobID) {
			return
		}

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		d := time.Second
		if n != 0 {
			d = 200 * time.Millisecond
		}
		time.Sleep(d)
	}
}

func (s *Service) Run() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Get("/jobs/{jobID}", s.HandleJobLogs)
	r.Post("/webhook", func(w http.ResponseWriter, r *http.Request) {
		err := s.handleWebhook(r)
		if err != nil {
			log.Println(err)
			w.WriteHeader(500)
		} else {
			w.WriteHeader(200)
		}
	})

	log.Println("server started")
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", s.config.ListenPort), r))
}

func main() {
	configData, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	var config Config
	err = yaml.Unmarshal(configData, &config)
	if err != nil {
		log.Fatal(err)
	}

	config.DataDir, err = filepath.Abs(config.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	for _, subdir := range []string{"logs", "fifo", "cache"} {
		err = os.MkdirAll(filepath.Join(config.DataDir, subdir), 0700)
		if err != nil {
			log.Fatal(err)
		}
	}

	cntd, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		log.Fatal(err)
	}

	s := Service{
		config:      config,
		containerd:  cntd,
		runningJobs: make(map[string]struct{}),
	}

	s.Run()
}
