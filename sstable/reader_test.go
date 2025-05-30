// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/cockroachdb/pebble/internal/errorfs"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
)

// get is a testing helper that simulates a read and helps verify bloom filters
// until they are available through iterators.
func (r *Reader) get(key []byte) (value []byte, err error) {
	if r.err != nil {
		return nil, r.err
	}

	if r.tableFilter != nil {
		dataH, err := r.readFilter()
		if err != nil {
			return nil, err
		}
		var lookupKey []byte
		if r.Split != nil {
			lookupKey = key[:r.Split(key)]
		} else {
			lookupKey = key
		}
		mayContain := r.tableFilter.mayContain(dataH.Get(), lookupKey)
		dataH.Release()
		if !mayContain {
			return nil, base.ErrNotFound
		}
	}

	i, err := r.NewIter(nil /* lower */, nil /* upper */)
	if err != nil {
		return nil, err
	}
	ikey, value := i.SeekGE(key, base.SeekGEFlagsNone)

	if ikey == nil || r.Compare(key, ikey.UserKey) != 0 {
		err := i.Close()
		if err == nil {
			err = base.ErrNotFound
		}
		return nil, err
	}

	// The value will be "freed" when the iterator is closed, so make a copy
	// which will outlast the lifetime of the iterator.
	newValue := make([]byte, len(value))
	copy(newValue, value)
	if err := i.Close(); err != nil {
		return nil, err
	}
	return newValue, nil
}

// iterAdapter adapts the new Iterator API which returns the key and value from
// positioning methods (Seek*, First, Last, Next, Prev) to the old API which
// returned a boolean corresponding to Valid. Only used by test code.
type iterAdapter struct {
	Iterator
	key *InternalKey
	val []byte
}

func newIterAdapter(iter Iterator) *iterAdapter {
	return &iterAdapter{
		Iterator: iter,
	}
}

func (i *iterAdapter) update(key *InternalKey, val []byte) bool {
	i.key = key
	i.val = val
	return i.key != nil
}

func (i *iterAdapter) String() string {
	return "iter-adapter"
}

func (i *iterAdapter) SeekGE(key []byte, flags base.SeekGEFlags) bool {
	return i.update(i.Iterator.SeekGE(key, flags))
}

func (i *iterAdapter) SeekPrefixGE(prefix, key []byte, flags base.SeekGEFlags) bool {
	return i.update(i.Iterator.SeekPrefixGE(prefix, key, flags))
}

func (i *iterAdapter) SeekLT(key []byte, flags base.SeekLTFlags) bool {
	return i.update(i.Iterator.SeekLT(key, flags))
}

func (i *iterAdapter) First() bool {
	return i.update(i.Iterator.First())
}

func (i *iterAdapter) Last() bool {
	return i.update(i.Iterator.Last())
}

func (i *iterAdapter) Next() bool {
	return i.update(i.Iterator.Next())
}

func (i *iterAdapter) NextIgnoreResult() {
	i.Iterator.Next()
	i.update(nil, nil)
}

func (i *iterAdapter) Prev() bool {
	return i.update(i.Iterator.Prev())
}

func (i *iterAdapter) Key() *InternalKey {
	return i.key
}

func (i *iterAdapter) Value() []byte {
	return i.val
}

func (i *iterAdapter) Valid() bool {
	return i.key != nil
}

func (i *iterAdapter) SetBounds(lower, upper []byte) {
	i.Iterator.SetBounds(lower, upper)
	i.key = nil
}

func TestReader(t *testing.T) {
	writerOpts := map[string]WriterOptions{
		// No bloom filters.
		"default": {},
		"bloom10bit": {
			// The standard policy.
			FilterPolicy: bloom.FilterPolicy(10),
			FilterType:   base.TableFilter,
		},
		"bloom1bit": {
			// A policy with many false positives.
			FilterPolicy: bloom.FilterPolicy(1),
			FilterType:   base.TableFilter,
		},
		"bloom100bit": {
			// A policy unlikely to have false positives.
			FilterPolicy: bloom.FilterPolicy(100),
			FilterType:   base.TableFilter,
		},
	}

	blockSizes := map[string]int{
		"1bytes":   1,
		"5bytes":   5,
		"10bytes":  10,
		"25bytes":  25,
		"Maxbytes": math.MaxInt32,
	}

	opts := map[string]*Comparer{
		"default":      nil,
		"prefixFilter": fixtureComparer,
	}

	testDirs := map[string]string{
		"default":      "testdata/reader",
		"prefixFilter": "testdata/prefixreader",
	}

	for dName, blockSize := range blockSizes {
		for iName, indexBlockSize := range blockSizes {
			for lName, tableOpt := range writerOpts {
				for oName, cmp := range opts {
					tableOpt.BlockSize = blockSize
					tableOpt.Comparer = cmp
					tableOpt.IndexBlockSize = indexBlockSize

					t.Run(
						fmt.Sprintf("opts=%s,writerOpts=%s,blockSize=%s,indexSize=%s",
							oName, lName, dName, iName),
						func(t *testing.T) {
							runTestReader(
								t, tableOpt, testDirs[oName], nil /* Reader */, 0)
						})
				}
			}
		}
	}
}

func TestHamletReader(t *testing.T) {
	prebuiltSSTs := []string{
		"testdata/h.ldb",
		"testdata/h.sst",
		"testdata/h.no-compression.sst",
		"testdata/h.no-compression.two_level_index.sst",
		"testdata/h.block-bloom.no-compression.sst",
		"testdata/h.table-bloom.no-compression.prefix_extractor.no_whole_key_filter.sst",
		"testdata/h.table-bloom.no-compression.sst",
	}

	for _, prebuiltSST := range prebuiltSSTs {
		f, err := os.Open(filepath.FromSlash(prebuiltSST))
		require.NoError(t, err)

		r, err := NewReader(f, ReaderOptions{})
		require.NoError(t, err)

		t.Run(
			fmt.Sprintf("sst=%s", prebuiltSST),
			func(t *testing.T) { runTestReader(t, WriterOptions{}, "testdata/hamletreader", r, 0) },
		)
	}
}

func TestReaderStats(t *testing.T) {
	tableOpt := WriterOptions{
		BlockSize:      30,
		IndexBlockSize: 30,
	}
	runTestReader(t, tableOpt, "testdata/readerstats", nil, 10000)
}

func TestInjectedErrors(t *testing.T) {
	prebuiltSSTs := []string{
		"testdata/h.ldb",
		"testdata/h.sst",
		"testdata/h.no-compression.sst",
		"testdata/h.no-compression.two_level_index.sst",
		"testdata/h.block-bloom.no-compression.sst",
		"testdata/h.table-bloom.no-compression.prefix_extractor.no_whole_key_filter.sst",
		"testdata/h.table-bloom.no-compression.sst",
	}

	for _, prebuiltSST := range prebuiltSSTs {
		run := func(i int) (reterr error) {
			f, err := os.Open(filepath.FromSlash(prebuiltSST))
			require.NoError(t, err)
			r, err := NewReader(errorfs.WrapFile(f, errorfs.OnIndex(int32(i))), ReaderOptions{})
			if err != nil {
				return firstError(err, f.Close())
			}
			defer func() { reterr = firstError(reterr, r.Close()) }()

			_, err = r.EstimateDiskUsage([]byte("borrower"), []byte("lender"))
			if err != nil {
				return err
			}

			iter, err := r.NewIter(nil, nil)
			if err != nil {
				return err
			}
			defer func() { reterr = firstError(reterr, iter.Close()) }()
			for k, v := iter.First(); k != nil && v != nil; k, v = iter.Next() {
			}
			if err = iter.Error(); err != nil {
				return err
			}
			return nil
		}
		for i := 0; ; i++ {
			err := run(i)
			if errors.Is(err, errorfs.ErrInjected) {
				t.Logf("%q, index %d: %s", prebuiltSST, i, err)
				continue
			}
			if err != nil {
				t.Errorf("%q, index %d: non-injected error: %+v", prebuiltSST, i, err)
				break
			}
			t.Logf("%q: no error at index %d", prebuiltSST, i)
			break
		}
	}
}

func TestInvalidReader(t *testing.T) {
	testCases := []struct {
		file     vfs.File
		expected string
	}{
		{nil, "nil file"},
		{vfs.NewMemFile([]byte("invalid sst bytes")), "invalid table"},
	}
	for _, tc := range testCases {
		r, err := NewReader(tc.file, ReaderOptions{})
		if !strings.Contains(err.Error(), tc.expected) {
			t.Fatalf("expected %q, but found %q", tc.expected, err.Error())
		}
		if r != nil {
			t.Fatalf("found non-nil reader returned with non-nil error %q", err.Error())
		}
	}
}

func runTestReader(t *testing.T, o WriterOptions, dir string, r *Reader, cacheSize int) {
	datadriven.Walk(t, dir, func(t *testing.T, path string) {
		defer func() {
			if r != nil {
				r.Close()
				r = nil
			}
		}()

		datadriven.RunTest(t, path, func(d *datadriven.TestData) string {
			switch d.Cmd {
			case "build":
				if r != nil {
					r.Close()
					r = nil
				}
				var err error
				_, r, err = runBuildCmd(d, &o, cacheSize)
				if err != nil {
					return err.Error()
				}
				return ""

			case "iter":
				seqNum, err := scanGlobalSeqNum(d)
				if err != nil {
					return err.Error()
				}
				var stats base.InternalIteratorStats
				r.Properties.GlobalSeqNum = seqNum
				iter, err := r.NewIterWithBlockPropertyFilters(
					nil,  /* lower */
					nil,  /* upper */
					nil,  /* filterer */
					true, /* use filter block */
					&stats,
				)
				if err != nil {
					return err.Error()
				}
				return runIterCmd(d, iter, runIterCmdStats(&stats))

			case "get":
				var b bytes.Buffer
				for _, k := range strings.Split(d.Input, "\n") {
					v, err := r.get([]byte(k))
					if err != nil {
						fmt.Fprintf(&b, "<err: %s>\n", err)
					} else {
						fmt.Fprintln(&b, string(v))
					}
				}
				return b.String()
			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})
	})
}

func TestReaderCheckComparerMerger(t *testing.T) {
	const testTable = "test"

	testComparer := &base.Comparer{
		Name:      "test.comparer",
		Compare:   base.DefaultComparer.Compare,
		Equal:     base.DefaultComparer.Equal,
		Separator: base.DefaultComparer.Separator,
		Successor: base.DefaultComparer.Successor,
	}
	testMerger := &base.Merger{
		Name:  "test.merger",
		Merge: base.DefaultMerger.Merge,
	}
	writerOpts := WriterOptions{
		Comparer:   testComparer,
		MergerName: "test.merger",
	}

	mem := vfs.NewMem()
	f0, err := mem.Create(testTable)
	require.NoError(t, err)

	w := NewWriter(f0, writerOpts)
	require.NoError(t, w.Set([]byte("test"), nil))
	require.NoError(t, w.Close())

	testCases := []struct {
		comparers []*base.Comparer
		mergers   []*base.Merger
		expected  string
	}{
		{
			[]*base.Comparer{testComparer},
			[]*base.Merger{testMerger},
			"",
		},
		{
			[]*base.Comparer{testComparer, base.DefaultComparer},
			[]*base.Merger{testMerger, base.DefaultMerger},
			"",
		},
		{
			[]*base.Comparer{},
			[]*base.Merger{testMerger},
			"unknown comparer test.comparer",
		},
		{
			[]*base.Comparer{base.DefaultComparer},
			[]*base.Merger{testMerger},
			"unknown comparer test.comparer",
		},
		{
			[]*base.Comparer{testComparer},
			[]*base.Merger{},
			"unknown merger test.merger",
		},
		{
			[]*base.Comparer{testComparer},
			[]*base.Merger{base.DefaultMerger},
			"unknown merger test.merger",
		},
	}

	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			f1, err := mem.Open(testTable)
			require.NoError(t, err)

			comparers := make(Comparers)
			for _, comparer := range c.comparers {
				comparers[comparer.Name] = comparer
			}
			mergers := make(Mergers)
			for _, merger := range c.mergers {
				mergers[merger.Name] = merger
			}
			r, err := NewReader(f1, ReaderOptions{}, comparers, mergers)
			if err != nil {
				if r != nil {
					t.Fatalf("found non-nil reader returned with non-nil error %q", err.Error())
				}
				if !strings.HasSuffix(err.Error(), c.expected) {
					t.Fatalf("expected %q, but found %q", c.expected, err.Error())
				}
			} else if c.expected != "" {
				t.Fatalf("expected %q, but found success", c.expected)
			}
			if r != nil {
				_ = r.Close()
			}
		})
	}
}
func checkValidPrefix(prefix, key []byte) bool {
	return prefix == nil || bytes.HasPrefix(key, prefix)
}

func testBytesIteratedWithCompression(
	t *testing.T,
	compression Compression,
	allowedSizeDeviationPercent uint64,
	blockSizes []int,
	maxNumEntries []uint64,
) {
	for i, blockSize := range blockSizes {
		for _, indexBlockSize := range blockSizes {
			for _, numEntries := range []uint64{0, 1, maxNumEntries[i]} {
				r := buildTestTable(t, numEntries, blockSize, indexBlockSize, compression)
				var bytesIterated, prevIterated uint64
				citer, err := r.NewCompactionIter(&bytesIterated)
				require.NoError(t, err)

				for key, _ := citer.First(); key != nil; key, _ = citer.Next() {
					if bytesIterated < prevIterated {
						t.Fatalf("bytesIterated moved backward: %d < %d", bytesIterated, prevIterated)
					}
					prevIterated = bytesIterated
				}

				expected := r.Properties.DataSize
				allowedSizeDeviation := expected * allowedSizeDeviationPercent / 100
				// There is some inaccuracy due to compression estimation.
				if bytesIterated < expected-allowedSizeDeviation || bytesIterated > expected+allowedSizeDeviation {
					t.Fatalf("bytesIterated: got %d, want %d", bytesIterated, expected)
				}

				require.NoError(t, citer.Close())
				require.NoError(t, r.Close())
			}
		}
	}
}

func TestBytesIterated(t *testing.T) {
	blockSizes := []int{10, 100, 1000, 4096, math.MaxInt32}
	t.Run("Compressed", func(t *testing.T) {
		testBytesIteratedWithCompression(t, SnappyCompression, 1, blockSizes, []uint64{1e5, 1e5, 1e5, 1e5, 1e5})
	})
	t.Run("Uncompressed", func(t *testing.T) {
		testBytesIteratedWithCompression(t, NoCompression, 0, blockSizes, []uint64{1e5, 1e5, 1e5, 1e5, 1e5})
	})
	t.Run("Zstd", func(t *testing.T) {
		// compression with zstd is extremely slow with small block size (esp the nocgo version).
		// use less numEntries to make the test run at reasonable speed (under 10 seconds).
		maxNumEntries := []uint64{1e2, 1e2, 1e3, 4e3, 1e5}
		if useStandardZstdLib {
			maxNumEntries = []uint64{1e3, 1e3, 1e4, 4e4, 1e5}
		}
		testBytesIteratedWithCompression(t, ZstdCompression, 1, blockSizes, maxNumEntries)
	})
}

func TestCompactionIteratorSetupForCompaction(t *testing.T) {
	blockSizes := []int{10, 100, 1000, 4096, math.MaxInt32}
	for _, blockSize := range blockSizes {
		for _, indexBlockSize := range blockSizes {
			for _, numEntries := range []uint64{0, 1, 1e5} {
				r := buildTestTable(t, numEntries, blockSize, indexBlockSize, DefaultCompression)
				var bytesIterated uint64
				citer, err := r.NewCompactionIter(&bytesIterated)
				require.NoError(t, err)
				switch i := citer.(type) {
				case *compactionIterator:
					require.NotNil(t, i.dataRS.sequentialFile)
				case *twoLevelCompactionIterator:
					require.NotNil(t, i.dataRS.sequentialFile)
				default:
					require.Failf(t, fmt.Sprintf("unknown compaction iterator type: %T", citer), "")
				}
				require.NoError(t, citer.Close())
				require.NoError(t, r.Close())
			}
		}
	}
}

func TestMaybeReadahead(t *testing.T) {
	var rs readaheadState
	datadriven.RunTest(t, "testdata/readahead", func(d *datadriven.TestData) string {
		cacheHit := false
		switch d.Cmd {
		case "reset":
			rs.size = initialReadaheadSize
			rs.limit = 0
			rs.numReads = 0
			return ""

		case "cache-read":
			cacheHit = true
			fallthrough
		case "read":
			args := strings.Split(d.Input, ",")
			if len(args) != 2 {
				return "expected 2 args: offset, size"
			}

			offset, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
			require.NoError(t, err)
			size, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
			require.NoError(t, err)
			var raSize int64
			if cacheHit {
				rs.recordCacheHit(offset, size)
			} else {
				raSize = rs.maybeReadahead(offset, size)
			}

			var buf strings.Builder
			fmt.Fprintf(&buf, "readahead:  %d\n", raSize)
			fmt.Fprintf(&buf, "numReads:   %d\n", rs.numReads)
			fmt.Fprintf(&buf, "size:       %d\n", rs.size)
			fmt.Fprintf(&buf, "prevSize:   %d\n", rs.prevSize)
			fmt.Fprintf(&buf, "limit:      %d", rs.limit)
			return buf.String()
		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestReaderChecksumErrors(t *testing.T) {
	for _, checksumType := range []ChecksumType{ChecksumTypeCRC32c, ChecksumTypeXXHash64} {
		t.Run(fmt.Sprintf("checksum-type=%d", checksumType), func(t *testing.T) {
			for _, twoLevelIndex := range []bool{false, true} {
				t.Run(fmt.Sprintf("two-level-index=%t", twoLevelIndex), func(t *testing.T) {
					mem := vfs.NewMem()

					{
						// Create an sstable with 3 data blocks.
						f, err := mem.Create("test")
						require.NoError(t, err)

						const blockSize = 32
						indexBlockSize := 4096
						if twoLevelIndex {
							indexBlockSize = 1
						}

						w := NewWriter(f, WriterOptions{
							BlockSize:      blockSize,
							IndexBlockSize: indexBlockSize,
							Checksum:       checksumType,
						})
						require.NoError(t, w.Set(bytes.Repeat([]byte("a"), blockSize), nil))
						require.NoError(t, w.Set(bytes.Repeat([]byte("b"), blockSize), nil))
						require.NoError(t, w.Set(bytes.Repeat([]byte("c"), blockSize), nil))
						require.NoError(t, w.Close())
					}

					// Load the layout so that we no the location of the data blocks.
					var layout *Layout
					{
						f, err := mem.Open("test")
						require.NoError(t, err)

						r, err := NewReader(f, ReaderOptions{})
						require.NoError(t, err)
						layout, err = r.Layout()
						require.NoError(t, err)
						require.EqualValues(t, len(layout.Data), 3)
						require.NoError(t, r.Close())
					}

					for _, bh := range layout.Data {
						// Read the sstable and corrupt the first byte in the target data
						// block.
						orig, err := mem.Open("test")
						require.NoError(t, err)
						data, err := ioutil.ReadAll(orig)
						require.NoError(t, err)
						require.NoError(t, orig.Close())

						// Corrupt the first byte in the block.
						data[bh.Offset] ^= 0xff

						corrupted, err := mem.Create("corrupted")
						require.NoError(t, err)
						_, err = corrupted.Write(data)
						require.NoError(t, err)
						require.NoError(t, corrupted.Close())

						// Verify that we encounter a checksum mismatch error while iterating
						// over the sstable.
						corrupted, err = mem.Open("corrupted")
						require.NoError(t, err)

						r, err := NewReader(corrupted, ReaderOptions{})
						require.NoError(t, err)

						iter, err := r.NewIter(nil, nil)
						require.NoError(t, err)
						for k, _ := iter.First(); k != nil; k, _ = iter.Next() {
						}
						require.Regexp(t, `checksum mismatch`, iter.Error())
						require.Regexp(t, `checksum mismatch`, iter.Close())

						iter, err = r.NewIter(nil, nil)
						require.NoError(t, err)
						for k, _ := iter.Last(); k != nil; k, _ = iter.Prev() {
						}
						require.Regexp(t, `checksum mismatch`, iter.Error())
						require.Regexp(t, `checksum mismatch`, iter.Close())

						require.NoError(t, r.Close())
					}
				})
			}
		})
	}
}

func TestValidateBlockChecksums(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewSource(seed))
	t.Logf("using seed = %d", seed)

	allFiles := []string{
		"testdata/h.no-compression.sst",
		"testdata/h.no-compression.two_level_index.sst",
		"testdata/h.sst",
		"testdata/h.table-bloom.no-compression.prefix_extractor.no_whole_key_filter.sst",
		"testdata/h.table-bloom.no-compression.sst",
		"testdata/h.table-bloom.sst",
		"testdata/h.zstd-compression.sst",
	}

	type corruptionLocation int
	const (
		corruptionLocationData corruptionLocation = iota
		corruptionLocationIndex
		corruptionLocationTopIndex
		corruptionLocationFilter
		corruptionLocationRangeDel
		corruptionLocationProperties
		corruptionLocationMetaIndex
	)

	testCases := []struct {
		name                string
		files               []string
		corruptionLocations []corruptionLocation
	}{
		{
			name:                "no corruption",
			corruptionLocations: []corruptionLocation{},
		},
		{
			name: "data block corruption",
			corruptionLocations: []corruptionLocation{
				corruptionLocationData,
			},
		},
		{
			name: "index block corruption",
			corruptionLocations: []corruptionLocation{
				corruptionLocationIndex,
			},
		},
		{
			name: "top index block corruption",
			files: []string{
				"testdata/h.no-compression.two_level_index.sst",
			},
			corruptionLocations: []corruptionLocation{
				corruptionLocationTopIndex,
			},
		},
		{
			name: "filter block corruption",
			files: []string{
				"testdata/h.table-bloom.no-compression.prefix_extractor.no_whole_key_filter.sst",
				"testdata/h.table-bloom.no-compression.sst",
				"testdata/h.table-bloom.sst",
			},
			corruptionLocations: []corruptionLocation{
				corruptionLocationFilter,
			},
		},
		{
			name: "range deletion block corruption",
			corruptionLocations: []corruptionLocation{
				corruptionLocationRangeDel,
			},
		},
		{
			name: "properties block corruption",
			corruptionLocations: []corruptionLocation{
				corruptionLocationProperties,
			},
		},
		{
			name: "metaindex block corruption",
			corruptionLocations: []corruptionLocation{
				corruptionLocationMetaIndex,
			},
		},
		{
			name: "multiple blocks corrupted",
			corruptionLocations: []corruptionLocation{
				corruptionLocationData,
				corruptionLocationIndex,
				corruptionLocationRangeDel,
				corruptionLocationProperties,
				corruptionLocationMetaIndex,
			},
		},
	}

	testFn := func(t *testing.T, file string, corruptionLocations []corruptionLocation) {
		// Create a copy of the SSTable that we can freely corrupt.
		f, err := os.Open(filepath.FromSlash(file))
		require.NoError(t, err)

		pathCopy := path.Join(t.TempDir(), path.Base(file))
		fCopy, err := os.OpenFile(pathCopy, os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		defer fCopy.Close()

		_, err = io.Copy(fCopy, f)
		require.NoError(t, err)
		err = fCopy.Sync()
		require.NoError(t, err)
		require.NoError(t, f.Close())

		filter := bloom.FilterPolicy(10)
		r, err := NewReader(fCopy, ReaderOptions{
			Filters: map[string]FilterPolicy{
				filter.Name(): filter,
			},
		})
		require.NoError(t, err)
		defer func() { require.NoError(t, r.Close()) }()

		// Prior to corruption, validation is successful.
		require.NoError(t, r.ValidateBlockChecksums())

		// If we are not testing for corruption, we can stop here.
		if len(corruptionLocations) == 0 {
			return
		}

		// Perform bit flips in various corruption locations.
		layout, err := r.Layout()
		require.NoError(t, err)
		for _, location := range corruptionLocations {
			var bh BlockHandle
			switch location {
			case corruptionLocationData:
				bh = layout.Data[rng.Intn(len(layout.Data))].BlockHandle
			case corruptionLocationIndex:
				bh = layout.Index[rng.Intn(len(layout.Index))]
			case corruptionLocationTopIndex:
				bh = layout.TopIndex
			case corruptionLocationFilter:
				bh = layout.Filter
			case corruptionLocationRangeDel:
				bh = layout.RangeDel
			case corruptionLocationProperties:
				bh = layout.Properties
			case corruptionLocationMetaIndex:
				bh = layout.MetaIndex
			default:
				t.Fatalf("unknown location")
			}

			// Corrupt a random byte within the selected block.
			pos := int64(bh.Offset) + rng.Int63n(int64(bh.Length))
			t.Logf("altering file=%s @ offset = %d", file, pos)

			b := make([]byte, 1)
			n, err := fCopy.ReadAt(b, pos)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			t.Logf("data (before) = %08b", b)

			b[0] ^= 0xff
			t.Logf("data (after) = %08b", b)

			_, err = fCopy.WriteAt(b, pos)
			require.NoError(t, err)
		}

		// Write back to the file.
		err = fCopy.Sync()
		require.NoError(t, err)

		// Confirm that checksum validation fails.
		err = r.ValidateBlockChecksums()
		require.Error(t, err)
		require.Regexp(t, `checksum mismatch`, err.Error())
	}

	for _, tc := range testCases {
		// By default, test across all files, unless overridden.
		files := tc.files
		if files == nil {
			files = allFiles
		}
		for _, file := range files {
			t.Run(tc.name+" "+path.Base(file), func(t *testing.T) {
				testFn(t, file, tc.corruptionLocations)
			})
		}
	}
}

func TestReader_TableFormat(t *testing.T) {
	test := func(t *testing.T, want TableFormat) {
		fs := vfs.NewMem()
		f, err := fs.Create("test")
		require.NoError(t, err)

		opts := WriterOptions{TableFormat: want}
		w := NewWriter(f, opts)
		err = w.Close()
		require.NoError(t, err)

		f, err = fs.Open("test")
		require.NoError(t, err)
		r, err := NewReader(f, ReaderOptions{})
		require.NoError(t, err)
		defer r.Close()

		got, err := r.TableFormat()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	for tf := TableFormatLevelDB; tf <= TableFormatMax; tf++ {
		t.Run(tf.String(), func(t *testing.T) {
			test(t, tf)
		})
	}
}

func buildTestTable(
	t *testing.T, numEntries uint64, blockSize, indexBlockSize int, compression Compression,
) *Reader {
	mem := vfs.NewMem()
	f0, err := mem.Create("test")
	require.NoError(t, err)

	w := NewWriter(f0, WriterOptions{
		BlockSize:      blockSize,
		IndexBlockSize: indexBlockSize,
		Compression:    compression,
		FilterPolicy:   nil,
	})

	var ikey InternalKey
	for i := uint64(0); i < numEntries; i++ {
		key := make([]byte, 8+i%3)
		value := make([]byte, i%100)
		binary.BigEndian.PutUint64(key, i)
		ikey.UserKey = key
		w.Add(ikey, value)
	}

	require.NoError(t, w.Close())

	// Re-open that filename for reading.
	f1, err := mem.Open("test")
	require.NoError(t, err)

	c := cache.New(128 << 20)
	defer c.Unref()
	r, err := NewReader(f1, ReaderOptions{
		Cache: c,
	}, FileReopenOpt{
		FS:       mem,
		Filename: "test",
	})
	require.NoError(t, err)
	return r
}

func buildBenchmarkTable(b *testing.B, options WriterOptions) (*Reader, [][]byte) {
	mem := vfs.NewMem()
	f0, err := mem.Create("bench")
	if err != nil {
		b.Fatal(err)
	}

	w := NewWriter(f0, options)

	var keys [][]byte
	var ikey InternalKey
	for i := uint64(0); i < 1e6; i++ {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, i)
		keys = append(keys, key)
		ikey.UserKey = key
		w.Add(ikey, nil)
	}

	if err := w.Close(); err != nil {
		b.Fatal(err)
	}

	// Re-open that filename for reading.
	f1, err := mem.Open("bench")
	if err != nil {
		b.Fatal(err)
	}
	c := cache.New(128 << 20)
	defer c.Unref()
	r, err := NewReader(f1, ReaderOptions{
		Cache: c,
	})
	if err != nil {
		b.Fatal(err)
	}
	return r, keys
}

var basicBenchmarks = []struct {
	name    string
	options WriterOptions
}{
	{
		name: "restart=16,compression=Snappy",
		options: WriterOptions{
			BlockSize:            32 << 10,
			BlockRestartInterval: 16,
			FilterPolicy:         nil,
			Compression:          SnappyCompression,
		},
	},
	{
		name: "restart=16,compression=ZSTD",
		options: WriterOptions{
			BlockSize:            32 << 10,
			BlockRestartInterval: 16,
			FilterPolicy:         nil,
			Compression:          ZstdCompression,
		},
	},
}

func BenchmarkTableIterSeekGE(b *testing.B) {
	for _, bm := range basicBenchmarks {
		b.Run(bm.name,
			func(b *testing.B) {
				r, keys := buildBenchmarkTable(b, bm.options)
				it, err := r.NewIter(nil /* lower */, nil /* upper */)
				require.NoError(b, err)
				rng := rand.New(rand.NewSource(uint64(time.Now().UnixNano())))

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it.SeekGE(keys[rng.Intn(len(keys))], base.SeekGEFlagsNone)
				}

				b.StopTimer()
				it.Close()
				r.Close()
			})
	}
}

func BenchmarkTableIterSeekLT(b *testing.B) {
	for _, bm := range basicBenchmarks {
		b.Run(bm.name,
			func(b *testing.B) {
				r, keys := buildBenchmarkTable(b, bm.options)
				it, err := r.NewIter(nil /* lower */, nil /* upper */)
				require.NoError(b, err)
				rng := rand.New(rand.NewSource(uint64(time.Now().UnixNano())))

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it.SeekLT(keys[rng.Intn(len(keys))], base.SeekLTFlagsNone)
				}

				b.StopTimer()
				it.Close()
				r.Close()
			})
	}
}

func BenchmarkTableIterNext(b *testing.B) {
	for _, bm := range basicBenchmarks {
		b.Run(bm.name,
			func(b *testing.B) {
				r, _ := buildBenchmarkTable(b, bm.options)
				it, err := r.NewIter(nil /* lower */, nil /* upper */)
				require.NoError(b, err)

				b.ResetTimer()
				var sum int64
				var key *InternalKey
				for i := 0; i < b.N; i++ {
					if key == nil {
						key, _ = it.First()
					}
					sum += int64(binary.BigEndian.Uint64(key.UserKey))
					key, _ = it.Next()
				}
				if testing.Verbose() {
					fmt.Fprint(ioutil.Discard, sum)
				}

				b.StopTimer()
				it.Close()
				r.Close()
			})
	}
}

func BenchmarkTableIterPrev(b *testing.B) {
	for _, bm := range basicBenchmarks {
		b.Run(bm.name,
			func(b *testing.B) {
				r, _ := buildBenchmarkTable(b, bm.options)
				it, err := r.NewIter(nil /* lower */, nil /* upper */)
				require.NoError(b, err)

				b.ResetTimer()
				var sum int64
				var key *InternalKey
				for i := 0; i < b.N; i++ {
					if key == nil {
						key, _ = it.Last()
					}
					sum += int64(binary.BigEndian.Uint64(key.UserKey))
					key, _ = it.Prev()
				}
				if testing.Verbose() {
					fmt.Fprint(ioutil.Discard, sum)
				}

				b.StopTimer()
				it.Close()
				r.Close()
			})
	}
}

func BenchmarkLayout(b *testing.B) {
	r, _ := buildBenchmarkTable(b, WriterOptions{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Layout()
	}
	b.StopTimer()
	r.Close()
}
