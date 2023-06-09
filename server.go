package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/go-github/v52/github"
)

func (s *Service) serverRun() {

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Get("/jobs/{jobID}", s.HandleJobLogs)
	r.Get("/jobs/{jobID}/artifacts", http.RedirectHandler("artifacts/", http.StatusMovedPermanently).ServeHTTP)
	r.Get("/jobs/{jobID}/artifacts/*", s.HandleJobArtifacts)
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

func validJobID(id string) bool {
	ok, err := regexp.MatchString("^[a-z0-9]+$", id)
	return err == nil && ok
}

func (s *Service) HandleJobArtifacts(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	if !validJobID(jobID) {
		log.Printf("invalid job ID: '%s'", jobID)
		http.Error(w, http.StatusText(404), 404)
		return
	}

	// serve files from data/artifacts/<jobID>/
	http.StripPrefix("/jobs/"+jobID+"/artifacts/", http.FileServer(http.Dir(filepath.Join(s.config.DataDir, "artifacts", jobID)))).ServeHTTP(w, r)
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

		cacheBranch := branch
		if m := regexp.MustCompile("^gh-readonly-queue/([^/]+)/").FindStringSubmatch(branch); m != nil {
			cacheBranch = m[1]
			log.Printf("branch '%s' is from merge queue, using target branch '%s' for cache", branch, cacheBranch)
		}

		if e.HeadCommit == nil {
			// this is a branch deletion.
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
			Cache: []string{
				fmt.Sprintf("branch-%s", cacheBranch),
				fmt.Sprintf("branch-%s", *e.Repo.DefaultBranch),
			},
			Trusted: true,
		})
	case *github.PullRequestEvent:
		if *e.Action == "opened" || *e.Action == "synchronize" {
			events = append(events, &Event{
				Event: "pull_request",
				Attributes: map[string]string{
					"branch": *e.PullRequest.Base.Ref,
				},
				Repo:           e.Repo,
				PullRequest:    e.PullRequest,
				CloneURL:       *e.PullRequest.Head.Repo.CloneURL,
				SHA:            *e.PullRequest.Head.SHA,
				InstallationID: *e.Installation.ID,
				Cache: []string{
					fmt.Sprintf("pr-%d", *e.PullRequest.Number),
					fmt.Sprintf("branch-%s", *e.PullRequest.Base.Ref),
					fmt.Sprintf("branch-%s", *e.Repo.DefaultBranch),
				},

				// Trusted if the PR is not from a fork.
				Trusted: *e.PullRequest.Head.Repo.Owner.Login == *e.Repo.Owner.Login,
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

		meta, err := parseMeta(content)
		if err != nil {
			log.Printf("failed to parse meta for file '%s': %v", *f.Name, err)
			continue
		}

		matched := false

		for _, me := range meta.Events {
			if me.Event != event.Event {
				continue
			}

			ok := true
			for _, condition := range me.Conditions {
				if !condition.matches(event.Attributes) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}

			matched = true
			break
		}

		if matched {
			jobs = append(jobs, &Job{
				ID:              makeJobID(),
				Event:           event,
				Name:            removeExtension(*f.Name),
				Script:          *f.Path,
				Permissions:     meta.Permissions,
				PermissionRepos: meta.PermissionRepos,
			})
		}
	}

	for _, job := range jobs {
		go s.runJob(context.Background(), job)
	}

	return nil
}
