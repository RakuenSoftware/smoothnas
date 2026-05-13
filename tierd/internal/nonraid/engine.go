package nonraid

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	MaxParityDevices = 3
	MaxDataDevices   = 255
)

// BlockDevice is the small random-access surface the nonRaid engine needs from
// real block devices, sparse files used by tests, and NBD exports.
type BlockDevice interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// Engine keeps parity in sync for one nonRaid array. Data devices are
// individually formatted filesystems; parity devices are raw block devices.
type Engine struct {
	data       []BlockDevice
	parity     []BlockDevice
	dataSizes  []int64
	paritySize int64
	locks      [256]sync.Mutex
}

// NewEngine returns a parity engine over already-open data and parity devices.
// dataSizes are the exposed sizes of each data disk. paritySize is the common
// usable parity size, normally the smallest parity device size.
func NewEngine(data []BlockDevice, dataSizes []int64, parity []BlockDevice, paritySize int64) (*Engine, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("nonRaid requires at least one data device")
	}
	if len(data) > MaxDataDevices {
		return nil, fmt.Errorf("nonRaid supports at most %d data devices with triple parity", MaxDataDevices)
	}
	if len(parity) == 0 || len(parity) > MaxParityDevices {
		return nil, fmt.Errorf("nonRaid requires between 1 and %d parity devices", MaxParityDevices)
	}
	if len(dataSizes) != len(data) {
		return nil, fmt.Errorf("data size count does not match data device count")
	}
	if paritySize <= 0 {
		return nil, fmt.Errorf("parity size must be positive")
	}
	for i, size := range dataSizes {
		if size <= 0 {
			return nil, fmt.Errorf("data device %d size must be positive", i+1)
		}
		if size > paritySize {
			return nil, fmt.Errorf("data device %d is larger than smallest parity device", i+1)
		}
	}
	return &Engine{
		data:       append([]BlockDevice(nil), data...),
		parity:     append([]BlockDevice(nil), parity...),
		dataSizes:  append([]int64(nil), dataSizes...),
		paritySize: paritySize,
	}, nil
}

func (e *Engine) DataCount() int { return len(e.data) }

func (e *Engine) ParityCount() int { return len(e.parity) }

func (e *Engine) DataSize(slot int) (int64, error) {
	if slot < 0 || slot >= len(e.dataSizes) {
		return 0, fmt.Errorf("data slot %d out of range", slot+1)
	}
	return e.dataSizes[slot], nil
}

func (e *Engine) stripeIndexes(offset int64, length int) (first, last int) {
	const stripeSize = int64(1 << 20)
	if length <= 0 {
		idx := int((offset / stripeSize) % int64(len(e.locks)))
		return idx, idx
	}
	first = int((offset / stripeSize) % int64(len(e.locks)))
	last = int(((offset + int64(length) - 1) / stripeSize) % int64(len(e.locks)))
	return first, last
}

func (e *Engine) lockRange(offset int64, length int) func() {
	first, last := e.stripeIndexes(offset, length)
	if first <= last {
		for i := first; i <= last; i++ {
			e.locks[i].Lock()
		}
		return func() {
			for i := last; i >= first; i-- {
				e.locks[i].Unlock()
			}
		}
	}
	for i := first; i < len(e.locks); i++ {
		e.locks[i].Lock()
	}
	for i := 0; i <= last; i++ {
		e.locks[i].Lock()
	}
	return func() {
		for i := last; i >= 0; i-- {
			e.locks[i].Unlock()
		}
		for i := len(e.locks) - 1; i >= first; i-- {
			e.locks[i].Unlock()
		}
	}
}

func validateRange(size, offset int64, length int) error {
	if offset < 0 {
		return fmt.Errorf("negative offset")
	}
	if length < 0 {
		return fmt.Errorf("negative length")
	}
	if offset+int64(length) > size {
		return io.ErrShortWrite
	}
	return nil
}

func readAtZero(dev io.ReaderAt, buf []byte, offset int64) error {
	done := 0
	for done < len(buf) {
		n, err := dev.ReadAt(buf[done:], offset+int64(done))
		done += n
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			for i := done; i < len(buf); i++ {
				buf[i] = 0
			}
			return nil
		}
		return err
	}
	return nil
}

func writeFullAt(dev io.WriterAt, buf []byte, offset int64) error {
	done := 0
	for done < len(buf) {
		n, err := dev.WriteAt(buf[done:], offset+int64(done))
		done += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// ReadData reads the data device for a healthy slot. Degraded reads should use
// ReconstructData with the missing slot list.
func (e *Engine) ReadData(slot int, offset int64, dst []byte) error {
	if slot < 0 || slot >= len(e.data) {
		return fmt.Errorf("data slot %d out of range", slot+1)
	}
	if err := validateRange(e.dataSizes[slot], offset, len(dst)); err != nil {
		return err
	}
	return readAtZero(e.data[slot], dst, offset)
}

// WriteData updates a data disk and all parity disks using the normal
// read/modify/write parity path.
func (e *Engine) WriteData(slot int, offset int64, src []byte) error {
	if slot < 0 || slot >= len(e.data) {
		return fmt.Errorf("data slot %d out of range", slot+1)
	}
	if len(src) == 0 {
		return nil
	}
	if err := validateRange(e.dataSizes[slot], offset, len(src)); err != nil {
		return err
	}
	unlock := e.lockRange(offset, len(src))
	defer unlock()

	old := make([]byte, len(src))
	if err := readAtZero(e.data[slot], old, offset); err != nil {
		return fmt.Errorf("read old data: %w", err)
	}

	for pidx, pdev := range e.parity {
		pbuf := make([]byte, len(src))
		if err := readAtZero(pdev, pbuf, offset); err != nil {
			return fmt.Errorf("read parity %d: %w", pidx+1, err)
		}
		coef := parityCoefficient(pidx, slot)
		for i := range src {
			delta := old[i] ^ src[i]
			if delta != 0 {
				pbuf[i] ^= gfMul(coef, delta)
			}
		}
		if err := writeFullAt(pdev, pbuf, offset); err != nil {
			return fmt.Errorf("write parity %d: %w", pidx+1, err)
		}
	}

	if err := writeFullAt(e.data[slot], src, offset); err != nil {
		return fmt.Errorf("write data %d: %w", slot+1, err)
	}
	return nil
}

// ZeroData writes zeros through the parity path. It is used for discard/TRIM
// handling and for deterministic destructive initialization.
func (e *Engine) ZeroData(slot int, offset int64, length int) error {
	const chunkSize = 1 << 20
	zeroes := make([]byte, chunkSize)
	for done := 0; done < length; {
		n := length - done
		if n > len(zeroes) {
			n = len(zeroes)
		}
		if err := e.WriteData(slot, offset+int64(done), zeroes[:n]); err != nil {
			return err
		}
		done += n
	}
	return nil
}

// Sync flushes every member device.
func (e *Engine) Sync() error {
	for i, dev := range e.data {
		if err := dev.Sync(); err != nil {
			return fmt.Errorf("sync data %d: %w", i+1, err)
		}
	}
	for i, dev := range e.parity {
		if err := dev.Sync(); err != nil {
			return fmt.Errorf("sync parity %d: %w", i+1, err)
		}
	}
	return nil
}

// BuildParity rewrites all parity devices from the current data devices. It is
// used when importing an existing disk set or after a destructive array create
// has zeroed all members.
func (e *Engine) BuildParity(chunkSize int) error {
	if chunkSize <= 0 {
		chunkSize = 4 << 20
	}
	bufs := make([][]byte, len(e.data))
	for i := range bufs {
		bufs[i] = make([]byte, chunkSize)
	}
	parityBufs := make([][]byte, len(e.parity))
	for i := range parityBufs {
		parityBufs[i] = make([]byte, chunkSize)
	}

	for off := int64(0); off < e.paritySize; off += int64(chunkSize) {
		n := chunkSize
		if remain := e.paritySize - off; remain < int64(n) {
			n = int(remain)
		}
		for p := range parityBufs {
			clear(parityBufs[p][:n])
		}
		for d, dev := range e.data {
			clear(bufs[d][:n])
			if off < e.dataSizes[d] {
				readLen := n
				if remain := e.dataSizes[d] - off; remain < int64(readLen) {
					readLen = int(remain)
				}
				if err := readAtZero(dev, bufs[d][:readLen], off); err != nil {
					return fmt.Errorf("read data %d: %w", d+1, err)
				}
			}
			for p := range parityBufs {
				coef := parityCoefficient(p, d)
				for i := 0; i < n; i++ {
					parityBufs[p][i] ^= gfMul(coef, bufs[d][i])
				}
			}
		}
		for p, dev := range e.parity {
			if err := writeFullAt(dev, parityBufs[p][:n], off); err != nil {
				return fmt.Errorf("write parity %d: %w", p+1, err)
			}
		}
	}
	return e.Sync()
}

// ReconstructData reconstructs one missing data slot from the remaining data
// slots and the first len(missingSlots) parity devices. Up to three missing
// slots can be supplied, matching the configured parity count.
func (e *Engine) ReconstructData(slot int, missingSlots []int, offset int64, length int) ([]byte, error) {
	if slot < 0 || slot >= len(e.data) {
		return nil, fmt.Errorf("data slot %d out of range", slot+1)
	}
	if err := validateRange(e.dataSizes[slot], offset, length); err != nil {
		return nil, err
	}
	missingIndex := -1
	seen := map[int]struct{}{}
	for i, missing := range missingSlots {
		if missing < 0 || missing >= len(e.data) {
			return nil, fmt.Errorf("missing data slot %d out of range", missing+1)
		}
		if _, ok := seen[missing]; ok {
			return nil, fmt.Errorf("missing data slot %d listed more than once", missing+1)
		}
		seen[missing] = struct{}{}
		if missing == slot {
			missingIndex = i
		}
	}
	if missingIndex < 0 {
		out := make([]byte, length)
		return out, e.ReadData(slot, offset, out)
	}
	if len(missingSlots) > len(e.parity) {
		return nil, fmt.Errorf("%d missing data disks exceeds %d parity disks", len(missingSlots), len(e.parity))
	}

	matrix := make([][]byte, len(missingSlots))
	for p := range matrix {
		matrix[p] = make([]byte, len(missingSlots))
		for j, missing := range missingSlots {
			matrix[p][j] = parityCoefficient(p, missing)
		}
	}
	inv, err := invertGFMatrix(matrix)
	if err != nil {
		return nil, err
	}

	rhs := make([][]byte, len(missingSlots))
	for p := range rhs {
		rhs[p] = make([]byte, length)
		if err := readAtZero(e.parity[p], rhs[p], offset); err != nil {
			return nil, fmt.Errorf("read parity %d: %w", p+1, err)
		}
	}
	known := make([]byte, length)
	for d, dev := range e.data {
		if _, miss := seen[d]; miss {
			continue
		}
		clear(known)
		if offEnd := offset + int64(length); offset < e.dataSizes[d] && offEnd > 0 {
			readLen := length
			if remain := e.dataSizes[d] - offset; remain < int64(readLen) {
				readLen = int(remain)
			}
			if err := readAtZero(dev, known[:readLen], offset); err != nil {
				return nil, fmt.Errorf("read data %d: %w", d+1, err)
			}
		}
		for p := range rhs {
			coef := parityCoefficient(p, d)
			for i := 0; i < length; i++ {
				rhs[p][i] ^= gfMul(coef, known[i])
			}
		}
	}

	out := make([]byte, length)
	for eq := range rhs {
		coef := inv[missingIndex][eq]
		if coef == 0 {
			continue
		}
		for i := 0; i < length; i++ {
			out[i] ^= gfMul(coef, rhs[eq][i])
		}
	}
	return out, nil
}
