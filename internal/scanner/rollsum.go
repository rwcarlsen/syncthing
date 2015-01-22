// Package rolling implements a 32 bit rolling checksum similar to rsync's
// algorithm.
package scanner

import (
	"hash"
	"io"
	"log"
	"math"

	"github.com/syncthing/protocol"
)

const charOffset = 31
const window = 64

type Rollsum struct {
	s1, s2  uint32
	window  []byte
	winsize int
	i       int
	target  uint32
	hasher  hash.Hash
	r       io.Reader
	offset  int64
	err     error
	block   protocol.BlockInfo
}

func newRollsumWindow(r io.Reader, window, blocksize int, h hash.Hash) *Rollsum {
	return &Rollsum{
		s1:      uint32(window) * charOffset,
		s2:      uint32(window) * (uint32(window) - 1) * charOffset,
		window:  make([]byte, window),
		winsize: window,
		target:  math.MaxUint32 / uint32(blocksize),
		r:       io.TeeReader(r, h),
		hasher:  h,
	}
}

// NewRollsum ...  blocksize must be a power of 2 up to 2^16.
func NewRollsum(r io.Reader, blocksize int, h hash.Hash) *Rollsum {
	log.Printf("*** created rollsum with blocksize = %v", blocksize)
	return newRollsumWindow(r, window, blocksize, h)
}

func (rs *Rollsum) Next() bool {
	data := make([]byte, 1)
	var size int32
	for {
		n, err := rs.r.Read(data)
		if n == 0 {
			if err == io.EOF {
				// handle last (partial) block
				break
			} else if err != nil {
				return false
			}
		}

		size++
		rs.writeByte(data[0])

		if rs.onSplit() {
			rs.block = protocol.BlockInfo{
				Offset: rs.offset,
				Size:   size,
				Hash:   rs.hasher.Sum(nil),
			}
			rs.hasher.Reset()
			rs.offset += int64(size)
			size = 0
			return true
		}
	}

	if size > 0 {
		rs.block = protocol.BlockInfo{
			Offset: rs.offset,
			Size:   size,
			Hash:   rs.hasher.Sum(nil),
		}
		return true
	}
	return false
}

func (rs *Rollsum) Block() protocol.BlockInfo { return rs.block }

func (rs *Rollsum) Err() error { return rs.err }

func (rs *Rollsum) onSplit() bool {
	//log.Print("**** sum: ", rs.sum())
	return rs.sum() < rs.target
	//return (rs.s2 & (uint32(rs.blocksize) - 1)) == (uint32(rs.blocksize) - 1)
}
func (rs *Rollsum) sum() uint32 { return (rs.s2 << 16) + rs.s1 }

func (rs *Rollsum) writeByte(c byte) {
	drop := rs.window[rs.i]
	rs.s1 = (rs.s1 + uint32(c) - uint32(drop)) % 1 << 16
	rs.s2 = (rs.s2 + rs.s1 - uint32(rs.winsize)*(uint32(drop)+charOffset)) % (1 << 16)

	rs.window[rs.i] = c
	rs.i = (rs.i + 1) % rs.winsize
}
