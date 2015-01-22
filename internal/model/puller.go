// Copyright (C) 2014 The Syncthing Authors.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details.
//
// You should have received a copy of the GNU General Public License along
// with this program. If not, see <http://www.gnu.org/licenses/>.

package model

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AudriusButkevicius/lfu-go"

	"github.com/syncthing/protocol"
	"github.com/syncthing/syncthing/internal/config"
	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/events"
	"github.com/syncthing/syncthing/internal/ignore"
	"github.com/syncthing/syncthing/internal/osutil"
	"github.com/syncthing/syncthing/internal/scanner"
	"github.com/syncthing/syncthing/internal/symlinks"
	"github.com/syncthing/syncthing/internal/versioner"
)

// TODO: Stop on errors

const (
	pauseIntv     = 60 * time.Second
	nextPullIntv  = 10 * time.Second
	checkPullIntv = 1 * time.Second
)

// A pullBlockState is passed to the puller routine for each block that needs
// to be fetched.
type pullBlockState struct {
	*sharedPullerState
	block protocol.BlockInfo
}

// A copyBlocksState is passed to copy routine if the file has blocks to be
// copied.
type copyBlocksState struct {
	*sharedPullerState
	blocks []protocol.BlockInfo
}

var (
	activity    = newDeviceActivity()
	errNoDevice = errors.New("no available source device")
)

type Puller struct {
	folder          string
	dir             string
	scanIntv        time.Duration
	model           *Model
	stop            chan struct{}
	versioner       versioner.Versioner
	ignorePerms     bool
	lenientMtimes   bool
	progressEmitter *ProgressEmitter
	copiers         int
	pullers         int
	queue           *jobQueue
}

// Serve will run scans and pulls. It will return when Stop()ed or on a
// critical error.
func (p *Puller) Serve() {
	if debug {
		l.Debugln(p, "starting")
		defer l.Debugln(p, "exiting")
	}

	p.stop = make(chan struct{})

	pullTimer := time.NewTimer(checkPullIntv)
	scanTimer := time.NewTimer(time.Millisecond) // The first scan should be done immediately.

	defer func() {
		pullTimer.Stop()
		scanTimer.Stop()
		// TODO: Should there be an actual FolderStopped state?
		p.model.setState(p.folder, FolderIdle)
	}()

	var prevVer int64
	var prevIgnoreHash string

	// We don't start pulling files until a scan has been completed.
	initialScanCompleted := false

loop:
	for {
		select {
		case <-p.stop:
			return

		// TODO: We could easily add a channel here for notifications from
		// Index(), so that we immediately start a pull when new index
		// information is available. Before that though, I'd like to build a
		// repeatable benchmark of how long it takes to sync a change from
		// device A to device B, so we have something to work against.
		case <-pullTimer.C:
			if !initialScanCompleted {
				// How did we even get here?
				if debug {
					l.Debugln(p, "skip (initial)")
				}
				pullTimer.Reset(nextPullIntv)
				continue
			}

			p.model.fmut.RLock()
			curIgnores := p.model.folderIgnores[p.folder]
			p.model.fmut.RUnlock()

			if newHash := curIgnores.Hash(); newHash != prevIgnoreHash {
				// The ignore patterns have changed. We need to re-evaluate if
				// there are files we need now that were ignored before.
				if debug {
					l.Debugln(p, "ignore patterns have changed, resetting prevVer")
				}
				prevVer = 0
				prevIgnoreHash = newHash
			}

			// RemoteLocalVersion() is a fast call, doesn't touch the database.
			curVer := p.model.RemoteLocalVersion(p.folder)
			if curVer == prevVer {
				if debug {
					l.Debugln(p, "skip (curVer == prevVer)", prevVer)
				}
				pullTimer.Reset(checkPullIntv)
				continue
			}

			if debug {
				l.Debugln(p, "pulling", prevVer, curVer)
			}
			p.model.setState(p.folder, FolderSyncing)
			tries := 0
			for {
				tries++

				changed := p.pullerIteration(curIgnores)
				if debug {
					l.Debugln(p, "changed", changed)
				}

				if changed == 0 {
					// No files were changed by the puller, so we are in
					// sync. Remember the local version number and
					// schedule a resync a little bit into the future.

					if lv := p.model.RemoteLocalVersion(p.folder); lv < curVer {
						// There's a corner case where the device we needed
						// files from disconnected during the puller
						// iteration. The files will have been removed from
						// the index, so we've concluded that we don't need
						// them, but at the same time we have the local
						// version that includes those files in curVer. So we
						// catch the case that localVersion might have
						// decreased here.
						l.Debugln(p, "adjusting curVer", lv)
						curVer = lv
					}
					prevVer = curVer
					if debug {
						l.Debugln(p, "next pull in", nextPullIntv)
					}
					pullTimer.Reset(nextPullIntv)
					break
				}

				if tries > 10 {
					// We've tried a bunch of times to get in sync, but
					// we're not making it. Probably there are write
					// errors preventing us. Flag this with a warning and
					// wait a bit longer before retrying.
					l.Warnf("Folder %q isn't making progress - check logs for possible root cause. Pausing puller for %v.", p.folder, pauseIntv)
					if debug {
						l.Debugln(p, "next pull in", pauseIntv)
					}
					pullTimer.Reset(pauseIntv)
					break
				}
			}
			p.model.setState(p.folder, FolderIdle)

		// The reason for running the scanner from within the puller is that
		// this is the easiest way to make sure we are not doing both at the
		// same time.
		case <-scanTimer.C:
			if debug {
				l.Debugln(p, "rescan")
			}
			p.model.setState(p.folder, FolderScanning)
			if err := p.model.ScanFolder(p.folder); err != nil {
				p.model.cfg.InvalidateFolder(p.folder, err.Error())
				break loop
			}
			p.model.setState(p.folder, FolderIdle)
			if p.scanIntv > 0 {
				// Sleep a random time between 3/4 and 5/4 of the configured interval.
				sleepNanos := (p.scanIntv.Nanoseconds()*3 + rand.Int63n(2*p.scanIntv.Nanoseconds())) / 4
				intv := time.Duration(sleepNanos) * time.Nanosecond

				if debug {
					l.Debugln(p, "next rescan in", intv)
				}
				scanTimer.Reset(intv)
			}
			if !initialScanCompleted {
				l.Infoln("Completed initial scan (rw) of folder", p.folder)
				initialScanCompleted = true
			}
		}
	}
}

func (p *Puller) Stop() {
	close(p.stop)
}

func (p *Puller) String() string {
	return fmt.Sprintf("puller/%s@%p", p.folder, p)
}

// pullerIteration runs a single puller iteration for the given folder and
// returns the number items that should have been synced (even those that
// might have failed). One puller iteration handles all files currently
// flagged as needed in the folder.
func (p *Puller) pullerIteration(ignores *ignore.Matcher) int {
	pullChan := make(chan pullBlockState)
	copyChan := make(chan copyBlocksState)
	finisherChan := make(chan *sharedPullerState)

	var copyWg sync.WaitGroup
	var pullWg sync.WaitGroup
	var doneWg sync.WaitGroup

	if debug {
		l.Debugln(p, "c", p.copiers, "p", p.pullers)
	}

	for i := 0; i < p.copiers; i++ {
		copyWg.Add(1)
		go func() {
			// copierRoutine finishes when copyChan is closed
			p.copierRoutine(copyChan, pullChan, finisherChan)
			copyWg.Done()
		}()
	}

	for i := 0; i < p.pullers; i++ {
		pullWg.Add(1)
		go func() {
			// pullerRoutine finishes when pullChan is closed
			p.pullerRoutine(pullChan, finisherChan)
			pullWg.Done()
		}()
	}

	doneWg.Add(1)
	// finisherRoutine finishes when finisherChan is closed
	go func() {
		p.finisherRoutine(finisherChan)
		doneWg.Done()
	}()

	p.model.fmut.RLock()
	folderFiles := p.model.folderFiles[p.folder]
	p.model.fmut.RUnlock()

	// !!!
	// WithNeed takes a database snapshot (by necessity). By the time we've
	// handled a bunch of files it might have become out of date and we might
	// be attempting to sync with an old version of a file...
	// !!!

	changed := 0

	fileDeletions := map[string]protocol.FileInfo{}
	dirDeletions := []protocol.FileInfo{}
	buckets := map[string][]protocol.FileInfo{}

	folderFiles.WithNeed(protocol.LocalDeviceID, func(intf db.FileIntf) bool {

		// Needed items are delivered sorted lexicographically. This isn't
		// really optimal from a performance point of view - it would be
		// better if files were handled in random order, to spread the load
		// over the cluster. But it means that we can be sure that we fully
		// handle directories before the files that go inside them, which is
		// nice.

		file := intf.(protocol.FileInfo)

		if ignores.Match(file.Name) {
			// This is an ignored file. Skip it, continue iteration.
			return true
		}

		events.Default.Log(events.ItemStarted, map[string]string{
			"folder": p.folder,
			"item":   file.Name,
		})

		if debug {
			l.Debugln(p, "handling", file.Name)
		}

		switch {
		case file.IsDeleted():
			// A deleted file, directory or symlink
			if file.IsDirectory() {
				dirDeletions = append(dirDeletions, file)
			} else {
				fileDeletions[file.Name] = file
				df, ok := p.model.CurrentFolderFile(p.folder, file.Name)
				// Local file can be already deleted, but with a lower version
				// number, hence the deletion coming in again as part of
				// WithNeed
				if ok && !df.IsDeleted() {
					// Put files into buckets per first hash
					key := string(df.Blocks[0].Hash)
					buckets[key] = append(buckets[key], df)
				}
			}
		case file.IsDirectory() && !file.IsSymlink():
			// A new or changed directory
			p.handleDir(file)
		default:
			// A new or changed file or symlink. This is the only case where we
			// do stuff concurrently in the background
			p.queue.Push(file.Name)
		}

		changed++
		return true
	})

nextFile:
	for {
		fileName, ok := p.queue.Pop()
		if !ok {
			break
		}

		f, ok := p.model.CurrentGlobalFile(p.folder, fileName)
		if !ok {
			// File is no longer in the index. Mark it as done and drop it.
			p.queue.Done(fileName)
			continue
		}

		// Local file can be already deleted, but with a lower version
		// number, hence the deletion coming in again as part of
		// WithNeed
		if !f.IsSymlink() && !f.IsDeleted() {
			key := string(f.Blocks[0].Hash)
			for i, candidate := range buckets[key] {
				if scanner.BlocksEqual(candidate.Blocks, f.Blocks) {
					// Remove the candidate from the bucket
					l := len(buckets[key]) - 1
					buckets[key][i] = buckets[key][l]
					buckets[key] = buckets[key][:l]
					// Remove the pending deletion (as we perform it by renaming)
					delete(fileDeletions, candidate.Name)

					p.renameFile(candidate, f)

					p.queue.Done(fileName)
					continue nextFile
				}
			}
		}

		// Not a rename or a symlink, deal with it.
		p.handleFile(f, copyChan, finisherChan)
	}

	// Signal copy and puller routines that we are done with the in data for
	// this iteration. Wait for them to finish.
	close(copyChan)
	copyWg.Wait()
	close(pullChan)
	pullWg.Wait()

	// Signal the finisher chan that there will be no more input.
	close(finisherChan)

	// Wait for the finisherChan to finish.
	doneWg.Wait()

	for _, file := range fileDeletions {
		p.deleteFile(file)
	}

	for i := range dirDeletions {
		p.deleteDir(dirDeletions[len(dirDeletions)-i-1])
	}

	return changed
}

// handleDir creates or updates the given directory
func (p *Puller) handleDir(file protocol.FileInfo) {
	realName := filepath.Join(p.dir, file.Name)
	mode := os.FileMode(file.Flags & 0777)
	if p.ignorePerms {
		mode = 0755
	}

	if debug {
		curFile, _ := p.model.CurrentFolderFile(p.folder, file.Name)
		l.Debugf("need dir\n\t%v\n\t%v", file, curFile)
	}

	info, err := os.Lstat(realName)
	switch {
	// There is already something under that name, but it's a file/link.
	// Most likely a file/link is getting replaced with a directory.
	// Remove the file/link and fall through to directory creation.
	case err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0):
		err = osutil.InWritableDir(os.Remove, realName)
		if err != nil {
			l.Infof("Puller (folder %q, dir %q): %v", p.folder, file.Name, err)
			return
		}
		fallthrough
	// The directory doesn't exist, so we create it with the right
	// mode bits from the start.
	case err != nil && os.IsNotExist(err):
		// We declare a function that acts on only the path name, so
		// we can pass it to InWritableDir. We use a regular Mkdir and
		// not MkdirAll because the parent should already exist.
		mkdir := func(path string) error {
			return os.Mkdir(path, mode)
		}

		if err = osutil.InWritableDir(mkdir, realName); err == nil {
			p.model.updateLocal(p.folder, file)
		} else {
			l.Infof("Puller (folder %q, dir %q): %v", p.folder, file.Name, err)
		}
		return
	// Weird error when stat()'ing the dir. Probably won't work to do
	// anything else with it if we can't even stat() it.
	case err != nil:
		l.Infof("Puller (folder %q, dir %q): %v", p.folder, file.Name, err)
		return
	}

	// The directory already exists, so we just correct the mode bits. (We
	// don't handle modification times on directories, because that sucks...)
	// It's OK to change mode bits on stuff within non-writable directories.

	if p.ignorePerms {
		p.model.updateLocal(p.folder, file)
	} else if err := os.Chmod(realName, mode); err == nil {
		p.model.updateLocal(p.folder, file)
	} else {
		l.Infof("Puller (folder %q, dir %q): %v", p.folder, file.Name, err)
	}
}

// deleteDir attempts to delete the given directory
func (p *Puller) deleteDir(file protocol.FileInfo) {
	realName := filepath.Join(p.dir, file.Name)
	// Delete any temporary files lying around in the directory
	dir, _ := os.Open(realName)
	if dir != nil {
		files, _ := dir.Readdirnames(-1)
		for _, file := range files {
			if defTempNamer.IsTemporary(file) {
				osutil.InWritableDir(os.Remove, filepath.Join(realName, file))
			}
		}
	}
	err := osutil.InWritableDir(os.Remove, realName)
	if err == nil || os.IsNotExist(err) {
		p.model.updateLocal(p.folder, file)
	} else {
		l.Infof("Puller (folder %q, dir %q): delete: %v", p.folder, file.Name, err)
	}
}

// deleteFile attempts to delete the given file
func (p *Puller) deleteFile(file protocol.FileInfo) {
	realName := filepath.Join(p.dir, file.Name)

	var err error
	if p.versioner != nil {
		err = osutil.InWritableDir(p.versioner.Archive, realName)
	} else {
		err = osutil.InWritableDir(os.Remove, realName)
	}

	if err != nil && !os.IsNotExist(err) {
		l.Infof("Puller (folder %q, file %q): delete: %v", p.folder, file.Name, err)
	} else {
		p.model.updateLocal(p.folder, file)
	}
}

// renameFile attempts to rename an existing file to a destination
// and set the right attributes on it.
func (p *Puller) renameFile(source, target protocol.FileInfo) {
	if debug {
		l.Debugln(p, "taking rename shortcut", source.Name, "->", target.Name)
	}

	from := filepath.Join(p.dir, source.Name)
	to := filepath.Join(p.dir, target.Name)

	var err error
	if p.versioner != nil {
		err = osutil.Copy(from, to)
		if err == nil {
			err = osutil.InWritableDir(p.versioner.Archive, from)
		}
	} else {
		err = osutil.TryRename(from, to)
	}

	if err != nil {
		l.Infof("Puller (folder %q, file %q): rename from %q: %v", p.folder, target.Name, source.Name, err)
		return
	}

	// Fix-up the metadata, and update the local index of the target file
	p.shortcutFile(target)

	// Source file already has the delete bit set.
	// Because we got rid of the file (by renaming it), we just need to update
	// the index, and we're done with it.
	p.model.updateLocal(p.folder, source)
}

// handleFile queues the copies and pulls as necessary for a single new or
// changed file.
func (p *Puller) handleFile(file protocol.FileInfo, copyChan chan<- copyBlocksState, finisherChan chan<- *sharedPullerState) {
	curFile, ok := p.model.CurrentFolderFile(p.folder, file.Name)

	if ok && len(curFile.Blocks) == len(file.Blocks) && scanner.BlocksEqual(curFile.Blocks, file.Blocks) {
		// We are supposed to copy the entire file, and then fetch nothing. We
		// are only updating metadata, so we don't actually *need* to make the
		// copy.
		if debug {
			l.Debugln(p, "taking shortcut on", file.Name)
		}
		p.queue.Done(file.Name)
		if file.IsSymlink() {
			p.shortcutSymlink(file)
		} else {
			p.shortcutFile(file)
		}
		return
	}

	scanner.PopulateOffsets(file.Blocks)

	// Figure out the absolute filenames we need once and for all
	tempName := filepath.Join(p.dir, defTempNamer.TempName(file.Name))
	realName := filepath.Join(p.dir, file.Name)

	reused := 0
	var blocks []protocol.BlockInfo

	// Check for an old temporary file which might have some blocks we could
	// reuse.
	tempBlocks, err := scanner.HashFile(tempName, protocol.BlockSize)
	if err == nil {
		// Check for any reusable blocks in the temp file
		tempCopyBlocks, _ := scanner.BlockDiff(tempBlocks, file.Blocks)

		// block.String() returns a string unique to the block
		existingBlocks := make(map[string]bool, len(tempCopyBlocks))
		for _, block := range tempCopyBlocks {
			existingBlocks[block.String()] = true
		}

		// Since the blocks are already there, we don't need to get them.
		for _, block := range file.Blocks {
			_, ok := existingBlocks[block.String()]
			if !ok {
				blocks = append(blocks, block)
			}
		}

		// The sharedpullerstate will know which flags to use when opening the
		// temp file depending if we are reusing any blocks or not.
		reused = len(file.Blocks) - len(blocks)
		if reused == 0 {
			// Otherwise, discard the file ourselves in order for the
			// sharedpuller not to panic when it fails to exlusively create a
			// file which already exists
			os.Remove(tempName)
		}
	} else {
		blocks = file.Blocks
	}

	s := sharedPullerState{
		file:       file,
		folder:     p.folder,
		tempName:   tempName,
		realName:   realName,
		copyTotal:  len(blocks),
		copyNeeded: len(blocks),
		reused:     reused,
	}

	if debug {
		l.Debugf("%v need file %s; copy %d, reused %v", p, file.Name, len(blocks), reused)
	}

	cs := copyBlocksState{
		sharedPullerState: &s,
		blocks:            blocks,
	}
	copyChan <- cs
}

// shortcutFile sets file mode and modification time, when that's the only
// thing that has changed.
func (p *Puller) shortcutFile(file protocol.FileInfo) {
	realName := filepath.Join(p.dir, file.Name)
	if !p.ignorePerms {
		err := os.Chmod(realName, os.FileMode(file.Flags&0777))
		if err != nil {
			l.Infof("Puller (folder %q, file %q): shortcut: %v", p.folder, file.Name, err)
			return
		}
	}

	t := time.Unix(file.Modified, 0)
	err := os.Chtimes(realName, t, t)
	if err != nil {
		if p.lenientMtimes {
			// We accept the failure with a warning here and allow the sync to
			// continue. We'll sync the new mtime back to the other devices later.
			// If they have the same problem & setting, we might never get in
			// sync.
			l.Infof("Puller (folder %q, file %q): shortcut: %v (continuing anyway as requested)", p.folder, file.Name, err)
		} else {
			l.Infof("Puller (folder %q, file %q): shortcut: %v", p.folder, file.Name, err)
			return
		}
	}

	p.model.updateLocal(p.folder, file)
}

// shortcutSymlink changes the symlinks type if necessery.
func (p *Puller) shortcutSymlink(file protocol.FileInfo) {
	err := symlinks.ChangeType(filepath.Join(p.dir, file.Name), file.Flags)
	if err != nil {
		l.Infof("Puller (folder %q, file %q): symlink shortcut: %v", p.folder, file.Name, err)
		return
	}

	p.model.updateLocal(p.folder, file)
}

// copierRoutine reads copierStates until the in channel closes and performs
// the relevant copies when possible, or passes it to the puller routine.
func (p *Puller) copierRoutine(in <-chan copyBlocksState, pullChan chan<- pullBlockState, out chan<- *sharedPullerState) {
	buf := make([]byte, 0, protocol.BlockSize*3)

	for state := range in {
		if p.progressEmitter != nil {
			p.progressEmitter.Register(state.sharedPullerState)
		}

		dstFd, err := state.tempFile()
		if err != nil {
			// Nothing more to do for this failed file (the error was logged
			// when it happened)
			out <- state.sharedPullerState
			continue
		}

		evictionChan := make(chan lfu.Eviction)

		fdCache := lfu.New()
		fdCache.UpperBound = 50
		fdCache.LowerBound = 20
		fdCache.EvictionChannel = evictionChan

		go func() {
			for item := range evictionChan {
				item.Value.(*os.File).Close()
			}
		}()

		folderRoots := make(map[string]string)
		p.model.fmut.RLock()
		for folder, cfg := range p.model.folderCfgs {
			folderRoots[folder] = cfg.Path
		}
		p.model.fmut.RUnlock()

		for _, block := range state.blocks {
			if int(block.Size) <= cap(buf) {
				buf = buf[:int(block.Size)]
			} else {
				buf = make([]byte, int(block.Size))
			}
			found := p.model.finder.Iterate(block.Hash, func(folder, file string, index int32) bool {
				path := filepath.Join(folderRoots[folder], file)

				var fd *os.File

				fdi := fdCache.Get(path)
				if fdi != nil {
					fd = fdi.(*os.File)
				} else {
					fd, err = os.Open(path)
					if err != nil {
						return false
					}
					fdCache.Set(path, fd)
				}

				_, err = fd.ReadAt(buf, protocol.BlockSize*int64(index))
				if err != nil {
					return false
				}

				hash, err := scanner.VerifyBuffer(buf, block)
				if err != nil {
					if hash != nil {
						if debug {
							l.Debugf("Finder block mismatch in %s:%s:%d expected %q got %q", folder, file, index, block.Hash, hash)
						}
						err = p.model.finder.Fix(folder, file, index, block.Hash, hash)
						if err != nil {
							l.Warnln("finder fix:", err)
						}
					} else if debug {
						l.Debugln("Finder failed to verify buffer", err)
					}
					return false
				}

				_, err = dstFd.WriteAt(buf, block.Offset)
				if err != nil {
					state.fail("dst write", err)
				}
				if file == state.file.Name {
					state.copiedFromOrigin()
				}
				return true
			})

			if state.failed() != nil {
				break
			}

			if !found {
				state.pullStarted()
				ps := pullBlockState{
					sharedPullerState: state.sharedPullerState,
					block:             block,
				}
				pullChan <- ps
			} else {
				state.copyDone()
			}
		}
		fdCache.Evict(fdCache.Len())
		close(evictionChan)
		out <- state.sharedPullerState
	}
}

func (p *Puller) pullerRoutine(in <-chan pullBlockState, out chan<- *sharedPullerState) {
	for state := range in {
		if state.failed() != nil {
			continue
		}

		// Get an fd to the temporary file. Tehcnically we don't need it until
		// after fetching the block, but if we run into an error here there is
		// no point in issuing the request to the network.
		fd, err := state.tempFile()
		if err != nil {
			continue
		}

		var lastError error
		potentialDevices := p.model.availability(p.folder, state.file.Name)
		for {
			// Select the least busy device to pull the block from. If we found no
			// feasible device at all, fail the block (and in the long run, the
			// file).
			selected := activity.leastBusy(potentialDevices)
			if selected == (protocol.DeviceID{}) {
				if lastError != nil {
					state.fail("pull", lastError)
				} else {
					state.fail("pull", errNoDevice)
				}
				break
			}

			potentialDevices = removeDevice(potentialDevices, selected)

			// Fetch the block, while marking the selected device as in use so that
			// leastBusy can select another device when someone else asks.
			activity.using(selected)
			buf, lastError := p.model.requestGlobal(selected, p.folder, state.file.Name, state.block.Offset, int(state.block.Size), state.block.Hash)
			activity.done(selected)
			if lastError != nil {
				continue
			}

			// Verify that the received block matches the desired hash, if not
			// try pulling it from another device.
			_, lastError = scanner.VerifyBuffer(buf, state.block)
			if lastError != nil {
				continue
			}

			// Save the block data we got from the cluster
			_, err = fd.WriteAt(buf, state.block.Offset)
			if err != nil {
				state.fail("save", err)
			} else {
				state.pullDone()
			}
			break
		}
		out <- state.sharedPullerState
	}
}

func (p *Puller) performFinish(state *sharedPullerState) {
	var err error
	// Set the correct permission bits on the new file
	if !p.ignorePerms {
		err = os.Chmod(state.tempName, os.FileMode(state.file.Flags&0777))
		if err != nil {
			l.Warnln("puller: final:", err)
			return
		}
	}

	// Set the correct timestamp on the new file
	t := time.Unix(state.file.Modified, 0)
	err = os.Chtimes(state.tempName, t, t)
	if err != nil {
		if p.lenientMtimes {
			// We accept the failure with a warning here and allow the sync to
			// continue. We'll sync the new mtime back to the other devices later.
			// If they have the same problem & setting, we might never get in
			// sync.
			l.Infof("Puller (folder %q, file %q): final: %v (continuing anyway as requested)", p.folder, state.file.Name, err)
		} else {
			l.Warnln("puller: final:", err)
			return
		}
	}

	// If we should use versioning, let the versioner archive the old
	// file before we replace it. Archiving a non-existent file is not
	// an error.
	if p.versioner != nil {
		err = p.versioner.Archive(state.realName)
		if err != nil {
			l.Warnln("puller: final:", err)
			return
		}
	}

	// If the target path is a symlink or a directory, we cannot copy
	// over it, hence remove it before proceeding.
	stat, err := os.Lstat(state.realName)
	if err == nil && (stat.IsDir() || stat.Mode()&os.ModeSymlink != 0) {
		osutil.InWritableDir(os.Remove, state.realName)
	}
	// Replace the original content with the new one
	err = osutil.Rename(state.tempName, state.realName)
	if err != nil {
		l.Warnln("puller: final:", err)
		return
	}

	// If it's a symlink, the target of the symlink is inside the file.
	if state.file.IsSymlink() {
		content, err := ioutil.ReadFile(state.realName)
		if err != nil {
			l.Warnln("puller: final: reading symlink:", err)
			return
		}

		// Remove the file, and replace it with a symlink.
		err = osutil.InWritableDir(func(path string) error {
			os.Remove(path)
			return symlinks.Create(path, string(content), state.file.Flags)
		}, state.realName)
		if err != nil {
			l.Warnln("puller: final: creating symlink:", err)
			return
		}
	}

	// Record the updated file in the index
	p.model.updateLocal(p.folder, state.file)
}

func (p *Puller) finisherRoutine(in <-chan *sharedPullerState) {
	for state := range in {
		if closed, err := state.finalClose(); closed {
			if debug {
				l.Debugln(p, "closing", state.file.Name)
			}
			if err != nil {
				l.Warnln("puller: final:", err)
				continue
			}

			p.queue.Done(state.file.Name)
			if state.failed() == nil {
				p.performFinish(state)
			}
			p.model.receivedFile(p.folder, state.file.Name)
			if p.progressEmitter != nil {
				p.progressEmitter.Deregister(state)
			}
		}
	}
}

// Moves the given filename to the front of the job queue
func (p *Puller) BringToFront(filename string) {
	p.queue.BringToFront(filename)
}

func (p *Puller) Jobs() ([]string, []string) {
	return p.queue.Jobs()
}

func invalidateFolder(cfg *config.Configuration, folderID string, err error) {
	for i := range cfg.Folders {
		folder := &cfg.Folders[i]
		if folder.ID == folderID {
			folder.Invalid = err.Error()
			return
		}
	}
}

func removeDevice(devices []protocol.DeviceID, device protocol.DeviceID) []protocol.DeviceID {
	for i := range devices {
		if devices[i] == device {
			devices[i] = devices[len(devices)-1]
			return devices[:len(devices)-1]
		}
	}
	return devices
}
