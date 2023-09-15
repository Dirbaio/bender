package main

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func (s *Service) cacheGCRun() {
	for {
		time.Sleep(20 * time.Second)
		s.deleteOldestCache()
	}
}

func (s *Service) deleteOldestCache() {
	cacheDir := filepath.Join(s.config.DataDir, "cache")

	var stat unix.Statfs_t
	unix.Statfs(cacheDir, &stat)
	freeSpaceMB := stat.Bavail * uint64(stat.Bsize) / 1024 / 1024

	if freeSpaceMB > uint64(s.config.Cache.MinFreeSpaceMB) {
		return
	}

	log.Printf("free space %d MB less than minimum of %d MB, deleting one old cache", freeSpaceMB, s.config.Cache.MinFreeSpaceMB)

	var res pathAndTime
	err := oldest(cacheDir, 4, &res)
	if err != nil {
		log.Printf("Failed to find oldest cache: %v", err)
	}

	if res.path == "" {
		log.Println("No cache to delete!?")
		return
	}

	log.Printf("deleting oldest cache: %s", res.path)
	err = doExec("btrfs", "subvolume", "delete", res.path)
	if err != nil {
		log.Printf("Failed to delete oldest cache: %v", err)
	}
}

type pathAndTime struct {
	path string
	time time.Time
}

func oldest(path string, depth int, res *pathAndTime) error {
	if depth == 0 {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if res.path == "" || res.time.After(info.ModTime()) {
			res.path = path
			res.time = info.ModTime()
		}
		return nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	for _, e := range entries {
		err = oldest(filepath.Join(path, e.Name()), depth-1, res)
		if err != nil {
			return err
		}
	}

	return nil
}
