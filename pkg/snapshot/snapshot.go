/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package snapshot

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/GoogleContainerTools/kaniko/pkg/config"
	"github.com/GoogleContainerTools/kaniko/pkg/filesystem"
	"github.com/GoogleContainerTools/kaniko/pkg/timing"

	"github.com/GoogleContainerTools/kaniko/pkg/util"
	"github.com/sirupsen/logrus"
)

// For testing
var snapshotPathPrefix = config.KanikoDir
var allPass = func(s string) (bool, error) { return true, nil }

// Snapshotter holds the root directory from which to take snapshots, and a list of snapshots taken
type Snapshotter struct {
	l          *LayeredMap
	directory  string
	ignorelist []util.IgnoreListEntry
}

// NewSnapshotter creates a new snapshotter rooted at d
func NewSnapshotter(l *LayeredMap, d string) *Snapshotter {
	return &Snapshotter{l: l, directory: d, ignorelist: util.IgnoreList()}
}

// Init initializes a new snapshotter
func (s *Snapshotter) Init() error {
	_, _, err := s.scanFullFilesystem()
	return err
}

// Key returns a string based on the current state of the file system
func (s *Snapshotter) Key() (string, error) {
	return s.l.Key()
}

// TakeSnapshot takes a snapshot of the specified files, avoiding directories in the ignorelist, and creates
// a tarball of the changed files. Return contents of the tarball, and whether or not any files were changed
func (s *Snapshotter) TakeSnapshot(files []string, shdCheckDelete bool) (string, error) {
	f, err := ioutil.TempFile(config.KanikoDir, "")
	if err != nil {
		return "", err
	}
	defer f.Close()

	s.l.Snapshot()
	if len(files) == 0 {
		logrus.Info("No files changed in this command, skipping snapshotting.")
		return "", nil
	}

	filesToAdd, err := filesystem.ResolvePaths(files, s.ignorelist)
	if err != nil {
		return "", nil
	}

	logrus.Info("Taking snapshot of files...")
	logrus.Debugf("Taking snapshot of files %v", filesToAdd)

	sort.Strings(filesToAdd)

	// Add files to the layered map
	for _, file := range filesToAdd {
		if err := s.l.Add(file); err != nil {
			return "", fmt.Errorf("unable to add file %s to layered map: %s", file, err)
		}
	}

	// Get whiteout paths
	filesToWhiteout := []string{}
	if shdCheckDelete {
		existingPaths := s.l.getFlattenedPathsForWhiteOut()
		foundFiles := util.WalkFS(s.directory, allPass)
		for _, file := range foundFiles {
			delete(existingPaths, file)
		}
		// The paths left here are the ones that have been deleted in this layer.
		for path := range existingPaths {
			// Only add the whiteout if the directory for the file still exists.
			dir := filepath.Dir(path)
			if _, ok := existingPaths[dir]; !ok {
				if s.l.MaybeAddWhiteout(path) {
					logrus.Debugf("Adding whiteout for %s", path)
					filesToWhiteout = append(filesToWhiteout, path)
				}
			}
		}
	}
	logrus.Infof("whiteouts %s", filesToWhiteout)
	t := util.NewTar(f)
	defer t.Close()
	if err := writeToTar(t, filesToAdd, filesToWhiteout); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// TakeSnapshotFS takes a snapshot of the filesystem, avoiding directories in the ignorelist, and creates
// a tarball of the changed files.
func (s *Snapshotter) TakeSnapshotFS() (string, error) {
	f, err := ioutil.TempFile(snapshotPathPrefix, "")
	if err != nil {
		return "", err
	}
	defer f.Close()
	t := util.NewTar(f)
	defer t.Close()

	filesToAdd, filesToWhiteOut, err := s.scanFullFilesystem()
	if err != nil {
		return "", err
	}

	if err := writeToTar(t, filesToAdd, filesToWhiteOut); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func (s *Snapshotter) scanFullFilesystem() ([]string, []string, error) {
	logrus.Info("Taking snapshot of full filesystem...")

	// Some of the operations that follow (e.g. hashing) depend on the file system being synced,
	// for example the hashing function that determines if files are equal uses the mtime of the files,
	// which can lag if sync is not called. Unfortunately there can still be lag if too much data needs
	// to be flushed or the disk does its own caching/buffering.
	syscall.Sync()

	s.l.Snapshot()

	foundPaths := util.WalkFS(s.directory, allPass)
	timer := timing.Start("Resolving Paths")
	// First handle whiteouts
	//   Get a list of all the files that existed before this layer
	existingPaths := s.l.getFlattenedPathsForWhiteOut()

	filesToAdd := []string{}
	resolvedMemFs := make(map[string]bool)

	for _, path := range foundPaths {
		delete(existingPaths, path)
		resolvedFiles, err := filesystem.ResolvePaths([]string{path}, s.ignorelist)
		if err != nil {
			return nil, nil, err
		}
		for _, path := range resolvedFiles {
			// Continue if this path is already processed
			if _, ok := resolvedMemFs[path]; ok {
				continue
			}
			if util.CheckIgnoreList(path) {
				logrus.Tracef("Not adding %s to layer, as it's whitelisted", path)
				continue
			}
			// Only add changed files.
			fileChanged, err := s.l.CheckFileChange(path)
			if err != nil {
				return nil, nil, fmt.Errorf("could not check if file has changed %s %s", path, err)
			}
			if fileChanged {
				logrus.Tracef("Adding file %s to layer, because it was changed.", path)
				filesToAdd = append(filesToAdd, path)
			}
		}
	}

	// The paths left here are the ones that have been deleted in this layer.
	filesToWhiteOut := []string{}
	for path := range existingPaths {
		// Only add the whiteout if the directory for the file still exists.
		dir := filepath.Dir(path)
		if _, ok := existingPaths[dir]; !ok {
			if s.l.MaybeAddWhiteout(path) {
				logrus.Debugf("Adding whiteout for %s", path)
				filesToWhiteOut = append(filesToWhiteOut, path)
			}
		}
	}
	timing.DefaultRun.Stop(timer)
	sort.Strings(filesToAdd)
	// Add files to the layered map
	for _, file := range filesToAdd {
		if err := s.l.Add(file); err != nil {
			return nil, nil, fmt.Errorf("unable to add file %s to layered map: %s", file, err)
		}
	}
	return filesToAdd, filesToWhiteOut, nil
}

func writeToTar(t util.Tar, files, whiteouts []string) error {
	timer := timing.Start("Writing tar file")
	defer timing.DefaultRun.Stop(timer)
	// Now create the tar.
	for _, path := range whiteouts {
		if err := t.Whiteout(path); err != nil {
			return err
		}
	}

	addedPaths := make(map[string]bool)
	for _, path := range files {
		if _, fileExists := addedPaths[path]; fileExists {
			continue
		}
		for _, parentPath := range util.ParentDirectories(path) {
			if parentPath == "/" {
				continue
			}
			if _, dirExists := addedPaths[parentPath]; dirExists {
				continue
			}
			if err := t.AddFileToTar(parentPath); err != nil {
				return err
			}
			addedPaths[parentPath] = true
		}
		if err := t.AddFileToTar(path); err != nil {
			return err
		}
		addedPaths[path] = true
	}
	return nil
}

// filesWithLinks returns the symlink and the target path if its exists.
func filesWithLinks(path string) ([]string, error) {
	link, err := util.GetSymLink(path)
	if err == util.ErrNotSymLink {
		return []string{path}, nil
	} else if err != nil {
		return nil, err
	}
	// Add symlink if it exists in the FS
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	if _, err := os.Stat(link); err != nil {
		return []string{path}, nil
	}
	return []string{path, link}, nil
}
