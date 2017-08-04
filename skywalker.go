//Copyright (c) 2017, Will Dixon. All rights reserved.
//Use of this source code is governed by a BSD-style
//license that can be found in the LICENSE file.

//Package skywalker walks through a filesystem concurrently.
package skywalker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/gobwas/glob"
)

//Worker is anything that knows what to do with a path.
type Worker interface {
	Work(path string)
}

//ListType is used to specify how to handle the contents of a list
type ListType int

const (
	//LTBlacklist is used to specify that a list is to exclude the contents.
	LTBlacklist ListType = iota
	//LTWhitelist is used to specify that a list is to include the contents.
	LTWhitelist
)

//Skywalker can concurrently go through files in Root and call Worker on everything.
//It is recommended to use DirList and ExtList as much as possible as it is more perfomant than List.
type Skywalker struct {
	//Root is where the file walker starts. It is converted to an absolute path before start.
	Root string

	//List and ListType should only be used for fine filtering of paths.
	//It uses https://github.com/gobwas/glob for glob checking on each patch check.
	ListType ListType
	List     []string
	list     []glob.Glob

	//ExtList and ExtListType are used to narrow down the files by their extensions.
	//Make sure to include the preceding ".".
	ExtListType ListType
	ExtList     []string
	extMap      map[string]struct{}

	//DirList and DirListType are used to narrow down by directories.
	//Will skip the appropriate directories and their files/subfolders.
	DirListType ListType
	DirList     []string
	dirMap      map[string]bool

	//NumWorkers are how many workers are listening to the queue to do the work.
	NumWorkers int

	//QueueSize is how many paths to queue up at a time.
	//Useful for fine control over memory usage if needed.
	QueueSize int

	//Worker is the function that is called on each file/directory.
	Worker Worker

	//FilesOnly should be set to true if you only want to queue up files.
	FilesOnly bool
}

//New creates a new Skywalker that can walk through the specified root and calls the Worker on each file and/or directory.
//Defaults Skywalker to have 20 workers, a QueueSize of 100 and only queue files.
func New(root string, worker Worker) *Skywalker {
	return &Skywalker{
		Root:       root,
		NumWorkers: 20,
		QueueSize:  100,
		Worker:     worker,
		FilesOnly:  true,
	}
}

//Walk goes through the files and folders in Root and calls the worker on each.
//Checks the lists specified to check whether it should ignore files or directories.
//It also handles the creation of workers and queues needed for walking.
func (sw *Skywalker) Walk() error {
	if err := sw.init(); err != nil {
		return err
	}
	if _, err := os.Stat(sw.Root); err != nil {
		return err
	}
	workerChan := make(chan string, sw.QueueSize)
	workerWG := new(sync.WaitGroup)
	workerWG.Add(sw.NumWorkers)
	for i := 0; i < sw.NumWorkers; i++ {
		go sw.worker(workerWG, workerChan)
	}
	err := filepath.Walk(sw.Root, sw.walker(workerChan))
	close(workerChan)
	workerWG.Wait()
	return err
}

func (sw *Skywalker) init() error {
	root, err := filepath.Abs(sw.Root)
	if err != nil {
		return err
	}
	sw.Root = root
	dirMap := make(map[string]bool, len(sw.DirList))
	for _, dir := range sw.DirList {
		if sw.DirListType == LTWhitelist {
			dirs := splitPath(dir)
			for i := len(dirs); i > 0; i-- {
				dirMap[filepath.Join(root, filepath.Join(dirs[:i]...))] = i == len(dirs)
			}
		} else {
			dirMap[filepath.Join(root, cleanDir(dir))] = true
		}
	}
	sw.dirMap = dirMap
	extMap := make(map[string]struct{}, len(sw.ExtList))
	for _, ext := range sw.ExtList {
		extMap[ext] = struct{}{}
	}
	sw.extMap = extMap
	list := make([]glob.Glob, len(sw.List))
	for i, g := range sw.List {
		gl, er := glob.Compile(cleanGlob(g), filepath.Separator)
		if er != nil {
			return er
		}
		list[i] = gl
	}
	sw.list = list
	return nil
}

func (sw *Skywalker) worker(workerWG *sync.WaitGroup, workerChan chan string) {
	defer workerWG.Done()
	for w := range workerChan {
		sw.Worker.Work(w)
	}
}

func (sw *Skywalker) walker(workerChan chan string) func(path string, info os.FileInfo, err error) error {
	return func(path string, info os.FileInfo, _ error) error {
		if info.IsDir() {
			if doSomething, err := sw.skipDir(path); doSomething {
				return err
			}
			if sw.FilesOnly {
				return nil
			}
		} else {
			if sw.skipFile(path) {
				return nil
			}
		}
		if sw.matchPath(path) == (sw.ListType == LTBlacklist) {
			return nil
		}
		workerChan <- path
		return nil
	}
}

func (sw *Skywalker) skipDir(path string) (bool, error) {
	switch sw.DirListType {
	case LTBlacklist:
		_, inList := sw.dirMap[path]
		if inList {
			return true, filepath.SkipDir
		}
	case LTWhitelist:
		if path == sw.Root {
			return false, nil
		}
		return sw.whiteListDir(path)
	}
	return false, nil
}

func (sw *Skywalker) whiteListDir(path string) (bool, error) {
	dirs := splitPath(strings.Replace(path, sw.Root, "", 1))
	for i := 1; i < len(dirs)+1; i++ {
		try := filepath.Join(sw.Root, filepath.Join(dirs[:i]...))
		root, found := sw.dirMap[try]
		if found && root {
			return false, nil // if it is the root no need to continue. Just use it
		}
		if !found {
			return true, filepath.SkipDir // if it was not found at all ignore. the order of the search is important
		}
	}
	return true, nil // if it was found but not the root and was the last iteration
}

func (sw *Skywalker) skipFile(path string) bool {
	dir, name := filepath.Split(path)
	if sw.DirListType == LTWhitelist {
		if skipDir, _ := sw.whiteListDir(dir); skipDir {
			return true
		}
	}
	_, inList := sw.extMap[filepath.Ext(name)]
	switch sw.ExtListType {
	case LTBlacklist:
		if inList {
			return true
		}
	case LTWhitelist:
		if !inList {
			return true
		}
	}
	return false
}

func (sw *Skywalker) matchPath(path string) bool {
	path = strings.Replace(path, sw.Root, "", 1)
	for _, gl := range sw.list {
		if match := gl.Match(path); match {
			return true
		}
	}
	return false
}

func cleanDir(dir string) string {
	return filepath.Clean(dir)
}

func cleanGlob(gl string) string {
	if runtime.GOOS == "windows" {
		return strings.Replace(gl, `/`, `\\`, -1) //must escape the backslash for windows comparison
	}
	return gl
}

func splitPath(path string) []string {
	return strings.Split(strings.Trim(cleanDir(path), string(filepath.Separator)), string(filepath.Separator))
}
