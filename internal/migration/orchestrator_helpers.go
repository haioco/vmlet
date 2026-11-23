package migration

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"syscall"
	"time"
)

// Linux SEEK_DATA/SEEK_HOLE whence values (not exported by the syscall
// package on every platform we build for).
const (
	seekData = 3
	seekHole = 4
)

// migSnapName builds a namespaced migration snapshot name. The "mig-" prefix
// is what DeleteSnapshots keys off — keep it stable.
func migSnapName(kind string, seq int) string {
	return fmt.Sprintf("mig-%s-%d", kind, seq)
}

// counters tracks byte counters for a migration (progress reporting).
type counters struct {
	disk      int64
	mem       int64
	diskIters int
	memIters  int
	lastDiff  int64
}

// shipDiskFull streams a full zfs send of snap to the peer's disk channel.
func (o *outgoingMigration) shipDiskFull(ctx context.Context, snap string) (int64, error) {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)

	go func() {
		defer pw.Close()
		if err := o.zfs.SendFull(o.vm.ID, snap, pw); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	n, err := o.peer.SendDiskStream(ctx, o.vm.ID, snap, "", pr)
	ioErr := <-errCh
	_ = pr.CloseWithError(ioErr)
	if err != nil {
		return n, err
	}
	if ioErr != nil {
		return n, ioErr
	}
	return n, nil
}

// shipDiskIncremental streams an incremental zfs send (from→to).
func (o *outgoingMigration) shipDiskIncremental(ctx context.Context, from, to string) (int64, error) {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)

	go func() {
		defer pw.Close()
		if err := o.zfs.SendIncremental(o.vm.ID, from, to, pw); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	n, err := o.peer.SendDiskStream(ctx, o.vm.ID, to, from, pr)
	ioErr := <-errCh
	_ = pr.CloseWithError(ioErr)
	if err != nil {
		return n, err
	}
	if ioErr != nil {
		return n, ioErr
	}
	return n, nil
}

// shipFileRange streams the byte range [from, to) of a local file to the
// peer's memory channel (a full ship is the range [0, size)).
func (o *outgoingMigration) shipFileRange(ctx context.Context, path string, from, to int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	rd := io.LimitReader(f, to-from)
	return o.peer.SendMemStream(ctx, o.vm.ID, from, rd)
}

// shipMemExtents streams only the allocated data extents of a (sparse) memory
// snapshot file, each framed with its absolute file offset. Firecracker diff
// snapshots rewrite dirty pages in place, so the target must patch its RAM
// image by offset — appending the tail of the file would corrupt memory.
func (o *outgoingMigration) shipMemExtents(ctx context.Context, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer pw.Close()
		errCh <- writeMemExtents(f, fi.Size(), pw)
	}()

	n, err := o.peer.SendMemExtents(ctx, o.vm.ID, pr)
	ioErr := <-errCh
	_ = pr.CloseWithError(ioErr)
	if err != nil {
		return n, err
	}
	if ioErr != nil {
		return n, ioErr
	}
	return n, nil
}

// writeMemExtents walks the data extents of f (SEEK_DATA/SEEK_HOLE) and frames
// each as [8-byte BE offset][8-byte BE length][payload bytes].
func writeMemExtents(f *os.File, size int64, w io.Writer) error {
	if size <= 0 {
		return nil
	}
	fd := int(f.Fd())
	var hdr [16]byte
	off := int64(0)
	for off < size {
		data, err := syscall.Seek(fd, off, seekData)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				return nil // no more data extents
			}
			if errors.Is(err, syscall.EINVAL) {
				// Filesystem without SEEK_DATA support (e.g. some tmpfs
				// setups): fall back to shipping the whole range.
				return frameExtent(f, 0, size, &hdr, w)
			}
			return fmt.Errorf("seek data at %d: %w", off, err)
		}
		hole, err := syscall.Seek(fd, data, seekHole)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				hole = size
			} else {
				return fmt.Errorf("seek hole at %d: %w", data, err)
			}
		}
		if hole > size {
			hole = size
		}
		if hole <= data {
			return nil
		}
		if err := frameExtent(f, data, hole-data, &hdr, w); err != nil {
			return err
		}
		off = hole
	}
	return nil
}

// frameExtent writes one framed extent using pread (the shared file offset is
// deliberately not used).
func frameExtent(f *os.File, offset, length int64, hdr *[16]byte, w io.Writer) error {
	binary.BigEndian.PutUint64(hdr[0:8], uint64(offset))
	binary.BigEndian.PutUint64(hdr[8:16], uint64(length))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := io.Copy(w, io.NewSectionReader(f, offset, length))
	return err
}

// shipVMState streams the vm_state snapshot file.
func (o *outgoingMigration) shipVMState(ctx context.Context, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return o.peer.SendVMStateStream(ctx, o.vm.ID, f)
}

// logf logs with the migration prefix for traceability in node logs.
func logf(vmID, format string, args ...interface{}) {
	log.Printf("🕊️  [migration:%s] "+format, append([]interface{}{vmID}, args...)...)
}

// restoreCTX is the generous deadline for the snapshot-load call.
func restoreCTX() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Minute)
}

// base64Std encodes bytes for JSON embedding.
func base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
