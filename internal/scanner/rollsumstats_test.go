// +build rollsumstats

package scanner

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"
)

const BlockdistFile = "blocksize-dist.dat"
const Blockdist2File = "blocksize-correl-dist.dat"

// creates a data file containing a list of the block sizes. Also generates a
// data file containing a list of:
//
//     (currBlocksize - prevBlocksize) / (currBlocksize)
//
// to allow investigation of correlation between sizes of sequences of blocks.
func TestRollsum_blockdistribution(t *testing.T) {
	blocksize := 1024 * 16
	data := make([]byte, blocksize*1000)
	rand.Seed(2)
	for i := range data {
		data[i] = byte(rand.Int31())
	}

	f, err := os.Create(BlockdistFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	f2, err := os.Create(Blockdist2File)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()

	roller := NewRollsum(bytes.NewBuffer(data), blocksize, sha256.New())
	small := 0
	big := 0
	n := 0
	prevsize := 0
	for roller.Next() {
		n++
		size := int(roller.Block().Size)

		if prevsize != 0 {
			//weight := 1 + math.Abs(float64(blocksize-size)/float64(blocksize))
			weight := 1.0
			fmt.Fprintf(f2, "%v\n", math.Abs(float64(size-prevsize)/float64(size)*weight))
		}
		prevsize = size

		fmt.Fprintf(f, "%v\n", float64(size)/float64(blocksize))
		if size < blocksize/4 {
			small++
		} else if size > blocksize*4 {
			big++
		}
	}
	fmt.Printf("block average is %.1f%% of blocksize\n", float64(len(data)/n)/float64(blocksize)*100)
	frac := float64(small) / float64(n)
	fmt.Printf("%.1f%% of blocks are less than 0.25X blocksize\n", frac*100)
	frac = float64(big) / float64(n)
	fmt.Printf("%.1f%% of blocks are greater than 4X blocksize\n", frac*100)
}

const ChecksumdistFile = "checksum-dist.dat"

// creates a data file to print out a sequences of sums resulting from rolling
// over a many random bytes.  The data can be used to investigate the
// uniformity of the checksum values.
func TestRollsum_checksumdistribution(t *testing.T) {
	blocksize := 1024 * 16
	data := make([]byte, blocksize*1000)
	rand.Seed(2)
	for i := range data {
		data[i] = byte(rand.Int31())
	}

	f, err := os.Create(ChecksumdistFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	roller := NewRollsum(bytes.NewBuffer(data), blocksize, sha256.New())
	for i := 0; i < 10000; i++ {
		roller.writeByte(byte(rand.Int31()))
		fmt.Fprintf(f, "%v\n", float64(roller.sum())/math.MaxUint32)
	}
}
