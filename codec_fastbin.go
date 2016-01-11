package link

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

var ErrFastbinTooLarge = errors.New("funny/link: too large fastbin packet")

// FbMessage interface is the codec methods that generated by fastbin.
type FbMessage interface {
	ServiceID() byte
	MessageID() byte
	BinarySize() int
	MarshalPacket([]byte)
	UnmarshalPacket([]byte)
}

// FbHandler is the request handler returns by FbService for each request.
type FbHandler func(*Session, FbMessage)

// FbService interface is the service methods that generated by fastbin.
type FbService interface {
	ServiceID() byte
	NewRequest(id byte) (FbMessage, FbHandler)
}

// FbRequest is a wrapper for each incoming fastbin message.
// It make Session.Receive() can returns message with its handler.
type FbRequest struct {
	message FbMessage
	handler FbHandler
}

// Procss the request.
func (req *FbRequest) Process(s *Session) {
	req.handler(s, req.message)
}

// Allocator provide a way to pooling memory.
// Reference: https://github.com/funny/slab
type Allocator interface {
	Alloc(int) []byte
	Free([]byte)
}

type nonAllocator struct{}

func (_ nonAllocator) Alloc(size int) []byte {
	return make([]byte, size)
}

func (_ nonAllocator) Free(_ []byte) {}

// Fastbin create a fastbin codec type.
// bufioSize is used for bufio.NewReaderSize().
// allocator is used for pooling memory.
// If allocator is nil, will allocat memory by make().
func Fastbin(bufioSize int, allocator Allocator) *FbCodecType {
	if allocator == nil {
		allocator = nonAllocator{}
	}
	return &FbCodecType{
		allocator: allocator,
		bufioSize: bufioSize,
	}
}

// FbCodecType is a codec type work with fastbin.
// Reference: https://github.com/funny/fastbin
type FbCodecType struct {
	bufioSize  int
	readerPool sync.Pool
	allocator  Allocator
	services   [256]FbService
}

// Register add a fastbin service into codec type.
func (ct *FbCodecType) Register(service FbService) {
	id := service.ServiceID()
	if ct.services[id] != nil {
		panic(fmt.Sprintf("Duplicate service ID: %d", id))
	}
	ct.services[id] = service
}

// NewEncoder implements CodecType.NewEncoder().
func (ct *FbCodecType) NewEncoder(w io.Writer) Encoder {
	return &fbEncoder{
		parent:    ct,
		writer:    w,
		allocator: ct.allocator,
	}
}

// NewDecoder implements CodecType.NewEncoder().
func (ct *FbCodecType) NewDecoder(r io.Reader) Decoder {
	reader, ok := ct.readerPool.Get().(*bufio.Reader)
	if ok {
		reader.Reset(r)
	} else {
		reader = bufio.NewReaderSize(r, ct.bufioSize)
	}
	return &fbDecoder{
		parent:     ct,
		reader:     reader,
		readerPool: &ct.readerPool,
		allocator:  ct.allocator,
	}
}

const fbHeadSize = 4

type fbEncoder struct {
	parent    *FbCodecType
	writer    io.Writer
	allocator Allocator
}

type fbDecoder struct {
	parent     *FbCodecType
	head       [fbHeadSize]byte
	reader     *bufio.Reader
	readerPool *sync.Pool
	allocator  Allocator
}

func (encoder *fbEncoder) Encode(msg interface{}) (err error) {
	rsp := msg.(FbMessage)

	n := rsp.BinarySize()
	if n > math.MaxUint16 {
		panic(ErrFastbinTooLarge)
	}

	b := encoder.allocator.Alloc(n + fbHeadSize)
	defer encoder.allocator.Free(b)

	binary.LittleEndian.PutUint16(b, uint16(n))
	b[2] = rsp.ServiceID()
	b[3] = rsp.MessageID()
	rsp.MarshalPacket(b[fbHeadSize:])
	_, err = encoder.writer.Write(b)
	return
}

func (decoder *fbDecoder) Decode(msg interface{}) (err error) {
	head := decoder.head[:]
	if _, err = io.ReadFull(decoder.reader, head); err != nil {
		return
	}
	n := int(binary.LittleEndian.Uint16(head))

	var b []byte

	if decoder.reader.Buffered() >= n {
		b, err = decoder.reader.Peek(n)
		if err != nil {
			return
		}
		defer func() {
			_, err = decoder.reader.Discard(n)
		}()
	} else {
		b = decoder.allocator.Alloc(n)
		defer decoder.allocator.Free(b)
		if _, err = io.ReadFull(decoder.reader, b); err != nil {
			return
		}
	}

	req := msg.(*FbRequest)
	req.message, req.handler = decoder.parent.services[head[2]].NewRequest(head[3])
	req.message.UnmarshalPacket(b)
	return
}

func (decoder *fbDecoder) Dispose() {
	decoder.reader.Reset(nil)
	decoder.readerPool.Put(decoder.reader)
}