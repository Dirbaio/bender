package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/davecgh/go-spew/spew"
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

	err = os.RemoveAll(filepath.Join(s.config.DataDir, "jobs", job.ID))
	if err != nil {
		log.Printf("error deleting job homedir: %v", err)
	}
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

	// Create cache dir
	cacheDir := filepath.Join(s.config.DataDir, "cache", *job.Repo.Owner.Login, *job.Repo.Name, job.Name)
	err = os.MkdirAll(cacheDir, 0700)
	if err != nil {
		return err
	}

	// find existing cache
	cacheBaseName := ""
	for _, cache := range job.Cache {
		log.Printf("checking cache %s", cache)
		if stat, err := os.Stat(filepath.Join(cacheDir, cache)); err == nil && stat.IsDir() {
			cacheBaseName = cache
			break
		}
	}
	cacheName := fmt.Sprintf("job-%s", job.ID)
	if cacheBaseName == "" {
		log.Printf("no base cache found")
		err = doExec("btrfs", "subvolume", "create", filepath.Join(cacheDir, cacheName))
	} else {
		log.Printf("using base cache %s", cacheBaseName)
		err = doExec("btrfs", "subvolume", "snapshot", filepath.Join(cacheDir, cacheBaseName), filepath.Join(cacheDir, cacheName))
	}
	if err != nil {
		return err
	}
	doDeleteCache := true
	defer func() {
		if doDeleteCache {
			log.Printf("deleting cache %s", cacheName)
			err := doExec("btrfs", "subvolume", "delete", filepath.Join(cacheDir, cacheName))
			if err != nil {
				log.Printf("error deleting cache: %v", err)
			}
		}
	}()

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

	mounts := []specs.Mount{
		{
			Type:        "none",
			Source:      home,
			Destination: "/ci",
			Options:     []string{"rbind"},
		},
		{
			Type:        "none",
			Source:      filepath.Join(cacheDir, cacheName),
			Destination: "/ci/cache",
			Options:     []string{"rbind"},
		},
		{
			Type:        "none",
			Source:      filepath.Join(s.config.DataDir, "resolv.conf"),
			Destination: "/etc/resolv.conf",
			Options:     []string{"rbind", "ro"},
		},
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

	primary := job.Cache[0]
	log.Printf("committing cache %s to primary %s", cacheName, primary)
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
	err = os.Rename(filepath.Join(cacheDir, cacheName), primaryPath)
	if err != nil {
		log.Printf("failed to rename cache %s to %s: %v", filepath.Join(cacheDir, cacheName), primaryPath, err)
	}
	doDeleteCache = false

	return nil
}
