package utahfs

import (
	"fmt"
	"io"
)

const nilPtr = ^uint32(0)

var errEndOfBlock = fmt.Errorf("blockfs: reached end of block")

// BlockStorage is a derivative of AppStorage that uses uint32 pointers as keys
// instead of strings. It is meant to help make implementing ORAM easier.
type BlockStorage interface {
	State() (map[string]interface{}, error)

	Get(ptr uint32) (data []byte, err error)
	Set(ptr uint32, data []byte) (err error)
}

type basicBlockStorage struct {
	base AppStorage
}

// NewBasicBlockStorage turns an AppStorage implementation into a
// BlockStorage implementation. It simply converts the pointer into a hex string
// and uses that as the key.
func NewBasicBlockStorage(base AppStorage) BlockStorage {
	return basicBlockStorage{base}
}

func (bbs basicBlockStorage) State() (map[string]interface{}, error) {
	return bbs.base.State()
}

func (bbs basicBlockStorage) Get(ptr uint32) ([]byte, error) {
	return bbs.base.Get(fmt.Sprintf("%x", ptr))
}

func (bbs basicBlockStorage) Set(ptr uint32, data []byte) error {
	return bbs.base.Set(fmt.Sprintf("%x", ptr), data)
}

// BlockFilesystem implements large files as skiplists over fixed-size blocks
// stored in an object storage service.
type BlockFilesystem struct {
	store BlockStorage

	numPtrs  int64
	dataSize int64
}

// NewBlockFilesystem returns a new block-based filesystem. Blocks will have
// `numPtrs` pointers in their skiplist and contain at most `dataSize` bytes of
// application data.
//
// Recommended values:
//   numPtrs = 12, dataSize = 512*1024
//
// This system manages two pieces of global state:
//   1. trash - Points to the first block of the trash list: a linked list of
//      blocks which have been discarded and are free for re-allocation.
//   2. next - The next unallocated pointer. A block with this pointer is
//      created only if the trash list is empty.
func NewBlockFilesystem(store BlockStorage, numPtrs, dataSize int64) (*BlockFilesystem, error) {
	if numPtrs < 1 {
		return nil, fmt.Errorf("blockfs: number of pointers must be greater than zero")
	} else if dataSize < 1 || dataSize >= (1<<24) {
		return nil, fmt.Errorf("blockfs: size of data block must be greater zero and less than %v", 1<<24)
	}

	return &BlockFilesystem{
		store: store,

		numPtrs:  numPtrs,
		dataSize: dataSize,
	}, nil
}

func (bfs *BlockFilesystem) blockSize() int64 { return 4*bfs.numPtrs + 3 + bfs.dataSize }

func (bfs *BlockFilesystem) get(key string, def uint32) (uint32, error) {
	state, err := bfs.store.State()
	if err != nil {
		return nilPtr, err
	}
	val, ok := state["bfs-"+key]
	if !ok {
		return def, nil
	}
	val32, ok := val.(uint32)
	if !ok {
		return nilPtr, fmt.Errorf("blockfs: state value %v has wrong type: %T", key, val)
	}
	return val32, nil
}

func (bfs *BlockFilesystem) set(key string, val uint32) error {
	state, err := bfs.store.State()
	if err != nil {
		return err
	}
	state["bfs-"+key] = val
	return nil
}

// allocate returns the pointer of a block which is free for use by the caller.
func (bfs *BlockFilesystem) allocate() (uint32, error) {
	trash, err := bfs.get("trash", nilPtr)
	if err != nil {
		return nilPtr, err
	} else if trash == nilPtr {
		next, err := bfs.get("next", 0)
		if err != nil {
			return nilPtr, err
		} else if err := bfs.set("next", next+1); err != nil {
			return nilPtr, err
		}
		return next, nil
	}

	data, err := bfs.store.Get(trash)
	if err != nil {
		return nilPtr, fmt.Errorf("blockfs: failed to load block %x: %v", trash, err)
	}
	b, err := parseBlock(bfs, data)
	if err != nil {
		return nilPtr, fmt.Errorf("blockfs: failed to parse block %x: %v", trash, err)
	}

	if err := bfs.set("trash", b.ptrs[0]); err != nil {
		return nilPtr, err
	}
	return trash, nil
}

// Create creates a new file. It returns the pointer to the file and an open
// copy.
func (bfs *BlockFilesystem) Create() (uint32, *BlockFile, error) {
	ptr, err := bfs.allocate()
	if err != nil {
		return nilPtr, nil, err
	}

	ptrs := make([]uint32, bfs.numPtrs)
	for i := 0; i < len(ptrs); i++ {
		ptrs[i] = nilPtr
	}

	bf := &BlockFile{
		parent: bfs,

		start: ptr,
		size:  0,

		pos:  0,
		idx:  0,
		ptr:  ptr,
		curr: &block{parent: bfs, ptrs: ptrs},
	}
	if err := bf.persist(); err != nil {
		return nilPtr, nil, err
	}

	return ptr, bf, nil
}

// Open returns a handle to an existing file.
func (bfs *BlockFilesystem) Open(ptr uint32) (*BlockFile, error) {
	bf := &BlockFile{
		parent: bfs,

		start: ptr,
		size:  0,
	}
	if err := bf.load(ptr, 0); err != nil {
		return nil, err
	}

	return bf, nil
}

// BlockFile implements read-write functionality for a variable-size file over
// a skiplist of fixed-size blocks.
type BlockFile struct {
	parent *BlockFilesystem

	// start points to the first block of the file.
	start uint32
	// size is the total size of the file, in bytes.
	size int64

	// pos is our current position in the file, in bytes.
	pos int64
	// idx is the index of this block in the skiplist.
	idx int64
	// ptr is the pointer for the current block of the file.
	ptr uint32
	// curr is the parsed version of the current block.
	curr *block
}

// persist saves any changes to the current block to the storage backend.
func (bf *BlockFile) persist() error {
	return bf.parent.store.Set(bf.ptr, bf.curr.Marshal())
}

// load pulls the block at `ptr` into memory. `pos` is our new position in the
// file.
func (bf *BlockFile) load(ptr uint32, pos int64) error {
	data, err := bf.parent.store.Get(ptr)
	if err != nil {
		return fmt.Errorf("blockfs: failed to load block %x: %v", ptr, err)
	}
	curr, err := parseBlock(bf.parent, data)
	if err != nil {
		return fmt.Errorf("blockfs: failed to parse block %x: %v", ptr, err)
	}

	bf.pos = pos
	bf.idx = pos / bf.parent.dataSize
	bf.ptr = ptr
	bf.curr = curr

	return nil
}

func (bf *BlockFile) Read(p []byte) (int, error) {
	n, err := bf.read(p)
	bf.pos += int64(n)
	return n, err
}

func (bf *BlockFile) read(p []byte) (int, error) {
	n, err := bf.readAt(p, bf.pos)
	if err == errEndOfBlock {
		if bf.curr.ptrs[0] == nilPtr {
			return 0, io.EOF
		} else if err := bf.load(bf.curr.ptrs[0], bf.pos); err != nil {
			return 0, err
		}
		return bf.readAt(p, bf.pos)
	}

	return n, err
}

func (bf *BlockFile) readAt(p []byte, offset int64) (int, error) {
	offset = offset - bf.idx*bf.parent.dataSize
	if offset == bf.parent.dataSize {
		return 0, errEndOfBlock
	} else if offset < 0 || offset > bf.parent.dataSize {
		return 0, fmt.Errorf("blockfs: invalid offset to read from block")
	} else if offset >= int64(len(bf.curr.data)) {
		return 0, io.EOF
	}

	n := copy(p, bf.curr.data[offset:])
	return n, nil
}

func (bf *BlockFile) Write(p []byte) (int, error) {
	n := 0

	for n < len(p) {
		m, err := bf.write(p[n:])

		n += m
		bf.pos += int64(m)
		if bf.pos > bf.size {
			bf.size = bf.pos
		}

		if err != nil {
			return n, err
		}
	}

	return n, bf.persist()
}

func (bf *BlockFile) write(p []byte) (int, error) {
	n, err := bf.writeAt(p, bf.pos)
	if err == nil {
		return n, nil
	} else if err != errEndOfBlock {
		return n, err
	} // else err == errEndOfBlock

	// Check if the next block already exists and just write over it if so.
	if bf.curr.ptrs[0] != nilPtr {
		if err := bf.persist(); err != nil {
			return 0, err
		} else if err := bf.load(bf.curr.ptrs[0], bf.pos); err != nil {
			return 0, err
		}
		return bf.writeAt(p, bf.pos)
	}

	// There is no next block. We have to create it. First thing is to change
	// the format of the current block from a tail to an intermediate.
	ptr, err := bf.parent.allocate()
	if err != nil {
		return 0, err
	}
	ptrs := bf.curr.Upgrade(bf.idx, bf.ptr, ptr)
	if err := bf.persist(); err != nil {
		return 0, err
	}

	// Load all the ancestor blocks that should point to our new block into
	// memory, and give them the pointer to the new block.
	pos, idx := bf.pos, bf.idx+1

	for i := 1; i < len(ptrs); i++ {
		if idx%(1<<uint(i)) != 0 {
			continue
		} else if err := bf.load(ptrs[i], pos-(1<<uint(i))*bf.parent.dataSize); err != nil {
			return 0, err
		}
		bf.curr.ptrs[i] = ptr
		if err := bf.persist(); err != nil {
			return 0, err
		}
	}

	// 'Load' the new block into memory.
	bf.pos = pos
	bf.idx = idx
	bf.ptr = ptr
	bf.curr = &block{parent: bf.parent, ptrs: ptrs}

	return bf.writeAt(p, bf.pos)
}

func (bf *BlockFile) writeAt(p []byte, offset int64) (int, error) {
	offset = offset - bf.idx*bf.parent.dataSize
	if offset == bf.parent.dataSize {
		return 0, errEndOfBlock
	} else if offset < 0 || offset > bf.parent.dataSize {
		return 0, fmt.Errorf("blockfs: invalid offset to write to block")
	}

	// Expand data slice if necessary.
	end := offset + int64(len(p))
	if end > bf.parent.dataSize {
		end = bf.parent.dataSize
	}
	if end > int64(len(bf.curr.data)) {
		temp := make([]byte, end)
		copy(temp, bf.curr.data)
		bf.curr.data = temp
	}

	n := copy(bf.curr.data[offset:], p)
	return n, nil
}

func (bf *BlockFile) Seek(offset int64, whence int) (int64, error) {
	// Calculate offset relative to the beginning of the file.
	if whence == io.SeekStart {
		// offset = offset
	} else if whence == io.SeekCurrent {
		offset = bf.pos + offset
	} else if whence == io.SeekEnd {
		offset = bf.size - offset
	} else {
		return -1, fmt.Errorf("blockfs: unexpected value for whence")
	}

	if offset < 0 {
		return -1, fmt.Errorf("blockfs: cannot seek past beginning of file")
	} else if offset > bf.size {
		return -1, fmt.Errorf("blockfs: cannot seek past end of file")
	}
	bf.pos = bf.idx * bf.parent.dataSize

	// Follow the skiplist.
	if offset < bf.pos {
		if err := bf.load(bf.start, 0); err != nil {
			return -1, err
		}
	}

	for bf.pos != offset {
		// See if we have what we need in-memory.
		if d := offset - bf.pos; d < bf.parent.dataSize {
			bf.pos += d
			return offset, nil
		}

		// We need to load another block. Choose the next pointer to follow.
		if bf.curr.ptrs[0] == nilPtr {
			return -1, fmt.Errorf("blockfs: unexpectedly reached end of skiplist")
		}
		stepped := false

		for i := len(bf.curr.ptrs) - 1; i >= 0; i-- {
			if bf.curr.ptrs[i] == nilPtr {
				continue
			}
			pos := bf.pos + (1<<uint(i))*bf.parent.dataSize
			if pos > offset {
				continue
			}

			// This pointer will get us as far as possible without going over.
			if err := bf.load(bf.curr.ptrs[i], pos); err != nil {
				return -1, err
			}
			stepped = true
			break
		}

		if !stepped { // This error should only ever occur if the skiplist is corrupted.
			return -1, fmt.Errorf("blockfs: failed to find a suitable pointer in skiplist")
		}
	}

	return bf.pos, nil
}

type block struct {
	parent *BlockFilesystem

	ptrs []uint32 // ptrs contains the skiplist pointers from the current block.
	data []byte   // data is the block's application data.
}

func parseBlock(bfs *BlockFilesystem, raw []byte) (*block, error) {
	if int64(len(raw)) != bfs.blockSize() {
		return nil, fmt.Errorf("blockfs: unexpected size: %v != %v", len(raw), bfs.blockSize())
	}

	// Read pointers.
	ptrs := make([]uint32, bfs.numPtrs)
	for i := 0; i < len(ptrs); i++ {
		ptrs[i] = uint32(readInt(raw[:4]))
		raw = raw[4:]
	}

	// Read length of application data.
	size := readInt(raw[:3])
	raw = raw[3:]
	if len(raw) < size {
		return nil, fmt.Errorf("blockfs: application data has unexpected size")
	}

	return &block{bfs, ptrs, raw[:size]}, nil
}

// Upgrade modifies this node from a tail node to an intermediate node and
// returns the pointers for the next tail node.
func (b *block) Upgrade(currIdx int64, currPtr, nextPtr uint32) []uint32 {
	// Compute the tail pointers for the subsequent block.
	out := make([]uint32, b.parent.numPtrs)
	out[0] = nilPtr
	for i := 1; i < len(out); i++ {
		if currIdx%(1<<uint(i)) == 0 {
			out[i] = currPtr
		} else {
			out[i] = b.ptrs[i]
		}
	}

	// Update this block to point to the next block and nothing else, because
	// nothing else exists past that.
	b.ptrs[0] = nextPtr
	for i := 1; i < len(b.ptrs); i++ {
		b.ptrs[i] = nilPtr
	}

	return out
}

func (b *block) Marshal() []byte {
	out := make([]byte, b.parent.blockSize())
	rest := out[0:]

	// Write pointers.
	for i := 0; i < len(b.ptrs); i++ {
		writeInt(int(b.ptrs[i]), rest[:4])
		rest = rest[4:]
	}

	// Write length of application data.
	writeInt(len(b.data), rest[:3])
	rest = rest[3:]

	// Write application data.
	copy(rest, b.data)

	return out
}

func readInt(in []byte) int {
	out := 0
	for i := len(in) - 1; i >= 0; i-- {
		out = out<<8 + int(in[i])
	}
	return out
}

func writeInt(in int, out []byte) {
	for i := 0; i < len(out); i++ {
		out[i] = byte(in)
		in = in >> 8
	}
}
