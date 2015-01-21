// Package rolling implements a 32 bit rolling checksum similar to rsync's
// algorithm.
package scanner

import (
	"hash"
	"io"
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
}

func NewRollsum(r io.Reader, splitlen int, h hash.Hash) *Rollsum {
	return &Rollsum{
		s1:      uint32(window * charOffset),
		s2:      uint32(window * (window - 1) * charOffset),
		window:  make([]byte, window),
		winsize: window,
		target:  math.MaxUint32 / uint32(splitlen),
		hasher:  h,
		r:       io.TeeReader(r, h),
		hasher:  h,
	}
}

func (rs *Rollsum) Scan() (protocol.BlockInfo, error) {
	data := make([]byte, 1)
	size := 0
	for {
		n, err := rs.r.Read(data)
		if n == 0 {
			if err == io.EOF {
				// handle last (partial) block
				break
			} else if err != nil {
				return nil, err
			}
		}

		size++
		rs.writeByte(c)

		if rs.onSplit() {
			s := protocol.BlockInfo{Offset: rs.offset, Size: size, Hash: h.Sum(nil)}
			rs.h.Reset()
			rs.offset += int64(size)
			size = 0
			return s, nil
		}

	}
}

func (rs *Rollsum) onSplit() bool { return rs.sum() < rs.target }

func (rs *Rollsum) sum() uint32 { return (rs.s1 << 16) | (rs.s2 & 0xffff) }

func (rs *Rollsum) writeByte(ch byte) {
	drop := rs.window[rs.i]
	rs.s1 += uint32(ch) - uint32(drop)
	rs.s2 += rs.s1 - uint32(rs.winsize)*uint32(drop+charOffset)

	rs.window[rs.i] = ch
	rs.i = (rs.i + 1) % rs.winsize
}
