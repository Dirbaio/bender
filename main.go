package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd"
	"github.com/google/go-github/v52/github"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DataDir        string       `yaml:"data_dir"`
	ExternalURL    string       `yaml:"external_url"`
	ListenPort     int          `yaml:"listen_port"`
	AllowedDomains []string     `yaml:"allowed_domains"`
	Image          string       `yaml:"image"`
	Github         GithubConfig `yaml:"github"`
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

type Event struct {
	Event      string // "push", "pull_request"
	Attributes map[string]string

	Repo           *github.Repository
	CloneURL       string
	SHA            string
	InstallationID int64

	// Cache[0] is the primary cache, Cache[1:] are secondary caches
	// that will be cloned into the primary cache if the primary cache
	// does not exist.
	// Example for PR 1234, which targets the foo branch:
	//    "pr-1234", "branch-foo", "branch-main"
	Cache []string

	// If true, secrets will be mounted.
	Trusted bool
}

type Job struct {
	*Event
	ID     string
	Name   string
	Script string
}

func main() {
	var configFlag = flag.String("c", "config.yaml", "path to config.yaml")
	flag.Parse()

	log.Printf("loading config from %s", *configFlag)
	configData, err := os.ReadFile(*configFlag)
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

	os.WriteFile(filepath.Join(config.DataDir, "resolv.conf"), []byte("nameserver 127.0.0.93"), 0644)

	cntd, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		log.Fatal(err)
	}

	s := Service{
		config:      config,
		containerd:  cntd,
		runningJobs: make(map[string]struct{}),
	}

	go s.netRun()
	s.serverRun()
}
