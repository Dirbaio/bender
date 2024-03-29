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
	DataDir     string            `yaml:"data_dir"`
	ExternalURL string            `yaml:"external_url"`
	ListenPort  int               `yaml:"listen_port"`
	NetSandbox  *NetSandboxConfig `yaml:"net_sandbox"`
	Image       string            `yaml:"image"`
	Github      GithubConfig      `yaml:"github"`
	Cache       CacheConfig       `yaml:"cache"`
}

type CacheConfig struct {
	MinFreeSpaceMB int `yaml:"min_free_space_mb"`
	MaxSizeMB      int `yaml:"max_size_mb"`
}

type NetSandboxConfig struct {
	AllowedDomains []string `yaml:"allowed_domains"`
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

	cgroup Cgroup
}

type Event struct {
	Event      string            `json:"event"`
	Attributes map[string]string `json:"-"`

	Repo           *github.Repository  `json:"repository"`
	PullRequest    *github.PullRequest `json:"pull_request"`
	CloneURL       string              `json:"-"`
	SHA            string              `json:"-"`
	InstallationID int64               `json:"-"`

	// Cache[0] is the primary cache, Cache[1:] are secondary caches
	// that will be cloned into the primary cache if the primary cache
	// does not exist.
	// Example for PR 1234, which targets the foo branch:
	//    "pr-1234", "branch-foo", "branch-main"
	Cache []string `json:"-"`

	// If true, secrets will be mounted.
	Trusted bool `json:"-"`
}

type Job struct {
	*Event
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Script          string            `json:"-"`
	Permissions     map[string]string `json:"-"`
	PermissionRepos []string          `json:"-"`
}

func main() {
	var configFlag = flag.String("c", "config.yaml", "path to config.yaml")
	flag.Parse()

	log.Printf("loading config from %s", *configFlag)
	configData, err := os.ReadFile(*configFlag)
	if err != nil {
		log.Fatal(err)
	}
	config := Config{
		ListenPort: 8000,
		Cache: CacheConfig{
			MinFreeSpaceMB: 20 * 1024, // 20gb
			MaxSizeMB:      40 * 1024, // 40gb
		},
	}
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

	cgroup := initCgroup()

	s := Service{
		config:      config,
		containerd:  cntd,
		runningJobs: make(map[string]struct{}),
		cgroup:      cgroup,
	}

	if s.config.NetSandbox != nil {
		go s.netRun()
	}

	go s.cacheGCRun()

	s.serverRun()
}
