// +build darwin dragonfly freebsd netbsd openbsd
// +build !fsnotify

package notify

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// TODO: Close fd on exit.
// TODO: Close kqueue fd on exit.
// TODO: Take into account currently monitored files with those read from dir.
// TODO: Write whole bunch of additional tests (which btw most likely won't
//       pass by default...).

// event is a struct storing reported event's data.
type event struct {
	// dir specifies if event relates to directory.
	dir bool
	// p is a absolute path to file for which event is reported.
	p string
	// e specifies type of a reported event.
	e Event
	// k is `syscall.Kevent_t` instance representing reported event.
	k *syscall.Kevent_t
}

// Event returns type of a reported event.
func (e *event) Event() Event { return e.e }

// IsDir returns a boolean indicating if event is reported for a directory.
func (e *event) IsDir() bool { return e.dir }

// Name returns path to file/directory for which event is reported.
func (e *event) Name() string { return e.p }

// Sys returns platform specific object describing reported event.
// If event generated by internal implementation, it returns nil.
func (e *event) Sys() interface{} { return e.k }

// newWatcher returns `kqueue` Watcher implementation.
func newWatcher() Watcher {
	k := &kqueue{
		idLkp:  make(map[int]*watched, 0),
		pthLkp: make(map[string]*watched, 0),
		c:      make(chan EventInfo),
	}
	return k
}

// monitor reads reported kqueue events and forwards them further after
// performing additional processing. If read event concerns directory,
// it generates Create/Delete event and sent them further instead of directory
// event. This event is detected based on reading contents of analyzed
// directory. If no changes in file list are detected, no event is send further.
// Reading directory structure is less accurate than kqueue and can lead
// to lack of detection of all events.
func (k *kqueue) monitor() {
	for {
		var kevn [1]syscall.Kevent_t
		n, err := syscall.Kevent(*k.fd, nil, kevn[:], nil)
		// ignore failure to capture an event.
		if err != nil {
			continue
		}
		if n > 0 {
			k.Lock()
			w := k.idLkp[int(kevn[0].Ident)]
			if w == nil {
				panic("kqueue: missing config for event")
			}
			if w.dir {
				// If it's dir and delete we have to send it and continue, because
				// other processing relies on opening (in this case not existing) dir.
				if (Event(kevn[0].Fflags) & NOTE_DELETE) != 0 {
					k.c <- &event{w.dir, w.p, Event(kevn[0].Fflags), &kevn[0]}
					delete(k.idLkp, w.fd)
					delete(k.pthLkp, w.p)
					k.Unlock()
					continue
				}
				if err := k.walk(w.p, func(fi os.FileInfo) error {
					p := filepath.Join(w.p, fi.Name())
					if (Event(kevn[0].Fflags) & NOTE_WRITE) != 0 {
						if err := k.watch(p, w.eDir, false, fi.IsDir()); err != nil {
							if err != errNoNewWatch {
								// TODO: pass error via chan because state of monitoring is
								// invalid.
								panic(err)
							}
						} else {
							k.c <- &event{w.dir, p, Create, nil}
						}
					} else {
						k.c <- &event{w.dir, w.p, Event(kevn[0].Fflags), &kevn[0]}
					}
					return nil
				}); err != nil {
					// TODO: pass error via chan because state of monitoring is invalid.
					panic(err)
				}
			} else {
				k.c <- &event{w.dir, w.p, Event(kevn[0].Fflags), &kevn[0]}
			}
			if (Event(kevn[0].Fflags) & NOTE_DELETE) != 0 {
				delete(k.idLkp, w.fd)
				delete(k.pthLkp, w.p)
			}
			k.Unlock()
		}
	}
}

// kqueu is a type holding data for kqueue watcher.
type kqueue struct {
	sync.Mutex
	// fd is a kqueue file descriptor
	fd *int
	// idLkp is a data structure mapping file descriptors with data about watching
	// represented by them files/directories.
	idLkp map[int]*watched
	// pthLkp is a data structure mapping file names with data about watching
	// represented by them files/directories.
	pthLkp map[string]*watched
	// c is a channel used to pass events further.
	c chan EventInfo
}

// watched is a data structure representing wached file/directory.
type watched struct {
	// p is a path to wached file/directory.
	p string
	// fd is a file descriptor for watched file/directory.
	fd int
	// dir is a boolean specifying if wached is directory.
	dir bool
	// eDir represents events watched directly.
	eDir Event
	// eNonDir represents events wached indirectly.
	eNonDir Event
}

// init initializes kqueu if not yet initialized.
func (k *kqueue) init() (err error) {
	if k.fd == nil {
		var fd int
		fd, err = syscall.Kqueue()
		if err != nil {
			return
		}
		k.fd = &fd
		go k.monitor()
	}
	return
}

// Watch implements Watcher interface.
// TODO: Maybe go one more time if called on already watched dir? Or maybe not?
func (k *kqueue) Watch(p string, e Event) error {
	var err error
	if err = k.init(); err != nil {
		return err
	}
	var dir bool
	if dir, err = isdir(p); err != nil {
		return err
	}
	if err = k.watch(p, e, true, dir); err != nil {
		if err == errNoNewWatch {
			return nil
		}
		return err
	}
	if dir {
		if err := k.walk(p, func(fi os.FileInfo) (err error) {
			if !fi.IsDir() {
				if err = k.watch(filepath.Join(p, fi.Name()),
					e, false, false); err != nil {
					if err != errNoNewWatch {
						return
					}
				}
			}
			return
		}); err != nil {
			return err
		}
	}
	return nil
}

var errNoNewWatch = errors.New("kqueue: file already watched")
var errNotWatched = errors.New("kqueue: cannot unwatch not watched file")

// watch starts to watch given `p` file/directory.
func (k *kqueue) watch(p string, e Event, direct, dir bool) error {
	w, ok := k.pthLkp[p]
	if !ok {
		fd, err := syscall.Open(p, syscall.O_NONBLOCK|syscall.O_RDONLY, 0)
		if err != nil {
			return err
		}
		w = &watched{fd: fd, p: p, dir: dir}
	}
	if direct {
		w.eDir |= e
	} else {
		w.eNonDir |= e
	}
	var kevn [1]syscall.Kevent_t
	syscall.SetKevent(&kevn[0], w.fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	kevn[0].Fflags = uint32(w.eDir | w.eNonDir)
	if _, err := syscall.Kevent(*k.fd, kevn[:], nil, nil); err != nil {
		return err
	}
	if !ok {
		k.idLkp[w.fd], k.pthLkp[w.p] = w, w
		return nil
	}
	return errNoNewWatch
}

// unwatch stops watching `p` file/directory.
func (k *kqueue) unwatch(p string, direct bool) (err error) {
	w := k.pthLkp[p]
	if w == nil {
		return errNotWatched
	}
	if direct {
		w.eDir = 0
	} else {
		w.eNonDir = 0
	}
	var kevn [1]syscall.Kevent_t
	syscall.SetKevent(&kevn[0], w.fd, syscall.EVFILT_VNODE, syscall.EV_DELETE)
	if _, err = syscall.Kevent(*k.fd, kevn[:], nil, nil); err != nil {
		return
	}
	if w.eNonDir&w.eDir != 0 {
		if err = k.watch(p, w.eNonDir|w.eDir, w.eNonDir == 0, w.dir); err != nil {
			return
		}
	} else {
		delete(k.idLkp, w.fd)
		delete(k.pthLkp, w.p)
		syscall.Close(w.fd)
	}
	return
}

// walk runs `f` func on each file from `p` directory.
func (k *kqueue) walk(p string, f func(os.FileInfo) error) (err error) {
	var fp *os.File
	if fp, err = os.Open(p); err != nil {
		return
	}
	defer fp.Close()
	var ls []os.FileInfo
	if ls, err = fp.Readdir(-1); err != nil {
		return
	}
	for i := range ls {
		if err = f(ls[i]); err != nil {
			return
		}
	}
	return
}

// Unwatch implements `Watcher` interface.
func (k *kqueue) Unwatch(p string) error {
	k.Lock()
	defer k.Unlock()
	dir, err := isdir(p)
	if err != nil {
		return err
	}
	if dir {
		if err = k.walk(p, func(fi os.FileInfo) error {
			if !fi.IsDir() {
				return k.unwatch(filepath.Join(p, fi.Name()), false)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return k.unwatch(p, true)
}

// isdir returns a boolean indicating if `p` string represents
// path to a directory.
func isdir(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// Fanin implements `Watcher` interface.
func (k *kqueue) Fanin(c chan<- EventInfo, stop <-chan struct{}) {
	go func() {
		for {
			select {
			case ei := <-k.c:
				c <- ei
				// TODO: Stop monitoring after stop. Verify if closing `kqueue`
				// file descriptors triggers stop of `Kevent` call.
			case <-stop:
				return
			}
		}
	}()
}
