package scanner

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"testing"

	"github.com/syncthing/protocol"
)

// test that the actual rolling checksum byte for byte matches on all
// identical sections over the window size using inputs that are the same
// except for a small mod+shift in the middle.
func TestRollsum_algo(t *testing.T) {
	data1 := []byte("hello my name is joe and I work in a button factory")
	data2 := []byte("hello my name is joe and I eat in a button factory")
	window := 8
	h := sha256.New()

	rs := newRollsumWindow(bytes.NewBuffer([]byte{}), window, 8, h)
	sums1 := []uint32{}
	for _, c := range data1 {
		rs.writeByte(c)
		sums1 = append(sums1, rs.sum())
	}

	rs = newRollsumWindow(bytes.NewBuffer([]byte{}), window, 8, h)
	sums2 := []uint32{}
	for _, c := range data2 {
		rs.writeByte(c)
		sums2 = append(sums2, rs.sum())
	}

	for i := 0; i < 27; i++ {
		if sums1[i] != sums2[i] {
			t.Errorf("block %v pre-sums don't match: %v != %v", i, sums1[i], sums2[i])
		}
	}

	for i := 1; i < 14; i++ {
		i1 := len(sums1) - i
		i2 := len(sums2) - i
		if sums1[i1] != sums2[i2] {
			t.Errorf("block %v post-sums don't match: %v != %v", i-1, sums1[i1], sums2[i2])
		}
	}
}

// this is effectively a regression test to make sure we don't break/change
// the split points.
func TestRollsum_SplitPoints(t *testing.T) {
	blocksize := 1024 * 64 // 65k
	data := make([]byte, blocksize*10)
	rand.Seed(1)
	for i := range data {
		data[i] = byte(rand.Int31())
	}

	expectSize := []int32{
		38718,
		185906,
		61635,
		37853,
		9415,
		71047,
		43394,
		52601,
		74123,
		5543,
		23490,
		4239,
		8368,
		39028,
	}

	roller := NewRollsum(bytes.NewBuffer(data), blocksize, sha256.New())
	blocks := []protocol.BlockInfo{}
	for roller.Next() {
		blocks = append(blocks, roller.Block())
	}
	if err := roller.Err(); err != nil {
		t.Fatal(err)
	}

	t.Logf("split into %v blocks", len(blocks))
	t.Logf("avg block size = %v bytes", len(data)/len(blocks))

	for i, b := range blocks {
		t.Logf("block %v: size=%v, offset=%v, sha256=%x", i, b.Size, b.Offset, b.Hash)
		if b.Size != expectSize[i] {
			t.Errorf("    wrong length: expected %v, got %v", expectSize[i], b.Size)
		}
	}
}
