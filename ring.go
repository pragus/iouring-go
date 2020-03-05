package iouring

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Ring contains an io_uring submit and completion ring.
type Ring struct {
	fd     int
	p      *Params
	cq     *CompletionQueue
	cqMu   sync.RWMutex
	sq     *SubmitQueue
	sqMu   sync.RWMutex
	sqPool sync.Pool
	idx    *uint64
}

// New is used to create an iouring.Ring.
func New(size uint) (*Ring, error) {
	p := Params{}
	fd, err := Setup(size, &p)
	if err != nil {
		return nil, err
	}
	var (
		cq CompletionQueue
		sq SubmitQueue
	)
	if err := MmapRing(fd, &p, &sq, &cq); err != nil {
		return nil, err
	}
	idx := uint64(0)
	return &Ring{
		p:   &p,
		fd:  fd,
		cq:  &cq,
		sq:  &sq,
		idx: &idx,
	}, nil
}

// Enter is used to enter the ring.
func (r *Ring) Enter(toSubmit uint, minComplete uint, flags uint, sigset *unix.Sigset_t) error {
	if err := Enter(r.fd, toSubmit, minComplete, flags, sigset); err != nil {
		return err
	}
	return nil
}

// Close is used to close the ring.
func (r *Ring) Close() error {
	if err := r.closeSq(); err != nil {
		return err
	}
	if r.p.Flags&FeatSingleMmap == 0 {
		if err := r.closeCq(); err != nil {
			return err
		}
	}
	return syscall.Close(r.fd)
}

func (r *Ring) closeCq() error {
	r.cqMu.Lock()
	defer r.cqMu.Unlock()
	if r.cq == nil {
		return nil
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_MUNMAP,
		r.cq.ptr,
		uintptr(r.cq.Size),
		uintptr(0),
		uintptr(0),
		uintptr(0),
		uintptr(0),
	)
	if errno != 0 {
		err := errno
		return errors.Wrap(err, "failed to munmap cq ring")
	}
	r.cq = nil
	return nil
}

func (r *Ring) closeSq() error {
	r.sqMu.Lock()
	defer r.sqMu.Unlock()
	if r.sq == nil {
		return nil
	}

	_, _, errno := syscall.Syscall6(
		syscall.SYS_MUNMAP,
		r.sq.ptr,
		uintptr(r.sq.Size),
		uintptr(0),
		uintptr(0),
		uintptr(0),
		uintptr(0),
	)
	if errno != 0 {
		err := errno
		return errors.Wrap(err, "failed to munmap sq ring")
	}
	r.sq = nil
	return nil
}

// SubmitHead returns the position of the head of the submit queue. This method
// is safe for calling concurrently.
func (r *Ring) SubmitHead() int {
	return int(atomic.LoadUint32(r.sq.Head))
}

// SubmitTail returns the position of the tail of the submit queue. This method
// is safe for calling concurrently.
func (r *Ring) SubmitTail() int {
	return int(atomic.LoadUint32(r.sq.Tail))
}

// Sqe returns the offset of the next available SQE.
func (r *Ring) Sqe() int {
	v := atomic.AddUint32(r.sq.Head, 1)
	// If the end of the slice is reached then allocate the first postion
	if v == r.sq.Size {
		if !atomic.CompareAndSwapUint32(r.sq.Head, v, 0) {
			return r.Sqe()
		}
		tail := r.SubmitTail()
		// If there's something at the start then enter the ring first.
		if tail != 0 {
			return 0
		}
		_ = r.Enter(uint(1), uint(1), EnterGetEvents, nil)
		return 0 // or r.AllocateSqe()?
	}
	return int(v)
}

// Idx returns an id for a
func (r *Ring) Idx() uint64 {
	return atomic.AddUint64(r.idx, 1)
}

// FileReadWriter returns an io.ReadWriter from an os.File that uses the ring.
func (r *Ring) FileReadWriter(f *os.File) ReadWriteAtCloser {
	return &ringFIO{
		r: r,
		f: f,
	}
}