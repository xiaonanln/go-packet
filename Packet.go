package packetconn

import (
	"encoding/binary"
	"fmt"
	"sync"

	"unsafe"

	"sync/atomic"
)

const (
	_MIN_PAYLOAD_CAP = 128
	_CAP_GROW_SHIFT  = uint(2)

	_PACKET_POOL_CAPACITY = 8192
)

var (
	packetEndian               = binary.LittleEndian
	predefinePayloadCapacities []uint32

	packetBufferPools = map[uint32]*sync.Pool{}
	packetPool        = &sync.Pool{
		New: func() interface{} {
			p := &Packet{}
			p.bytes = p.initialBytes[:]
			return p
		},
	}
)

func init() {
	payloadCap := uint32(_MIN_PAYLOAD_CAP) << _CAP_GROW_SHIFT
	for payloadCap < _MAX_PAYLOAD_LENGTH {
		predefinePayloadCapacities = append(predefinePayloadCapacities, payloadCap)
		payloadCap <<= _CAP_GROW_SHIFT
	}
	predefinePayloadCapacities = append(predefinePayloadCapacities, _MAX_PAYLOAD_LENGTH)

	M := uint32(10 * 1024 * 1024)
	for _, payloadCap := range predefinePayloadCapacities {
		payloadCap := payloadCap
		if payloadCap <= M {
			packetBufferPools[payloadCap] = &sync.Pool{
				New: func() interface{} {
					return make([]byte, _PREPAYLOAD_SIZE+payloadCap)
				},
			}
		} else {
			packetBufferPools[payloadCap] = &sync.Pool{
				New: func() interface{} {
					return make([]byte, _PREPAYLOAD_SIZE+payloadCap)
				},
			}
		}
	}
}

func getPayloadCapOfPayloadLen(payloadLen uint32) uint32 {
	for _, payloadCap := range predefinePayloadCapacities {
		if payloadCap >= payloadLen {
			return payloadCap
		}
	}
	return _MAX_PAYLOAD_LENGTH
}

// Packet is a packet for sending data
type Packet struct {
	readCursor   uint32
	refcount     int64
	bytes        []byte
	initialBytes [_PREPAYLOAD_SIZE + _MIN_PAYLOAD_CAP]byte
}

func allocPacket() *Packet {
	pkt := packetPool.Get().(*Packet)
	pkt.refcount = 1

	if pkt.GetPayloadLen() != 0 {
		panic(fmt.Errorf("allocPacket: payload should be 0, but is %d", pkt.GetPayloadLen()))
	}

	return pkt
}

// NewPacket allocates a new packet
func NewPacket() *Packet {
	return allocPacket()
}

func (p *Packet) AssureCapacity(need uint32) {
	requireCap := p.GetPayloadLen() + need
	oldCap := p.PayloadCap()

	if requireCap <= oldCap { // most case
		return
	}

	// try to find the proper capacity for the need bytes
	resizeToCap := getPayloadCapOfPayloadLen(requireCap)

	buffer := packetBufferPools[resizeToCap].Get().([]byte)
	if len(buffer) != int(resizeToCap+_SIZE_FIELD_SIZE) {
		panic(fmt.Errorf("buffer size should be %d, but is %d", resizeToCap, len(buffer)))
	}
	copy(buffer, p.data())
	oldPayloadCap := p.PayloadCap()
	oldBytes := p.bytes
	p.bytes = buffer

	if oldPayloadCap > _MIN_PAYLOAD_CAP {
		// release old bytes
		packetBufferPools[oldPayloadCap].Put(oldBytes)
	}
}

// AddRefCount adds reference count of packet
func (p *Packet) AddRefCount(add int64) {
	atomic.AddInt64(&p.refcount, add)
}

// Payload returns the total payload of packet
func (p *Packet) Payload() []byte {
	return p.bytes[_PREPAYLOAD_SIZE : _PREPAYLOAD_SIZE+p.GetPayloadLen()]
}

// UnwrittenPayload returns the unwritten payload, which is the left payload capacity
func (p *Packet) UnwrittenPayload() []byte {
	payloadLen := p.GetPayloadLen()
	return p.bytes[_PREPAYLOAD_SIZE+payloadLen:]
}

func (p *Packet) TotalPayload() []byte {
	return p.bytes[_PREPAYLOAD_SIZE:]
}

// UnreadPayload returns the unread payload
func (p *Packet) UnreadPayload() []byte {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	return p.bytes[pos:payloadEnd]
}

// HasUnreadPayload returns if all payload is read
func (p *Packet) HasUnreadPayload() bool {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	plen := p.GetPayloadLen()
	return pos < plen
}

func (p *Packet) data() []byte {
	return p.bytes[0 : _PREPAYLOAD_SIZE+p.GetPayloadLen()]
}

// PayloadCap returns the current payload capacity
func (p *Packet) PayloadCap() uint32 {
	return uint32(len(p.bytes) - _PREPAYLOAD_SIZE)
}

// Release releases the packet to packet pool
func (p *Packet) Release() {
	refcount := atomic.AddInt64(&p.refcount, -1)

	if refcount == 0 {
		payloadCap := p.PayloadCap()
		if payloadCap > _MIN_PAYLOAD_CAP {
			buffer := p.bytes
			p.bytes = p.initialBytes[:]
			packetBufferPools[payloadCap].Put(buffer) // reclaim the buffer
		}

		p.readCursor = 0
		p.setPayloadLen(0)
		packetPool.Put(p)
	} else if refcount < 0 {
		panic(fmt.Errorf("releasing packet with refcount=%d", p.refcount))
	}
}

// ClearPayload clears packet payload
func (p *Packet) ClearPayload() {
	p.readCursor = 0
	p.setPayloadLen(0)
}

// AppendByte appends one byte to the end of payload
func (p *Packet) AppendByte(b byte) {
	p.AssureCapacity(1)
	p.bytes[_PREPAYLOAD_SIZE+p.GetPayloadLen()] = b
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) += 1
}

// ReadOneByte reads one byte from the beginning
func (p *Packet) ReadOneByte() (v byte) {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	v = p.bytes[pos]
	p.readCursor += 1
	return
}

// AppendBool appends one byte 1/0 to the end of payload
func (p *Packet) AppendBool(b bool) {
	if b {
		p.AppendByte(1)
	} else {
		p.AppendByte(0)
	}
}

// ReadBool reads one byte 1/0 from the beginning of unread payload
func (p *Packet) ReadBool() (v bool) {
	return p.ReadOneByte() != 0
}

// AppendUint16 appends one uint16 to the end of payload
func (p *Packet) AppendUint16(v uint16) {
	p.AssureCapacity(2)
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	packetEndian.PutUint16(p.bytes[payloadEnd:payloadEnd+2], v)
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) += 2
}

// AppendUint32 appends one uint32 to the end of payload
func (p *Packet) AppendUint32(v uint32) {
	p.AssureCapacity(4)
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	packetEndian.PutUint32(p.bytes[payloadEnd:payloadEnd+4], v)
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) += 4
}

// PopUint32 pops one uint32 from the end of payload
func (p *Packet) PopUint32() (v uint32) {
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	v = packetEndian.Uint32(p.bytes[payloadEnd-4 : payloadEnd])
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) -= 4
	return
}

// AppendUint64 appends one uint64 to the end of payload
func (p *Packet) AppendUint64(v uint64) {
	p.AssureCapacity(8)
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	packetEndian.PutUint64(p.bytes[payloadEnd:payloadEnd+8], v)
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) += 8
}

// PackFloat32 packs float32 in specified byte order
func PackFloat32(order binary.ByteOrder, b []byte, f float32) {
	fi := *(*uint32)(unsafe.Pointer(&f)) // convert bits from float32 to uint32
	order.PutUint32(b, fi)
}

// UnpackFloat32 unpacks float32 in specified byte order
func UnpackFloat32(order binary.ByteOrder, b []byte) (f float32) {
	fi := order.Uint32(b)
	f = *(*float32)(unsafe.Pointer(&fi))
	return
}

// AppendFloat32 appends one float32 to the end of payload
func (p *Packet) AppendFloat32(f float32) {
	p.AppendUint32(*(*uint32)(unsafe.Pointer(&f)))
}

// ReadFloat32 reads one float32 from the beginning of unread payload
func (p *Packet) ReadFloat32() float32 {
	v := p.ReadUint32()
	return *(*float32)(unsafe.Pointer(&v))
}

// AppendFloat64 appends one float64 to the end of payload
func (p *Packet) AppendFloat64(f float64) {
	p.AppendUint64(*(*uint64)(unsafe.Pointer(&f)))
}

// ReadFloat64 reads one float64 from the beginning of unread payload
func (p *Packet) ReadFloat64() float64 {
	v := p.ReadUint64()
	return *(*float64)(unsafe.Pointer(&v))
}

// AppendBytes appends slice of bytes to the end of payload
func (p *Packet) AppendBytes(v []byte) {
	bytesLen := uint32(len(v))
	p.AssureCapacity(bytesLen)
	payloadEnd := _PREPAYLOAD_SIZE + p.GetPayloadLen()
	copy(p.bytes[payloadEnd:payloadEnd+bytesLen], v)
	*(*uint32)(unsafe.Pointer(&p.bytes[0])) += bytesLen
}

// AppendVarStr appends a varsize string to the end of payload
func (p *Packet) AppendVarStr(s string) {
	p.AppendVarBytesH([]byte(s))
}

// AppendVarBytesI appends varsize bytes to the end of payload
func (p *Packet) AppendVarBytesI(v []byte) {
	p.AppendUint32(uint32(len(v)))
	p.AppendBytes(v)
}

// AppendVarBytesH appends varsize bytes to the end of payload
func (p *Packet) AppendVarBytesH(v []byte) {
	p.AppendUint16(uint16(len(v)))
	p.AppendBytes(v)
}

// ReadUint16 reads one uint16 from the beginning of unread payload
func (p *Packet) ReadUint16() (v uint16) {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	v = packetEndian.Uint16(p.bytes[pos : pos+2])
	p.readCursor += 2
	return
}

// ReadInt16 reads one int16 from the beginning of unread payload
func (p *Packet) ReadInt16() (v int16) {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	*(*uint16)(unsafe.Pointer(&v)) = packetEndian.Uint16(p.bytes[pos : pos+2])
	p.readCursor += 2
	return
}

// ReadUint32 reads one uint32 from the beginning of unread payload
func (p *Packet) ReadUint32() (v uint32) {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	v = packetEndian.Uint32(p.bytes[pos : pos+4])
	p.readCursor += 4
	return
}

// ReadUint64 reads one uint64 from the beginning of unread payload
func (p *Packet) ReadUint64() (v uint64) {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	v = packetEndian.Uint64(p.bytes[pos : pos+8])
	p.readCursor += 8
	return
}

// ReadBytes reads bytes from the beginning of unread payload
func (p *Packet) ReadBytes(size uint32) []byte {
	pos := p.readCursor + _PREPAYLOAD_SIZE
	if pos > uint32(len(p.bytes)) || pos+size > uint32(len(p.bytes)) {
		panic(fmt.Errorf("Packet %p bytes is %d, but reading %d+%d", p, len(p.bytes), pos, size))
	}

	bytes := p.bytes[pos : pos+size] // bytes are not copied
	p.readCursor += size
	return bytes
}

// ReadVarStr reads a varsize string from the beginning of unread  payload
func (p *Packet) ReadVarStr() string {
	b := p.ReadVarBytesH()
	return string(b)
}

// ReadVarBytesI reads a varsize slice of bytes from the beginning of unread payload
func (p *Packet) ReadVarBytesI() []byte {
	blen := p.ReadUint32()
	return p.ReadBytes(blen)
}

// ReadVarBytesH reads a varsize slice of bytes from the beginning of unread payload
func (p *Packet) ReadVarBytesH() []byte {
	blen := p.ReadUint16()
	return p.ReadBytes(uint32(blen))
}

func (p *Packet) AppendMapStringString(m map[string]string) {
	p.AppendUint32(uint32(len(m)))
	for k, v := range m {
		p.AppendVarStr(k)
		p.AppendVarStr(v)
	}
}

func (p *Packet) ReadMapStringString() map[string]string {
	size := p.ReadUint32()
	m := make(map[string]string, size)
	for i := uint32(0); i < size; i++ {
		k := p.ReadVarStr()
		v := p.ReadVarStr()
		m[k] = v
	}
	return m
}

// AppendStringList appends a list of strings to the end of payload
func (p *Packet) AppendStringList(list []string) {
	p.AppendUint16(uint16(len(list)))
	for _, s := range list {
		p.AppendVarStr(s)
	}
}

// ReadStringList reads a list of strings from the beginning of unread payload
func (p *Packet) ReadStringList() []string {
	listlen := int(p.ReadUint16())
	list := make([]string, listlen)
	for i := 0; i < listlen; i++ {
		list[i] = p.ReadVarStr()
	}
	return list
}

// GetPayloadLen returns the payload length
func (p *Packet) GetPayloadLen() uint32 {
	return *(*uint32)(unsafe.Pointer(&p.bytes[0]))
}

func (p *Packet) setPayloadLen(plen uint32) {
	pplen := (*uint32)(unsafe.Pointer(&p.bytes[0]))
	*pplen = plen
}