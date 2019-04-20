/*Edit! This Gosh fork includes a modifed copy of of Distin H's buffered io pipes package and
the corresponding buffer package on github, both have the same license and copyright.
Awesome program, highly recommend. I did this so goshworker could just use
the gosh exec api without extrenal dependencies exernal dependiencies other custom
than channels which will be for greater memory and task management later, while
keeping this version of gosh standard libary compliant.

The MIT License (MIT)

Copyright (c) 2015 Dustin H

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.*/
package iox

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sync"
)

type Buffer interface {
	Len() int64 // How much data is Buffered in bytes
	Cap() int64 // How much data can be Buffered at once in bytes.
	io.Reader   // Read() will read from the top of the buffer [io.EOF if empty]
	io.Writer   // Write() will write to the end of the buffer [io.ErrShortWrite if not enough space]
	Reset()     // Truncates the buffer, Len() == 0.
}

// BufferAt is a buffer which supports io.ReaderAt and io.WriterAt
type BufferAt interface {
	Buffer
	io.ReaderAt
	io.WriterAt
}

func len64(p []byte) int64 {
	return int64(len(p))
}

// Gap returns buf.Cap() - buf.Len()
func Gap(buf Buffer) int64 {
	return buf.Cap() - buf.Len()
}

// Full returns true iff buf.Len() == buf.Cap()
func Full(buf Buffer) bool {
	return buf.Len() == buf.Cap()
}

// Empty returns false iff buf.Len() == 0
func Empty(buf Buffer) bool {
	return buf.Len() == 0
}

// NewUnboundedBuffer returns a Buffer which buffers "mem" bytes to memory
// and then creates file's of size "file" to buffer above "mem" bytes.
func NewUnboundedBuffer(mem, file int64) Buffer {
	return NewMulti(New(mem), NewPartition(NewFilePool(file, os.TempDir())))
}

// Pipe creates a buffered pipe.
// It can be used to connect code expecting an io.Reader with code expecting an io.Writer.
// Reads on one end read from the supplied Buffer. Writes write to the supplied Buffer.
// It is safe to call Read and Write in parallel with each other or with Close.
// Close will complete once pending I/O is done, and may cancel blocking Read/Writes.
// Buffered data will still be available to Read after the Writer has been closed.
// Parallel calls to Read, and parallel calls to Write are also safe :
// the individual calls will be gated sequentially.
func Pipe(buf Buffer) (r *PipeReader, w *PipeWriter) {
	p := newBufferedPipe(buf)
	r = &PipeReader{bufpipe: p}
	w = &PipeWriter{bufpipe: p}
	return r, w
}

// Copy copies from src to buf, and from buf to dst in parallel until
// either EOF is reached on src or an error occurs. It returns the number of bytes
// copied to dst and the first error encountered while copying, if any.
// EOF is not considered to be an error. If src implements WriterTo, it is used to
// write to the supplied Buffer. If dst implements ReaderFrom, it is used to read from
// the supplied Buffer.
func Copy(dst io.Writer, src io.Reader, buf Buffer) (n int64, err error) {
	return io.Copy(dst, NewReader(src, buf))
}

// NewReader reads from the buffer which is concurrently filled with data from the passed src.
func NewReader(src io.Reader, buf Buffer) io.ReadCloser {
	r, w := Pipe(buf)

	go func() {
		_, err := io.Copy(w, src)
		w.CloseWithError(err)
	}()

	return r
}

type PipeReader struct {
	*bufpipe
}

// CloseWithError closes the reader; subsequent writes to the write half of the pipe will return the error err.
func (r *PipeReader) CloseWithError(err error) error {
	if err == nil {
		err = io.ErrClosedPipe
	}
	r.bufpipe.l.Lock()
	defer r.bufpipe.l.Unlock()
	if r.bufpipe.rerr == nil {
		r.bufpipe.rerr = err
		r.bufpipe.rwait.Signal()
		r.bufpipe.wwait.Signal()
	}
	return nil
}

// Close closes the reader; subsequent writes to the write half of the pipe will return the error io.ErrClosedPipe.
func (r *PipeReader) Close() error {
	return r.CloseWithError(nil)
}

// A PipeWriter is the write half of a pipe.
type PipeWriter struct {
	*bufpipe
}

// CloseWithError closes the writer; once the buffer is empty subsequent reads from the read half of the pipe will return
// no bytes and the error err, or io.EOF if err is nil. CloseWithError always returns nil.
func (w *PipeWriter) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	w.bufpipe.l.Lock()
	defer w.bufpipe.l.Unlock()
	if w.bufpipe.werr == nil {
		w.bufpipe.werr = err
		w.bufpipe.rwait.Signal()
		w.bufpipe.wwait.Signal()
	}
	return nil
}

// Close closes the writer; once the buffer is empty subsequent reads from the read half of the pipe will return
// no bytes and io.EOF after all the buffer has been read. CloseWithError always returns nil.
func (w *PipeWriter) Close() error {
	return w.CloseWithError(nil)
}

type bufpipe struct {
	rl    sync.Mutex
	wl    sync.Mutex
	l     sync.Mutex
	rwait sync.Cond
	wwait sync.Cond
	b     Buffer
	rerr  error // if reader closed, error to give writes
	werr  error // if writer closed, error to give reads
}

func newBufferedPipe(buf Buffer) *bufpipe {
	s := &bufpipe{
		b: buf,
	}
	s.rwait.L = &s.l
	s.wwait.L = &s.l
	return s
}

func empty(buf Buffer) bool {
	return buf.Len() == 0
}

func gap(buf Buffer) int64 {
	return buf.Cap() - buf.Len()
}

func (r *PipeReader) Read(p []byte) (n int, err error) {
	r.rl.Lock()
	defer r.rl.Unlock()

	r.l.Lock()
	defer r.wwait.Signal()
	defer r.l.Unlock()

	for empty(r.b) {
		if r.rerr != nil {
			return 0, io.ErrClosedPipe
		}

		if r.werr != nil {
			return 0, r.werr
		}

		r.wwait.Signal()
		r.rwait.Wait()
	}

	n, err = r.b.Read(p)
	if err == io.EOF {
		err = nil
	}

	return n, err
}

func (w *PipeWriter) Write(p []byte) (int, error) {
	var m int
	var n, space int64
	var err error
	sliceLen := int64(len(p))

	w.wl.Lock()
	defer w.wl.Unlock()

	w.l.Lock()
	defer w.rwait.Signal()
	defer w.l.Unlock()

	if w.werr != nil {
		return 0, io.ErrClosedPipe
	}

	// while there is data to write
	for writeLen := sliceLen; writeLen > 0 && err == nil; writeLen = sliceLen - n {

		// wait for some buffer space to become available (while no errs)
		for space = gap(w.b); space == 0 && w.rerr == nil && w.werr == nil; space = gap(w.b) {
			w.rwait.Signal()
			w.wwait.Wait()
		}

		if w.rerr != nil {
			err = w.rerr
			break
		}

		if w.werr != nil {
			err = io.ErrClosedPipe
			break
		}

		// space > 0, and locked

		var nn int64
		if space < writeLen {
			// => writeLen - space > 0
			// => (sliceLen - n) - space > 0
			// => sliceLen > n + space
			// nn is safe to use for p[:nn]
			nn = n + space
		} else {
			nn = sliceLen
		}

		m, err = w.b.Write(p[n:nn])
		n += int64(m)

		// one of the following cases has occurred:
		// 1. done writing -> writeLen == 0
		// 2. ran out of buffer space -> gap(w.b) == 0
		// 3. an error occurred err != nil
		// all of these cases are handled at the top of this loop
	}

	return int(n), err
}

type memory struct {
	N int64
	*bytes.Buffer
}

// New returns a new in memory BufferAt with max size N.
// It's backed by a bytes.Buffer.
func New(n int64) BufferAt {
	return &memory{
		N:      n,
		Buffer: bytes.NewBuffer(nil),
	}
}

func (buf *memory) Cap() int64 {
	return buf.N
}

func (buf *memory) Len() int64 {
	return int64(buf.Buffer.Len())
}

func (buf *memory) Write(p []byte) (n int, err error) {
	return LimitWriter(buf.Buffer, Gap(buf)).Write(p)
}

func (buf *memory) WriteAt(p []byte, off int64) (n int, err error) {
	if off > buf.Len() {
		return 0, io.ErrShortWrite
	} else if len64(p)+off <= buf.Len() {
		d := buf.Bytes()[off:]
		return copy(d, p), nil
	} else {
		d := buf.Bytes()[off:]
		n = copy(d, p)
		m, err := buf.Write(p[n:])
		return n + m, err
	}
}

func (buf *memory) ReadAt(p []byte, off int64) (n int, err error) {
	return bytes.NewReader(buf.Bytes()).ReadAt(p, off)
}

func (buf *memory) Read(p []byte) (n int, err error) {
	return io.LimitReader(buf.Buffer, buf.Len()).Read(p)
}

func (buf *memory) ReadFrom(r io.Reader) (n int64, err error) {
	return buf.Buffer.ReadFrom(io.LimitReader(r, Gap(buf)))
}

func init() {
	gob.Register(&memory{})
}

func (buf *memory) MarshalBinary() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintln(&b, buf.N)
	b.Write(buf.Bytes())
	return b.Bytes(), nil
}

func (buf *memory) UnmarshalBinary(bindata []byte) error {
	data := make([]byte, len(bindata))
	copy(data, bindata)
	b := bytes.NewBuffer(data)
	_, err := fmt.Fscanln(b, &buf.N)
	buf.Buffer = bytes.NewBuffer(b.Bytes())
	return err
}

type swap struct {
	A BufferAt
	B BufferAt
}

// NewSwap creates a Buffer which writes to a until you write past a.Cap()
// then it io.Copy's from a to b and writes to b.
// Once the Buffer is empty again, it starts over writing to a.
// Note that if b.Cap() <= a.Cap() it will cause a panic, b is expected
// to be larger in order to accomodate writes past a.Cap().
func NewSwap(a, b Buffer) Buffer {
	return NewSwapAt(toBufferAt(a), toBufferAt(b))
}

// NewSwapAt creates a BufferAt which writes to a until you write past a.Cap()
// then it io.Copy's from a to b and writes to b.
// Once the Buffer is empty again, it starts over writing to a.
// Note that if b.Cap() <= a.Cap() it will cause a panic, b is expected
// to be larger in order to accomodate writes past a.Cap().
func NewSwapAt(a, b BufferAt) BufferAt {
	if b.Cap() <= a.Cap() {
		panic("Buffer b must be larger than a.")
	}
	return &swap{A: a, B: b}
}

func (buf *swap) Len() int64 {
	return buf.A.Len() + buf.B.Len()
}

func (buf *swap) Cap() int64 {
	return buf.B.Cap()
}

func (buf *swap) Read(p []byte) (n int, err error) {
	if buf.A.Len() > 0 {
		return buf.A.Read(p)
	}
	return buf.B.Read(p)
}

func (buf *swap) ReadAt(p []byte, off int64) (n int, err error) {
	if buf.A.Len() > 0 {
		return buf.A.ReadAt(p, off)
	}
	return buf.B.ReadAt(p, off)
}

func (buf *swap) Write(p []byte) (n int, err error) {
	switch {
	case buf.B.Len() > 0:
		n, err = buf.B.Write(p)

	case buf.A.Len()+int64(len(p)) > buf.A.Cap():
		_, err = io.Copy(buf.B, buf.A)
		if err == nil {
			n, err = buf.B.Write(p)
		}

	default:
		n, err = buf.A.Write(p)
	}

	return n, err
}

func (buf *swap) WriteAt(p []byte, off int64) (n int, err error) {
	switch {
	case buf.B.Len() > 0:
		n, err = buf.B.WriteAt(p, off)

	case off+int64(len(p)) > buf.A.Cap():
		_, err = io.Copy(buf.B, buf.A)
		if err == nil {
			n, err = buf.B.WriteAt(p, off)
		}

	default:
		n, err = buf.A.WriteAt(p, off)
	}

	return n, err
}

func (buf *swap) Reset() {
	buf.A.Reset()
	buf.B.Reset()
}

func init() {
	gob.Register(&swap{})
}

type spill struct {
	Buffer
	Spiller io.Writer
}

// NewSpill returns a Buffer which writes data to w when there's an error
// writing to buf. Such as when buf is full, or the disk is full, etc.
func NewSpill(buf Buffer, w io.Writer) Buffer {
	if w == nil {
		w = ioutil.Discard
	}
	return &spill{
		Buffer:  buf,
		Spiller: w,
	}
}

func (buf *spill) Cap() int64 {
	return math.MaxInt64
}

func (buf *spill) Write(p []byte) (n int, err error) {
	if n, err = buf.Buffer.Write(p); err != nil {
		m, err := buf.Spiller.Write(p[n:])
		return m + n, err
	}
	return len(p), nil
}

func init() {
	gob.Register(&spill{})
}

type ring struct {
	BufferAt
	L int64
	*WrapReader
	*WrapWriter
}

// NewRing returns a Ring Buffer from a BufferAt.
// It overwrites old data in the Buffer when needed (when its full).
func NewRing(buffer BufferAt) Buffer {
	return &ring{
		BufferAt:   buffer,
		WrapReader: NewWrapReader(buffer, 0, buffer.Cap()),
		WrapWriter: NewWrapWriter(buffer, 0, buffer.Cap()),
	}
}

func (buf *ring) Len() int64 {
	return buf.L
}

func (buf *ring) Cap() int64 {
	return math.MaxInt64
}

func (buf *ring) Read(p []byte) (n int, err error) {
	if buf.L == buf.BufferAt.Cap() {
		buf.WrapReader.Seek(buf.WrapWriter.Offset(), 0)
	}
	n, err = io.LimitReader(buf.WrapReader, buf.L).Read(p)
	buf.L -= int64(n)
	return n, err
}

func (buf *ring) Write(p []byte) (n int, err error) {
	n, err = buf.WrapWriter.Write(p)
	buf.L += int64(n)
	if buf.L > buf.BufferAt.Cap() {
		buf.L = buf.BufferAt.Cap()
	}
	return n, err
}

func (buf *ring) Reset() {
	buf.BufferAt.Reset()
	buf.L = 0
	buf.WrapReader = NewWrapReader(buf.BufferAt, 0, buf.BufferAt.Cap())
	buf.WrapWriter = NewWrapWriter(buf.BufferAt, 0, buf.BufferAt.Cap())
}

type Pool interface {
	Get() (Buffer, error) // Allocate a Buffer
	Put(buf Buffer) error // Release or Reuse a Buffer
}

type pool struct {
	pool sync.Pool
}

// NewPool returns a Pool(), it's backed by a sync.Pool so its safe for concurrent use.
// Get() and Put() errors will always be nil.
// It will not work with gob.
func NewPool(New func() Buffer) Pool {
	return &pool{
		pool: sync.Pool{
			New: func() interface{} {
				return New()
			},
		},
	}
}

func (p *pool) Get() (Buffer, error) {
	return p.pool.Get().(Buffer), nil
}

func (p *pool) Put(buf Buffer) error {
	buf.Reset()
	p.pool.Put(buf)
	return nil
}

type memPool struct {
	N int64
	Pool
}

// NewMemPool returns a Pool, Get() returns an in memory buffer of max size N.
// Put() returns the buffer to the pool after resetting it.
// Get() and Put() errors will always be nil.
func NewMemPool(N int64) Pool {
	return &memPool{
		N: N,
		Pool: NewPool(func() Buffer {
			return New(N)
		}),
	}
}

func (m *memPool) MarshalBinary() ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	err := binary.Write(buf, binary.LittleEndian, m.N)
	return buf.Bytes(), err
}

func (m *memPool) UnmarshalBinary(data []byte) error {
	buf := bytes.NewReader(data)
	err := binary.Read(buf, binary.LittleEndian, &m.N)
	m.Pool = NewPool(func() Buffer {
		return New(m.N)
	})
	return err
}

type filePool struct {
	N         int64
	Directory string
}

// NewFilePool returns a Pool, Get() returns a file-based buffer of max size N.
// Put() closes and deletes the underlying file for the buffer.
// Get() may return an error if it fails to create a file for the buffer.
// Put() may return an error if it fails to delete the file.
func NewFilePool(N int64, dir string) Pool {
	return &filePool{N: N, Directory: dir}
}

func (p *filePool) Get() (Buffer, error) {
	file, err := ioutil.TempFile(p.Directory, "buffer")
	if err != nil {
		return nil, err
	}
	return NewFile(p.N, file), nil
}

func (p *filePool) Put(buf Buffer) (err error) {
	buf.Reset()
	if fileBuf, ok := buf.(*fileBuffer); ok {
		fileBuf.file.Close()
		err = os.Remove(fileBuf.file.Name())
	}
	return err
}

func init() {
	gob.Register(&memPool{})
	gob.Register(&filePool{})
}

type chain struct {
	Buf  BufferAt
	Next BufferAt
}

type nopBufferAt struct {
	Buffer
}

func (buf *nopBufferAt) ReadAt(p []byte, off int64) (int, error) {
	panic("ReadAt not implemented")
}

func (buf *nopBufferAt) WriteAt(p []byte, off int64) (int, error) {
	panic("WriteAt not implemented")
}

// toBufferAt converts a Buffer to a BufferAt with nop ReadAt and WriteAt funcs
func toBufferAt(buf Buffer) BufferAt {
	return &nopBufferAt{Buffer: buf}
}

// NewMultiAt returns a BufferAt which is the logical concatenation of the passed BufferAts.
// The data in the buffers is shifted such that there is no non-empty buffer following
// a non-full buffer, this process is also run after every Read.
// If no buffers are passed, the returned Buffer is nil.
func NewMultiAt(buffers ...BufferAt) BufferAt {
	if len(buffers) == 0 {
		return nil
	} else if len(buffers) == 1 {
		return buffers[0]
	}

	buf := &chain{
		Buf:  buffers[0],
		Next: NewMultiAt(buffers[1:]...),
	}

	buf.Defrag()

	return buf
}

// NewMulti returns a Buffer which is the logical concatenation of the passed buffers.
// The data in the buffers is shifted such that there is no non-empty buffer following
// a non-full buffer, this process is also run after every Read.
// If no buffers are passed, the returned Buffer is nil.
func NewMulti(buffers ...Buffer) Buffer {
	bufAt := make([]BufferAt, len(buffers))
	for i, buf := range buffers {
		bufAt[i] = toBufferAt(buf)
	}
	return NewMultiAt(bufAt...)
}

func (buf *chain) Reset() {
	buf.Next.Reset()
	buf.Buf.Reset()
}

func (buf *chain) Cap() (n int64) {
	Next := buf.Next.Cap()
	if buf.Buf.Cap() > math.MaxInt64-Next {
		return math.MaxInt64
	}
	return buf.Buf.Cap() + Next
}

func (buf *chain) Len() (n int64) {
	Next := buf.Next.Len()
	if buf.Buf.Len() > math.MaxInt64-Next {
		return math.MaxInt64
	}
	return buf.Buf.Len() + Next
}

func (buf *chain) Defrag() {
	for !Full(buf.Buf) && !Empty(buf.Next) {
		r := io.LimitReader(buf.Next, Gap(buf.Buf))
		if _, err := io.Copy(buf.Buf, r); err != nil && err != io.EOF {
			return
		}
	}
}

func (buf *chain) Read(p []byte) (n int, err error) {
	n, err = buf.Buf.Read(p)
	if len(p[n:]) > 0 && (err == nil || err == io.EOF) {
		m, err := buf.Next.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}

	buf.Defrag()

	return n, err
}

func (buf *chain) ReadAt(p []byte, off int64) (n int, err error) {
	if buf.Buf.Len() < off {
		return buf.Next.ReadAt(p, off-buf.Buf.Len())
	}

	n, err = buf.Buf.ReadAt(p, off)
	if len(p[n:]) > 0 && (err == nil || err == io.EOF) {
		var m int
		m, err = buf.Next.ReadAt(p[n:], 0)
		n += m
	}
	return n, err
}

func (buf *chain) Write(p []byte) (n int, err error) {
	if n, err = buf.Buf.Write(p); err == io.ErrShortWrite {
		err = nil
	}
	p = p[n:]
	if len(p) > 0 && err == nil {
		m, err := buf.Next.Write(p)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, err
}

func (buf *chain) WriteAt(p []byte, off int64) (n int, err error) {
	switch {
	case buf.Buf.Cap() <= off: // past the end
		return buf.Next.WriteAt(p, off-buf.Buf.Cap())

	case buf.Buf.Cap() >= off+int64(len(p)): // fits in
		return buf.Buf.WriteAt(p, off)

	default: // partial fit
		n, err = buf.Buf.WriteAt(p, off)
		if len(p[n:]) > 0 && (err == nil || err == io.ErrShortWrite) {
			var m int
			m, err = buf.Next.WriteAt(p[n:], 0)
			n += m
		}
		return n, err
	}
}

func init() {
	gob.Register(&chain{})
	gob.Register(&nopBufferAt{})
}

func (buf *chain) MarshalBinary() ([]byte, error) {
	b := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(b)
	if err := enc.Encode(&buf.Buf); err != nil {
		return nil, err
	}
	if err := enc.Encode(&buf.Next); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (buf *chain) UnmarshalBinary(data []byte) error {
	b := bytes.NewBuffer(data)
	dec := gob.NewDecoder(b)
	if err := dec.Decode(&buf.Buf); err != nil {
		return err
	}
	if err := dec.Decode(&buf.Next); err != nil {
		return err
	}
	return nil
}

type File interface {
	Name() string
	Stat() (fi os.FileInfo, err error)
	io.ReaderAt
	io.WriterAt
	Close() error
}

type fileBuffer struct {
	file File
	*Wrapper
}

// NewFile returns a new BufferAt backed by "file" with max-size N.
func NewFile(N int64, file File) BufferAt {
	return &fileBuffer{
		file:    file,
		Wrapper: NewWrapper(file, 0, 0, N),
	}
}

func init() {
	gob.Register(&fileBuffer{})
}

func (buf *fileBuffer) MarshalBinary() ([]byte, error) {
	fullpath, err := filepath.Abs(filepath.Dir(buf.file.Name()))
	if err != nil {
		return nil, err
	}
	base := filepath.Base(buf.file.Name())
	buf.file.Close()

	buffer := bytes.NewBuffer(nil)
	fmt.Fprintln(buffer, filepath.Join(fullpath, base))
	fmt.Fprintln(buffer, buf.Wrapper.N, buf.Wrapper.L, buf.Wrapper.O)
	return buffer.Bytes(), nil
}

func (buf *fileBuffer) UnmarshalBinary(data []byte) error {
	buffer := bytes.NewBuffer(data)
	var filename string
	var N, L, O int64
	_, err := fmt.Fscanln(buffer, &filename)

	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	buf.file = file

	_, err = fmt.Fscanln(buffer, &N, &L, &O)
	buf.Wrapper = NewWrapper(file, L, O, N)
	return nil
}

type discard struct{}

// Discard is a Buffer which writes to ioutil.Discard and read's return 0, io.EOF.
// All of its methods are concurrent safe.
var Discard Buffer = discard{}

func (buf discard) Len() int64 {
	return 0
}

func (buf discard) Cap() int64 {
	return math.MaxInt64
}

func (buf discard) Reset() {}

func (buf discard) Read(p []byte) (n int, err error) {
	return 0, io.EOF
}

func (buf discard) Write(p []byte) (int, error) {
	return ioutil.Discard.Write(p)
}

func init() {
	gob.Register(&discard{})
}

type DoerAt interface {
	DoAt([]byte, int64) (int, error)
}

// DoAtFunc is implemented by ReadAt/WriteAt
type DoAtFunc func([]byte, int64) (int, error)

type wrapper struct {
	off    int64
	wrapAt int64
	doat   DoAtFunc
}

func (w *wrapper) Offset() int64 {
	return w.off
}

func (w *wrapper) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		w.off = offset
	case 1:
		w.off += offset
	case 2:
		w.off = (w.wrapAt + offset)
	}
	w.off %= w.wrapAt
	return w.off, nil
}

func (w *wrapper) DoAt(p []byte, off int64) (n int, err error) {
	return w.doat(p, off)
}

// WrapWriter wraps writes around a section of data.
type WrapWriter struct {
	*wrapper
}

// NewWrapWriter creates a WrapWriter starting at offset off, and wrapping at offset wrapAt.
func NewWrapWriter(w io.WriterAt, off int64, wrapAt int64) *WrapWriter {
	return &WrapWriter{
		&wrapper{
			doat:   w.WriteAt,
			off:    (off % wrapAt),
			wrapAt: wrapAt,
		},
	}
}

// Write writes p starting at the current offset, wrapping when it reaches the end.
// The current offset is shifted forward by the amount written.
func (w *WrapWriter) Write(p []byte) (n int, err error) {
	n, err = Wrap(w, p, w.off, w.wrapAt)
	w.off = (w.off + int64(n)) % w.wrapAt
	return n, err
}

// WriteAt writes p starting at offset off, wrapping when it reaches the end.
func (w *WrapWriter) WriteAt(p []byte, off int64) (n int, err error) {
	return Wrap(w, p, off, w.wrapAt)
}

// WrapReader wraps reads around a section of data.
type WrapReader struct {
	*wrapper
}

// NewWrapReader creates a WrapReader starting at offset off, and wrapping at offset wrapAt.
func NewWrapReader(r io.ReaderAt, off int64, wrapAt int64) *WrapReader {
	return &WrapReader{
		&wrapper{
			doat:   r.ReadAt,
			off:    (off % wrapAt),
			wrapAt: wrapAt,
		},
	}
}

// Read reads into p starting at the current offset, wrapping if it reaches the end.
// The current offset is shifted forward by the amount read.
func (r *WrapReader) Read(p []byte) (n int, err error) {
	n, err = Wrap(r, p, r.off, r.wrapAt)
	r.off = (r.off + int64(n)) % r.wrapAt
	return n, err
}

// ReadAt reads into p starting at the current offset, wrapping when it reaches the end.
func (r *WrapReader) ReadAt(p []byte, off int64) (n int, err error) {
	return Wrap(r, p, off, r.wrapAt)
}

// maxConsecutiveEmptyActions determines how many consecutive empty reads/writes can occur before giving up
const maxConsecutiveEmptyActions = 100

// Wrap causes an action on an array of bytes (like read/write) to be done from an offset off,
// wrapping at offset wrapAt.
func Wrap(w DoerAt, p []byte, off int64, wrapAt int64) (n int, err error) {
	var m, fails int

	off %= wrapAt

	for len(p) > 0 {

		if off+int64(len(p)) < wrapAt {
			m, err = w.DoAt(p, off)
		} else {
			space := wrapAt - off
			m, err = w.DoAt(p[:space], off)
		}

		if err != nil && err != io.EOF {
			return n + m, err
		}

		switch m {
		case 0:
			fails++
		default:
			fails = 0
		}

		if fails > maxConsecutiveEmptyActions {
			return n + m, io.ErrNoProgress
		}

		n += m
		p = p[m:]
		off += int64(m)
		off %= wrapAt
	}

	return n, err
}

// ReadWriterAt implements io.ReaderAt and io.WriterAt
type ReadWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// Wrapper implements a io.ReadWriter and ReadWriterAt such that
// when reading/writing goes past N bytes, it "wraps" back to the beginning.
type Wrapper struct {
	// N is the offset at which to "wrap" back to the start
	N int64
	// L is the length of the data written
	L int64
	// O is our offset in the data
	O   int64
	rwa ReadWriterAt
}

// NewWrapper creates a Wrapper based on ReadWriterAt rwa.
// L is the current length, O is the current offset, and N is offset at which we "wrap".
func NewWrapper(rwa ReadWriterAt, L, O, N int64) *Wrapper {
	return &Wrapper{
		L:   L,
		O:   O,
		N:   N,
		rwa: rwa,
	}
}

// Len returns the # of bytes in the Wrapper
func (wpr *Wrapper) Len() int64 {
	return wpr.L
}

// Cap returns the "wrap" offset (max # of bytes)
func (wpr *Wrapper) Cap() int64 {
	return wpr.N
}

// Reset seeks to the start (0 offset), and sets the length to 0.
func (wpr *Wrapper) Reset() {
	wpr.O = 0
	wpr.L = 0
}

// SetReadWriterAt lets you switch the underlying Read/WriterAt
func (wpr *Wrapper) SetReadWriterAt(rwa ReadWriterAt) {
	wpr.rwa = rwa
}

// Read reads from the current offset into p, wrapping at Cap()
func (wpr *Wrapper) Read(p []byte) (n int, err error) {
	n, err = wpr.ReadAt(p, 0)
	wpr.L -= int64(n)
	wpr.O += int64(n)
	wpr.O %= wpr.N
	return n, err
}

// ReadAt reads from the current offset+off into p, wrapping at Cap()
func (wpr *Wrapper) ReadAt(p []byte, off int64) (n int, err error) {
	wrap := NewWrapReader(wpr.rwa, wpr.O+off, wpr.N)
	r := io.LimitReader(wrap, wpr.L-off)
	return r.Read(p)
}

// Write writes p to the end of the Wrapper (at Len()), wrapping at Cap()
func (wpr *Wrapper) Write(p []byte) (n int, err error) {
	return wpr.WriteAt(p, wpr.L)
}

// WriteAt writes p at the current offset+off, wrapping at Cap()
func (wpr *Wrapper) WriteAt(p []byte, off int64) (n int, err error) {
	wrap := NewWrapWriter(wpr.rwa, wpr.O+off, wpr.N)
	w := LimitWriter(wrap, wpr.N-off)
	n, err = w.Write(p)
	if wpr.L < off+int64(n) {
		wpr.L = int64(n) + off
	}
	return n, err
}

func init() {
	gob.Register(&Wrapper{})
}

type limitedWriter struct {
	W io.Writer
	N int64
}

func (l *limitedWriter) Write(p []byte) (n int, err error) {
	if l.N <= 0 {
		return 0, io.ErrShortWrite
	}
	if int64(len(p)) > l.N {
		p = p[0:l.N]
		err = io.ErrShortWrite
	}
	n, er := l.W.Write(p)
	if er != nil {
		err = er
	}
	l.N -= int64(n)
	return n, err
}

// LimitWriter works like io.LimitReader. It writes at most n bytes
// to the underlying Writer. It returns io.ErrShortWrite if more than n
// bytes are attempted to be written.
func LimitWriter(w io.Writer, n int64) io.Writer {
	return &limitedWriter{W: w, N: n}
}

type partition struct {
	List
	Pool
}

// NewPartition returns a Buffer which uses a Pool to extend or shrink its size as needed.
// It automatically allocates new buffers with pool.Get() to extend is length, and
// pool.Put() to release unused buffers as it shrinks.
func NewPartition(pool Pool, buffers ...Buffer) Buffer {
	return &partition{
		Pool: pool,
		List: buffers,
	}
}

func (buf *partition) Cap() int64 {
	return math.MaxInt64
}

func (buf *partition) Read(p []byte) (n int, err error) {
	for len(p) > 0 {

		if len(buf.List) == 0 {
			return n, io.EOF
		}

		buffer := buf.List[0]

		if Empty(buffer) {
			buf.Pool.Put(buf.Pop())
			continue
		}

		m, er := buffer.Read(p)
		n += m
		p = p[m:]

		if er != nil && er != io.EOF {
			return n, er
		}

	}
	return n, nil
}

func (buf *partition) Write(p []byte) (n int, err error) {
	for len(p) > 0 {

		if len(buf.List) == 0 {
			next, err := buf.Pool.Get()
			if err != nil {
				return n, err
			}
			buf.Push(next)
		}

		buffer := buf.List[len(buf.List)-1]

		if Full(buffer) {
			next, err := buf.Pool.Get()
			if err != nil {
				return n, err
			}
			buf.Push(next)
			continue
		}

		m, er := buffer.Write(p)
		n += m
		p = p[m:]

		if er == io.ErrShortWrite {
			er = nil
		} else if er != nil {
			return n, er
		}

	}
	return n, nil
}

func (buf *partition) Reset() {
	for len(buf.List) > 0 {
		buf.Pool.Put(buf.Pop())
	}
}

func init() {
	gob.Register(&partition{})
}

type List []Buffer

// Len is the sum of the Len()'s of the Buffers in the List.
func (l *List) Len() (n int64) {
	for _, buffer := range *l {
		if n > math.MaxInt64-buffer.Len() {
			return math.MaxInt64
		}
		n += buffer.Len()
	}
	return n
}

// Cap is the sum of the Cap()'s of the Buffers in the List.
func (l *List) Cap() (n int64) {
	for _, buffer := range *l {
		if n > math.MaxInt64-buffer.Cap() {
			return math.MaxInt64
		}
		n += buffer.Cap()
	}
	return n
}

// Reset calls Reset() on each of the Buffers in the list.
func (l *List) Reset() {
	for _, buffer := range *l {
		buffer.Reset()
	}
}

// Push adds a Buffer to the end of the List
func (l *List) Push(b Buffer) {
	*l = append(*l, b)
}

// Pop removes and returns a Buffer from the front of the List
func (l *List) Pop() (b Buffer) {
	b = (*l)[0]
	*l = (*l)[1:]
	return b
}
