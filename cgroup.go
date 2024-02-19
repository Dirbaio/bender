package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Cgroup struct {
	mountpoint string
	root       string
	bender     string
	jobs       string
}

func initCgroup() Cgroup {
	mountpoint := "/sys/fs/cgroup"
	rootBytes, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		panic(err)
	}
	root := string(rootBytes)
	root = strings.TrimSpace(strings.TrimPrefix(root, "0::"))

	cg := Cgroup{
		mountpoint: mountpoint,
		root:       root,
		bender:     filepath.Join(root, "bender"),
		jobs:       filepath.Join(root, "jobs"),
	}

	// create sub-cgroups
	err = os.Mkdir(filepath.Join(cg.mountpoint, cg.bender), 0777)
	if err != nil && !os.IsExist(err) {
		panic(err)
	}
	err = os.Mkdir(filepath.Join(cg.mountpoint, cg.jobs), 0777)
	if err != nil && !os.IsExist(err) {
		panic(err)
	}

	// move ourselves to the bender cgroup.
	err = os.WriteFile(filepath.Join(cg.mountpoint, cg.bender, "cgroup.procs"), []byte(fmt.Sprint(os.Getpid())), 0777)
	if err != nil {
		panic(err)
	}

	return cg
}
