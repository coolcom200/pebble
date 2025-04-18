// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/cockroachdb/pebble/internal/humanize"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestWriter(t *testing.T) {
	runDataDriven(t, "testdata/writer", false)
}

func TestRewriter(t *testing.T) {
	runDataDriven(t, "testdata/rewriter", false)
}

func TestWriterParallel(t *testing.T) {
	runDataDriven(t, "testdata/writer", true)
}

func TestRewriterParallel(t *testing.T) {
	runDataDriven(t, "testdata/rewriter", true)
}

func runDataDriven(t *testing.T, file string, parallelism bool) {
	var r *Reader
	defer func() {
		if r != nil {
			require.NoError(t, r.Close())
		}
	}()
	formatVersion := TableFormatMax

	format := func(m *WriterMetadata) string {
		var b bytes.Buffer
		if m.HasPointKeys {
			fmt.Fprintf(&b, "point:    [%s-%s]\n", m.SmallestPoint, m.LargestPoint)
		}
		if m.HasRangeDelKeys {
			fmt.Fprintf(&b, "rangedel: [%s-%s]\n", m.SmallestRangeDel, m.LargestRangeDel)
		}
		if m.HasRangeKeys {
			fmt.Fprintf(&b, "rangekey: [%s-%s]\n", m.SmallestRangeKey, m.LargestRangeKey)
		}
		fmt.Fprintf(&b, "seqnums:  [%d-%d]\n", m.SmallestSeqNum, m.LargestSeqNum)
		return b.String()
	}

	datadriven.RunTest(t, file, func(td *datadriven.TestData) string {
		switch td.Cmd {
		case "build":
			if r != nil {
				_ = r.Close()
				r = nil
			}
			var meta *WriterMetadata
			var err error
			meta, r, err = runBuildCmd(td, &WriterOptions{
				TableFormat: formatVersion,
				Parallelism: parallelism,
			}, 0)
			if err != nil {
				return err.Error()
			}
			return format(meta)

		case "build-raw":
			if r != nil {
				_ = r.Close()
				r = nil
			}
			var meta *WriterMetadata
			var err error
			meta, r, err = runBuildRawCmd(td, &WriterOptions{
				TableFormat: formatVersion,
			})
			if err != nil {
				return err.Error()
			}
			return format(meta)

		case "scan":
			origIter, err := r.NewIter(nil /* lower */, nil /* upper */)
			if err != nil {
				return err.Error()
			}
			iter := newIterAdapter(origIter)
			defer iter.Close()

			var buf bytes.Buffer
			for valid := iter.First(); valid; valid = iter.Next() {
				fmt.Fprintf(&buf, "%s:%s\n", iter.Key(), iter.Value())
			}
			return buf.String()

		case "get":
			var buf bytes.Buffer
			for _, k := range strings.Split(td.Input, "\n") {
				value, err := r.get([]byte(k))
				if err != nil {
					fmt.Fprintf(&buf, "get %s: %s\n", k, err.Error())
				} else {
					fmt.Fprintf(&buf, "%s\n", value)
				}
			}
			return buf.String()

		case "scan-range-del":
			iter, err := r.NewRawRangeDelIter()
			if err != nil {
				return err.Error()
			}
			if iter == nil {
				return ""
			}
			defer iter.Close()

			var buf bytes.Buffer
			for s := iter.First(); s != nil; s = iter.Next() {
				fmt.Fprintf(&buf, "%s\n", s)
			}
			return buf.String()

		case "scan-range-key":
			iter, err := r.NewRawRangeKeyIter()
			if err != nil {
				return err.Error()
			}
			if iter == nil {
				return ""
			}
			defer iter.Close()

			var buf bytes.Buffer
			for s := iter.First(); s != nil; s = iter.Next() {
				fmt.Fprintf(&buf, "%s\n", s)
			}
			return buf.String()

		case "layout":
			l, err := r.Layout()
			if err != nil {
				return err.Error()
			}
			verbose := false
			if len(td.CmdArgs) > 0 {
				if td.CmdArgs[0].Key == "verbose" {
					verbose = true
				} else {
					return "unknown arg"
				}
			}
			var buf bytes.Buffer
			l.Describe(&buf, verbose, r, nil)
			return buf.String()

		case "rewrite":
			var meta *WriterMetadata
			var err error
			meta, r, err = runRewriteCmd(td, r, WriterOptions{
				TableFormat: formatVersion,
			})
			if err != nil {
				return err.Error()
			}
			if err != nil {
				return err.Error()
			}
			return format(meta)

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func testBlockBufClear(t *testing.T, b1, b2 *blockBuf) {
	require.Equal(t, b1.tmp, b2.tmp)
}

func TestBlockBufClear(t *testing.T) {
	b1 := &blockBuf{}
	b1.tmp[0] = 1
	b1.compressedBuf = make([]byte, 1)
	b1.clear()
	testBlockBufClear(t, b1, &blockBuf{})
}

func TestClearDataBlockBuf(t *testing.T) {
	d := newDataBlockBuf(1, ChecksumTypeCRC32c)
	d.blockBuf.compressedBuf = make([]byte, 1)
	d.dataBlock.add(ikey("apple"), nil)
	d.dataBlock.add(ikey("banana"), nil)

	d.clear()
	testBlockCleared(t, &d.dataBlock, &blockWriter{})
	testBlockBufClear(t, &d.blockBuf, &blockBuf{})

	dataBlockBufPool.Put(d)
}

func TestClearIndexBlockBuf(t *testing.T) {
	i := newIndexBlockBuf(false)
	i.block.add(ikey("apple"), nil)
	i.block.add(ikey("banana"), nil)
	i.clear()

	testBlockCleared(t, &i.block, &blockWriter{})
	require.Equal(
		t, i.size.estimate, sizeEstimate{emptySize: i.size.estimate.emptySize},
	)
	indexBlockBufPool.Put(i)
}

func TestClearWriteTask(t *testing.T) {
	w := writeTaskPool.Get().(*writeTask)
	ch := make(chan bool, 1)
	w.compressionDone = ch
	w.buf = &dataBlockBuf{}
	w.flushableIndexBlock = &indexBlockBuf{}
	w.currIndexBlock = &indexBlockBuf{}
	w.indexEntrySep = ikey("apple")
	w.inflightSize = 1
	w.indexInflightSize = 1
	w.finishedIndexProps = []byte{'a', 'v'}

	w.clear()

	var nilDataBlockBuf *dataBlockBuf
	var nilIndexBlockBuf *indexBlockBuf
	// Channels should be the same(no new channel should be allocated)
	require.Equal(t, w.compressionDone, ch)
	require.Equal(t, w.buf, nilDataBlockBuf)
	require.Equal(t, w.flushableIndexBlock, nilIndexBlockBuf)
	require.Equal(t, w.currIndexBlock, nilIndexBlockBuf)
	require.Equal(t, w.indexEntrySep, base.InvalidInternalKey)
	require.Equal(t, w.inflightSize, 0)
	require.Equal(t, w.indexInflightSize, 0)
	require.Equal(t, w.finishedIndexProps, []byte(nil))

	writeTaskPool.Put(w)
}

func TestDoubleClose(t *testing.T) {
	// There is code in Cockroach land which relies on Writer.Close being
	// idempotent. We should test this in Pebble, so that we don't cause
	// Cockroach test failures.
	f := &discardFile{}
	w := NewWriter(f, WriterOptions{
		BlockSize:   1,
		TableFormat: TableFormatPebblev1,
	})
	w.Set(ikey("a").UserKey, nil)
	w.Set(ikey("b").UserKey, nil)
	err := w.Close()
	require.NoError(t, err)
	err = w.Close()
	require.Equal(t, err, errWriterClosed)
}

func TestParallelWriterErrorProp(t *testing.T) {
	fs := vfs.NewMem()
	f, err := fs.Create("test")
	require.NoError(t, err)
	opts := WriterOptions{
		TableFormat: TableFormatPebblev1, BlockSize: 1, Parallelism: true,
	}

	w := NewWriter(f, opts)
	// Directly testing this, because it's difficult to get the Writer to
	// encounter an error, precisely when the writeQueue is doing block writes.
	w.coordination.writeQueue.err = errors.New("write queue write error")
	w.Set(ikey("a").UserKey, nil)
	w.Set(ikey("b").UserKey, nil)
	err = w.Close()
	require.Equal(t, err.Error(), "write queue write error")
}

func TestSizeEstimate(t *testing.T) {
	var sizeEstimate sizeEstimate
	datadriven.RunTest(t, "testdata/size_estimate",
		func(td *datadriven.TestData) string {
			switch td.Cmd {
			case "init":
				if len(td.CmdArgs) != 1 {
					return "init <empty size>"
				}
				emptySize, err := strconv.Atoi(td.CmdArgs[0].String())
				if err != nil {
					return "invalid empty size"
				}
				sizeEstimate.init(uint64(emptySize))
				return "success"
			case "clear":
				sizeEstimate.clear()
				return fmt.Sprintf("%d", sizeEstimate.size())
			case "size":
				return fmt.Sprintf("%d", sizeEstimate.size())
			case "add_inflight":
				if len(td.CmdArgs) != 1 {
					return "add_inflight <inflight size estimate>"
				}
				inflightSize, err := strconv.Atoi(td.CmdArgs[0].String())
				if err != nil {
					return "invalid inflight size"
				}
				sizeEstimate.addInflight(inflightSize)
				return fmt.Sprintf("%d", sizeEstimate.size())
			case "entry_written":
				if len(td.CmdArgs) != 3 {
					return "entry_written <new_size> <prev_inflight_size> <entry_size>"
				}
				newSize, err := strconv.Atoi(td.CmdArgs[0].String())
				if err != nil {
					return "invalid inflight size"
				}
				inflightSize, err := strconv.Atoi(td.CmdArgs[1].String())
				if err != nil {
					return "invalid inflight size"
				}
				entrySize, err := strconv.Atoi(td.CmdArgs[2].String())
				if err != nil {
					return "invalid inflight size"
				}
				sizeEstimate.written(uint64(newSize), inflightSize, entrySize)
				return fmt.Sprintf("%d", sizeEstimate.size())
			case "num_written_entries":
				return fmt.Sprintf("%d", sizeEstimate.numWrittenEntries)
			case "num_inflight_entries":
				return fmt.Sprintf("%d", sizeEstimate.numInflightEntries)
			case "num_entries":
				return fmt.Sprintf("%d", sizeEstimate.numWrittenEntries+sizeEstimate.numInflightEntries)
			default:
				return fmt.Sprintf("unknown command: %s", td.Cmd)
			}
		})
}
func TestWriterClearCache(t *testing.T) {
	// Verify that Writer clears the cache of blocks that it writes.
	mem := vfs.NewMem()
	opts := ReaderOptions{Cache: cache.New(64 << 20)}
	defer opts.Cache.Unref()

	writerOpts := WriterOptions{Cache: opts.Cache}
	cacheOpts := &cacheOpts{cacheID: 1, fileNum: 1}
	invalidData := func() *cache.Value {
		invalid := []byte("invalid data")
		v := opts.Cache.Alloc(len(invalid))
		copy(v.Buf(), invalid)
		return v
	}

	build := func(name string) {
		f, err := mem.Create(name)
		require.NoError(t, err)

		w := NewWriter(f, writerOpts, cacheOpts)
		require.NoError(t, w.Set([]byte("hello"), []byte("world")))
		require.NoError(t, w.Close())
	}

	// Build the sstable a first time so that we can determine the locations of
	// all of the blocks.
	build("test")

	f, err := mem.Open("test")
	require.NoError(t, err)

	r, err := NewReader(f, opts)
	require.NoError(t, err)

	layout, err := r.Layout()
	require.NoError(t, err)

	foreachBH := func(layout *Layout, f func(bh BlockHandle)) {
		for _, bh := range layout.Data {
			f(bh.BlockHandle)
		}
		for _, bh := range layout.Index {
			f(bh)
		}
		f(layout.TopIndex)
		f(layout.Filter)
		f(layout.RangeDel)
		f(layout.Properties)
		f(layout.MetaIndex)
	}

	// Poison the cache for each of the blocks.
	poison := func(bh BlockHandle) {
		opts.Cache.Set(cacheOpts.cacheID, cacheOpts.fileNum, bh.Offset, invalidData()).Release()
	}
	foreachBH(layout, poison)

	// Build the table a second time. This should clear the cache for the blocks
	// that are written.
	build("test")

	// Verify that the written blocks have been cleared from the cache.
	check := func(bh BlockHandle) {
		h := opts.Cache.Get(cacheOpts.cacheID, cacheOpts.fileNum, bh.Offset)
		if h.Get() != nil {
			t.Fatalf("%d: expected cache to be cleared, but found %q", bh.Offset, h.Get())
		}
	}
	foreachBH(layout, check)

	require.NoError(t, r.Close())
}

type discardFile struct{ wrote int64 }

func (f discardFile) Close() error {
	return nil
}

func (f *discardFile) Write(p []byte) (int, error) {
	f.wrote += int64(len(p))
	return len(p), nil
}

func (f discardFile) Sync() error {
	return nil
}

type blockPropErrSite uint

const (
	errSiteAdd blockPropErrSite = iota
	errSiteFinishBlock
	errSiteFinishIndex
	errSiteFinishTable
	errSiteNone
)

type testBlockPropCollector struct {
	errSite blockPropErrSite
	err     error
}

func (c *testBlockPropCollector) Name() string { return "testBlockPropCollector" }

func (c *testBlockPropCollector) Add(_ InternalKey, _ []byte) error {
	if c.errSite == errSiteAdd {
		return c.err
	}
	return nil
}

func (c *testBlockPropCollector) FinishDataBlock(_ []byte) ([]byte, error) {
	if c.errSite == errSiteFinishBlock {
		return nil, c.err
	}
	return nil, nil
}

func (c *testBlockPropCollector) AddPrevDataBlockToIndexBlock() {}

func (c *testBlockPropCollector) FinishIndexBlock(_ []byte) ([]byte, error) {
	if c.errSite == errSiteFinishIndex {
		return nil, c.err
	}
	return nil, nil
}

func (c *testBlockPropCollector) FinishTable(_ []byte) ([]byte, error) {
	if c.errSite == errSiteFinishTable {
		return nil, c.err
	}
	return nil, nil
}

func TestWriterBlockPropertiesErrors(t *testing.T) {
	blockPropErr := errors.Newf("block property collector failed")
	testCases := []blockPropErrSite{
		errSiteAdd,
		errSiteFinishBlock,
		errSiteFinishIndex,
		errSiteFinishTable,
		errSiteNone,
	}

	var (
		k1 = base.MakeInternalKey([]byte("a"), 0, base.InternalKeyKindSet)
		v1 = []byte("apples")
		k2 = base.MakeInternalKey([]byte("b"), 0, base.InternalKeyKindSet)
		v2 = []byte("bananas")
		k3 = base.MakeInternalKey([]byte("c"), 0, base.InternalKeyKindSet)
		v3 = []byte("carrots")
	)

	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			fs := vfs.NewMem()
			f, err := fs.Create("test")
			require.NoError(t, err)

			w := NewWriter(f, WriterOptions{
				BlockSize: 1,
				BlockPropertyCollectors: []func() BlockPropertyCollector{
					func() BlockPropertyCollector {
						return &testBlockPropCollector{
							errSite: tc,
							err:     blockPropErr,
						}
					},
				},
				TableFormat: TableFormatPebblev1,
			})

			err = w.Add(k1, v1)
			switch tc {
			case errSiteAdd:
				require.Error(t, err)
				require.Equal(t, blockPropErr, err)
				return
			case errSiteFinishBlock:
				require.NoError(t, err)
				// Addition of a second key completes the first block.
				err = w.Add(k2, v2)
				require.Error(t, err)
				require.Equal(t, blockPropErr, err)
				return
			case errSiteFinishIndex:
				require.NoError(t, err)
				// Addition of a second key completes the first block.
				err = w.Add(k2, v2)
				require.NoError(t, err)
				// The index entry for the first block is added after the completion of
				// the second block, which is triggered by adding a third key.
				err = w.Add(k3, v3)
				require.Error(t, err)
				require.Equal(t, blockPropErr, err)
				return
			}

			err = w.Close()
			if tc == errSiteFinishTable {
				require.Error(t, err)
				require.Equal(t, blockPropErr, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestWriter_TableFormatCompatibility(t *testing.T) {
	testCases := []struct {
		name        string
		minFormat   TableFormat
		configureFn func(opts *WriterOptions)
		writeFn     func(w *Writer) error
	}{
		{
			name:      "block properties",
			minFormat: TableFormatPebblev1,
			configureFn: func(opts *WriterOptions) {
				opts.BlockPropertyCollectors = []func() BlockPropertyCollector{
					func() BlockPropertyCollector {
						return NewBlockIntervalCollector(
							"collector", &valueCharBlockIntervalCollector{charIdx: 0}, nil,
						)
					},
				}
			},
		},
		{
			name:      "range keys",
			minFormat: TableFormatPebblev2,
			writeFn: func(w *Writer) error {
				return w.RangeKeyDelete([]byte("a"), []byte("b"))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for tf := TableFormatLevelDB; tf <= TableFormatMax; tf++ {
				t.Run(tf.String(), func(t *testing.T) {
					fs := vfs.NewMem()
					f, err := fs.Create("sst")
					require.NoError(t, err)

					opts := WriterOptions{TableFormat: tf}
					if tc.configureFn != nil {
						tc.configureFn(&opts)
					}

					w := NewWriter(f, opts)
					if tc.writeFn != nil {
						err = tc.writeFn(w)
						require.NoError(t, err)
					}

					err = w.Close()
					if tf < tc.minFormat {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}

// Tests for races, such as https://github.com/cockroachdb/cockroach/issues/77194,
// in the Writer.
func TestWriterRace(t *testing.T) {
	ks := testkeys.Alpha(5)
	ks = ks.EveryN(ks.Count() / 1_000)
	keys := make([][]byte, ks.Count())
	for ki := 0; ki < len(keys); ki++ {
		keys[ki] = testkeys.Key(ks, ki)
	}
	readerOpts := ReaderOptions{
		Comparer: testkeys.Comparer,
		Filters:  map[string]base.FilterPolicy{},
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			val := make([]byte, rand.Intn(1000))
			opts := WriterOptions{
				Comparer:    testkeys.Comparer,
				BlockSize:   rand.Intn(1 << 10),
				Compression: NoCompression,
			}
			defer wg.Done()
			f := &memFile{}
			w := NewWriter(f, opts)
			for ki := 0; ki < len(keys); ki++ {
				require.NoError(
					t,
					w.Add(base.MakeInternalKey(keys[ki], uint64(ki), InternalKeyKindSet), val),
				)
				require.Equal(
					t, base.DecodeInternalKey(w.dataBlockBuf.dataBlock.curKey).UserKey, keys[ki],
				)
			}
			require.NoError(t, w.Close())
			require.Equal(t, w.meta.LargestPoint.UserKey, keys[len(keys)-1])
			r, err := NewMemReader(f.Bytes(), readerOpts)
			require.NoError(t, err)
			defer r.Close()
			it, err := r.NewIter(nil, nil)
			require.NoError(t, err)
			defer it.Close()
			ki := 0
			for k, v := it.First(); k != nil; k, v = it.Next() {
				require.Equal(t, k.UserKey, keys[ki])
				require.Equal(t, v, val)
				ki++
			}
		}()
	}
	wg.Wait()
}

func BenchmarkWriter(b *testing.B) {
	keys := make([][]byte, 1e6)
	const keyLen = 24
	keySlab := make([]byte, keyLen*len(keys))
	for i := range keys {
		key := keySlab[i*keyLen : i*keyLen+keyLen]
		binary.BigEndian.PutUint64(key[:8], 123) // 16-byte shared prefix
		binary.BigEndian.PutUint64(key[8:16], 456)
		binary.BigEndian.PutUint64(key[16:], uint64(i))
		keys[i] = key
	}

	b.ResetTimer()

	for _, bs := range []int{base.DefaultBlockSize, 32 << 10} {
		b.Run(fmt.Sprintf("block=%s", humanize.IEC.Int64(int64(bs))), func(b *testing.B) {
			for _, filter := range []bool{true, false} {
				b.Run(fmt.Sprintf("filter=%t", filter), func(b *testing.B) {
					for _, comp := range []Compression{NoCompression, SnappyCompression, ZstdCompression} {
						b.Run(fmt.Sprintf("compression=%s", comp), func(b *testing.B) {
							opts := WriterOptions{
								BlockRestartInterval: 16,
								BlockSize:            bs,
								Compression:          comp,
							}
							if filter {
								opts.FilterPolicy = bloom.FilterPolicy(10)
							}
							f := &discardFile{}
							for i := 0; i < b.N; i++ {
								f.wrote = 0
								w := NewWriter(f, opts)

								for j := range keys {
									if err := w.Set(keys[j], keys[j]); err != nil {
										b.Fatal(err)
									}
								}
								if err := w.Close(); err != nil {
									b.Fatal(err)
								}
								b.SetBytes(int64(f.wrote))
							}
						})
					}
				})
			}
		})
	}
}

var test4bSuffixComparer = &base.Comparer{
	Compare:   base.DefaultComparer.Compare,
	Equal:     base.DefaultComparer.Equal,
	Separator: base.DefaultComparer.Separator,
	Successor: base.DefaultComparer.Successor,
	Split: func(key []byte) int {
		if len(key) > 4 {
			return len(key) - 4
		}
		return len(key)
	},
	Name: "comparer-split-4b-suffix",
}
