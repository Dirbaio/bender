package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/google/go-github/v52/github"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sqlbunny/errors"
)

func (s *Service) isJobRunning(id string) bool {
	s.runningJobsMutex.Lock()
	_, isRunning := s.runningJobs[id]
	s.runningJobsMutex.Unlock()
	return isRunning
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
}

func (s *Service) runJobInner(ctx context.Context, job *Job, gh *github.Client, logs *os.File) error {
	token, err := s.getRepoToken(ctx, job)
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

	log.Println("creating container")

	// Create artifacts dir
	artifactsDir := filepath.Join(s.config.DataDir, "artifacts", job.ID)
	err = os.MkdirAll(artifactsDir, 0700)
	if err != nil {
		return err
	}

	// Create job dir
	jobDir := filepath.Join(s.config.DataDir, "jobs", job.ID)
	err = os.MkdirAll(jobDir, 0700)
	if err != nil {
		return err
	}
	home := filepath.Join(jobDir, "home")
	err = os.MkdirAll(home, 0700)
	if err != nil {
		return err
	}
	defer func() {
		log.Printf("deleting job dir: %s", jobDir)
		err := os.RemoveAll(jobDir)
		if err != nil {
			log.Printf("error deleting job dir: %v", err)
		}
	}()

	// Setup cache
	cacheDir := filepath.Join(s.config.DataDir, "cache", *job.Repo.Owner.Login, *job.Repo.Name, job.Name)
	err = os.MkdirAll(cacheDir, 0700)
	if err != nil {
		return err
	}

	cacheBaseName := ""
	for _, cache := range job.Cache {
		log.Printf("checking cache %s", cache)
		if stat, err := os.Stat(filepath.Join(cacheDir, cache)); err == nil && stat.IsDir() {
			cacheBaseName = cache
			break
		}
	}
	jobCacheDir := filepath.Join(jobDir, "cache")
	if cacheBaseName == "" {
		log.Printf("no base cache found")
		err = doExec("btrfs", "subvolume", "create", jobCacheDir)
	} else {
		log.Printf("using base cache %s", cacheBaseName)
		err = doExec("btrfs", "subvolume", "snapshot", filepath.Join(cacheDir, cacheBaseName), jobCacheDir)
	}
	if err != nil {
		return err
	}
	defer func() {
		if _, err := os.Stat(jobCacheDir); err == nil {
			log.Printf("deleting cache %s", jobCacheDir)
			err := doExec("btrfs", "subvolume", "delete", jobCacheDir)
			if err != nil {
				log.Printf("error deleting cache: %v", err)
			}
		}
	}()

	// Setup home dir
	buf := bytes.NewBuffer(nil)
	buf.WriteString("machine github.com\nlogin x-access-token\npassword ")
	buf.WriteString(token)
	err = os.WriteFile(filepath.Join(home, ".netrc"), buf.Bytes(), 0600)
	if err != nil {
		return err
	}

	buf = bytes.NewBuffer(nil)
	buf.WriteString(`
[user]
email = ci@embassy.dev
name = Embassy CI
[init]
defaultBranch = main
[advice]
detachedHead = false
`)
	err = os.WriteFile(filepath.Join(home, ".gitconfig"), buf.Bytes(), 0600)
	if err != nil {
		return err
	}

	j, err := json.Marshal(job)
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(home, "job.json"), j, 0600)
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

	mounts := []specs.Mount{
		{
			Type:        "none",
			Source:      home,
			Destination: "/ci",
			Options:     []string{"rbind"},
		},
		{
			Type:        "none",
			Source:      jobCacheDir,
			Destination: "/ci/cache",
			Options:     []string{"rbind"},
		},
		{
			Type:        "none",
			Source:      artifactsDir,
			Destination: "/ci/artifacts",
			Options:     []string{"rbind"},
		},
	}

	if s.config.NetSandbox != nil {
		mounts = append(mounts, specs.Mount{
			Type:        "none",
			Source:      filepath.Join(s.config.DataDir, "resolv.conf"),
			Destination: "/etc/resolv.conf",
			Options:     []string{"rbind", "ro"},
		})
	} else {
		mounts = append(mounts, specs.Mount{
			Type:        "none",
			Source:      "/etc/resolv.conf",
			Destination: "/etc/resolv.conf",
			Options:     []string{"rbind", "ro"},
		})
	}

	if job.Trusted {
		secretPath := filepath.Join(s.config.DataDir, "secrets", *job.Repo.Owner.Login, *job.Repo.Name)
		err = os.MkdirAll(secretPath, 0700)
		if err != nil {
			return err
		}

		mounts = append(mounts, specs.Mount{
			Type:        "none",
			Source:      secretPath,
			Destination: "/ci/secrets",
			Options:     []string{"rbind"},
		})
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
			oci.WithNamespacedCgroup(),
			oci.WithHostNamespace(specs.NetworkNamespace), // TODO network sandboxing
			oci.WithMounts(mounts),
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

	// start the process inside the container
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

	primary := job.Cache[0]
	log.Printf("committing cache to primary %s", primary)
	primaryPath := filepath.Join(cacheDir, primary)
	if _, err := os.Stat(primaryPath); err == nil {
		err = doExec("btrfs", "subvolume", "delete", primaryPath)
		if err != nil {
			log.Printf("failed to remove old primary cache %s: %v. Trying `rm -rf`", primaryPath, err)
			err = os.RemoveAll(primaryPath)
			if err != nil {
				log.Printf("failed to remove old primary cache %s with `rm -rf`: %v", primaryPath, err)
			}
		}
	}
	err = os.Rename(jobCacheDir, primaryPath)
	if err != nil {
		log.Printf("failed to rename cache %s to %s: %v", jobCacheDir, primaryPath, err)
	}

	err = s.postComment(ctx, job, gh, home)
	if err != nil {
		log.Printf("failed to post github comment: %v", err)
	}

	if err := status.Error(); err != nil {
		return err
	}
	if status.ExitCode() != 0 {
		return errors.Errorf("exited with code %d", status.ExitCode())
	}
	return nil
}

func (s *Service) postComment(ctx context.Context, job *Job, gh *github.Client, home string) error {
	if job.PullRequest == nil {
		return nil
	}

	commentPath := filepath.Join(home, "comment.md")
	stat, err := os.Lstat(commentPath)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	if stat.IsDir() || stat.Mode()&os.ModeSymlink == os.ModeSymlink {
		return nil
	}

	comment, err := os.ReadFile(commentPath)
	if err != nil {
		return err
	}

	// post comment to github
	_, _, err = gh.Issues.CreateComment(ctx, *job.Repo.Owner.Login, *job.Repo.Name, *job.PullRequest.Number, &github.IssueComment{
		Body: github.String(string(comment)),
	})
	if err != nil {
		return err
	}

	return nil
}

// recursively remove all symlinks in a directory
func removeSymlinks(path string) error {
	return filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if info.Mode()&os.ModeSymlink != os.ModeSymlink {
			return nil
		}
		return os.Remove(path)
	})
}

func (s *Service) getRepoToken(ctx context.Context, job *Job) (string, error) {
	var permissions = github.InstallationPermissions{
		Metadata: github.String("read"),
		Contents: github.String("read"),
	}
	var repositories = []string{
		*job.Repo.Name,
	}

	if job.Trusted {
		for key, value := range job.Permissions {
			if value != "read" && value != "write" {
				return "", errors.Errorf("invalid permission %q for %q", value, key)
			}

			switch key {
			case "actions":
				permissions.Actions = github.String(value)
			case "checks":
				permissions.Checks = github.String(value)
			case "contents":
				permissions.Contents = github.String(value)
			case "deployments":
				permissions.Deployments = github.String(value)
			case "issues":
				permissions.Issues = github.String(value)
			case "packages":
				permissions.Packages = github.String(value)
			case "pages":
				permissions.Pages = github.String(value)
			case "pull_requests":
				permissions.PullRequests = github.String(value)
			case "repository_projects":
				permissions.RepositoryProjects = github.String(value)
			case "security_events":
				permissions.SecurityEvents = github.String(value)
			case "statuses":
				permissions.Statuses = github.String(value)
			default:
				return "", errors.Errorf("Unknown permission: %q", key)
			}
		}

		repositories = append(repositories, job.PermissionRepos...)
	}

	itr, err := ghinstallation.New(http.DefaultTransport, s.config.Github.AppID, job.InstallationID, []byte(s.config.Github.PrivateKey))
	itr.InstallationTokenOptions = &github.InstallationTokenOptions{
		Permissions:  &permissions,
		Repositories: repositories,
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
