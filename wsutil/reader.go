package wsutil

import (
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"

	"github.com/dinhngocdiep/ws"
)

// ErrNoFrameAdvance means that Reader's Read() method was called without
// preceding NextFrame() call.
var ErrNoFrameAdvance = errors.New("no frame advance")

// ErrFrameTooLarge indicates that a message of length higher than
// MaxFrameSize was being read.
var ErrFrameTooLarge = errors.New("frame too large")

// FrameHandlerFunc handles parsed frame header and its body represented by
// io.Reader.
//
// Note that reader represents already unmasked body.
type FrameHandlerFunc func(ws.Header, io.Reader) error

// Reader is a wrapper around source io.Reader which represents WebSocket
// connection. It contains options for reading messages from source.
//
// Reader implements io.Reader, which Read() method reads payload of incoming
// WebSocket frames. It also takes care on fragmented frames and possibly
// intermediate control frames between them.
//
// Note that Reader's methods are not goroutine safe.
type Reader struct {
	Source io.Reader
	State  ws.State

	// SkipHeaderCheck disables checking header bits to be RFC6455 compliant.
	SkipHeaderCheck bool

	// CheckUTF8 enables UTF-8 checks for text frames payload. If incoming
	// bytes are not valid UTF-8 sequence, ErrInvalidUTF8 returned.
	CheckUTF8 bool

	// Extensions is a list of negotiated extensions for reader Source.
	// It is used to meet the specs and clear appropriate bits in fragment
	// header RSV segment.
	Extensions []RecvExtension

	// MaxFrameSize controls the maximum frame size in bytes
	// that can be read. A message exceeding that size will return
	// a ErrFrameTooLarge to the application.
	//
	// Not setting this field means there is no limit.
	MaxFrameSize int64

	OnContinuation FrameHandlerFunc
	OnIntermediate FrameHandlerFunc

	opCode ws.OpCode                  // Used to store message op code on fragmentation.
	frame  io.Reader                  // Used to as frame reader.
	raw    io.LimitedReader           // Used to discard frames without cipher.
	utf8   UTF8Reader                 // Used to check UTF8 sequences if CheckUTF8 is true.
	tmp    [ws.MaxHeaderSize - 2]byte // Used for reading headers.
	cr     *CipherReader              // Used by NextFrame() to unmask frame payload.
}

// NewReader creates new frame reader that reads from r keeping given state to
// make some protocol validity checks when it needed.
func NewReader(r io.Reader, s ws.State) *Reader {
	return &Reader{
		Source: r,
		State:  s,
	}
}

// NewClientSideReader is a helper function that calls NewReader with r and
// ws.StateClientSide.
func NewClientSideReader(r io.Reader) *Reader {
	return NewReader(r, ws.StateClientSide)
}

// NewServerSideReader is a helper function that calls NewReader with r and
// ws.StateServerSide.
func NewServerSideReader(r io.Reader) *Reader {
	return NewReader(r, ws.StateServerSide)
}

// Read implements io.Reader. It reads the next message payload into p.
// It takes care on fragmented messages.
//
// The error is io.EOF only if all of message bytes were read.
// If an io.EOF happens during reading some but not all the message bytes
// Read() returns io.ErrUnexpectedEOF.
//
// The error is ErrNoFrameAdvance if no NextFrame() call was made before
// reading next message bytes.
func (r *Reader) Read(p []byte) (n int, err error) {
	if r.frame == nil {
		if !r.fragmented() {
			// Every new Read() must be preceded by NextFrame() call.
			return 0, ErrNoFrameAdvance
		}
		// Read next continuation or intermediate control frame.
		_, err := r.NextFrame()
		if err != nil {
			return 0, err
		}
		if r.frame == nil {
			// We handled intermediate control and now got nothing to read.
			return 0, nil
		}
	}

	n, err = r.frame.Read(p)
	if err != nil && err != io.EOF {
		return n, err
	}
	if err == nil && r.raw.N != 0 {
		return n, nil
	}

	// EOF condition (either err is io.EOF or r.raw.N is zero).
	switch {
	case r.raw.N != 0:
		err = io.ErrUnexpectedEOF

	case r.fragmented():
		err = nil
		r.resetFragment()

	case r.CheckUTF8 && !r.utf8.Valid():
		// NOTE: check utf8 only when full message received, since partial
		// reads may be invalid.
		n = r.utf8.Accepted()
		err = ErrInvalidUTF8

	default:
		r.reset()
		err = io.EOF
	}

	return n, err
}

// Discard discards current message unread bytes.
// It discards all frames of fragmented message.
func (r *Reader) Discard() (err error) {
	for {
		_, err = io.Copy(ioutil.Discard, &r.raw)
		if err != nil {
			break
		}
		if !r.fragmented() {
			break
		}
		if _, err = r.NextFrame(); err != nil {
			break
		}
	}
	r.reset()
	return err
}

// NextFrame prepares r to read next message. It returns received frame header
// and non-nil error on failure.
//
// Note that next NextFrame() call must be done after receiving or discarding
// all current message bytes.
func (r *Reader) NextFrame() (hdr ws.Header, err error) {
	hdr, err = r.readHeader(r.Source)
	if err == io.EOF && r.fragmented() {
		// If we are in fragmented state EOF means that is was totally
		// unexpected.
		//
		// NOTE: This is necessary to prevent callers such that
		// ioutil.ReadAll to receive some amount of bytes without an error.
		// ReadAll() ignores an io.EOF error, thus caller may think that
		// whole message fetched, but actually only part of it.
		err = io.ErrUnexpectedEOF
	}
	if err == nil && !r.SkipHeaderCheck {
		err = ws.CheckHeader(hdr, r.State)
	}
	if err != nil {
		return hdr, err
	}

	if n := r.MaxFrameSize; n > 0 && hdr.Length > n {
		return hdr, ErrFrameTooLarge
	}

	// Save raw reader to use it on discarding frame without ciphering and
	// other streaming checks.
	r.raw = io.LimitedReader{
		R: r.Source,
		N: hdr.Length,
	}

	frame := io.Reader(&r.raw)
	if hdr.Masked {
		if r.cr == nil {
			r.cr = NewCipherReader(frame, hdr.Mask)
		} else {
			r.cr.Reset(frame, hdr.Mask)
		}
		frame = r.cr
	}

	for _, x := range r.Extensions {
		hdr, err = x.UnsetBits(hdr)
		if err != nil {
			return hdr, err
		}
	}

	if r.fragmented() {
		if hdr.OpCode.IsControl() {
			if cb := r.OnIntermediate; cb != nil {
				err = cb(hdr, frame)
			}
			if err == nil {
				// Ensure that src is empty.
				_, err = io.Copy(ioutil.Discard, &r.raw)
			}
			return hdr, err
		}
	} else {
		r.opCode = hdr.OpCode
	}
	if r.CheckUTF8 && (hdr.OpCode == ws.OpText || (r.fragmented() && r.opCode == ws.OpText)) {
		r.utf8.Source = frame
		frame = &r.utf8
	}

	// Save reader with ciphering and other streaming checks.
	r.frame = frame

	if hdr.OpCode == ws.OpContinuation {
		if cb := r.OnContinuation; cb != nil {
			err = cb(hdr, frame)
		}
	}

	if hdr.Fin {
		r.State = r.State.Clear(ws.StateFragmented)
	} else {
		r.State = r.State.Set(ws.StateFragmented)
	}

	return hdr, err
}

func (r *Reader) fragmented() bool {
	return r.State.Fragmented()
}

func (r *Reader) resetFragment() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	// Reset source of the UTF8Reader, but not the state.
	r.utf8.Source = nil
}

func (r *Reader) reset() {
	r.raw = io.LimitedReader{}
	r.frame = nil
	r.utf8 = UTF8Reader{}
	r.opCode = 0
}

// readHeader reads a frame header from in.
func (r *Reader) readHeader(in io.Reader) (h ws.Header, err error) {
	// Make slice of bytes with capacity 12 that could hold any header.
	//
	// The maximum header size is 14, but due to the 2 hop reads,
	// after first hop that reads first 2 constant bytes, we could reuse 2 bytes.
	// So 14 - 2 = 12.
	bts := r.tmp[:2]

	// Prepare to hold first 2 bytes to choose size of next read.
	_, err = io.ReadFull(in, bts)
	if err != nil {
		return h, err
	}
	const bit0 = 0x80

	h.Fin = bts[0]&bit0 != 0
	h.Rsv = (bts[0] & 0x70) >> 4
	h.OpCode = ws.OpCode(bts[0] & 0x0f)

	var extra int

	if bts[1]&bit0 != 0 {
		h.Masked = true
		extra += 4
	}

	length := bts[1] & 0x7f
	switch {
	case length < 126:
		h.Length = int64(length)

	case length == 126:
		extra += 2

	case length == 127:
		extra += 8

	default:
		err = ws.ErrHeaderLengthUnexpected
		return h, err
	}

	if extra == 0 {
		return h, err
	}

	// Increase len of bts to extra bytes need to read.
	// Overwrite first 2 bytes that was read before.
	bts = bts[:extra]
	_, err = io.ReadFull(in, bts)
	if err != nil {
		return h, err
	}

	switch {
	case length == 126:
		h.Length = int64(binary.BigEndian.Uint16(bts[:2]))
		bts = bts[2:]

	case length == 127:
		if bts[0]&0x80 != 0 {
			err = ws.ErrHeaderLengthMSB
			return h, err
		}
		h.Length = int64(binary.BigEndian.Uint64(bts[:8]))
		bts = bts[8:]
	}

	if h.Masked {
		copy(h.Mask[:], bts)
	}

	return h, nil
}

// NextReader prepares next message read from r. It returns header that
// describes the message and io.Reader to read message's payload. It returns
// non-nil error when it is not possible to read message's initial frame.
//
// Note that next NextReader() on the same r should be done after reading all
// bytes from previously returned io.Reader. For more performant way to discard
// message use Reader and its Discard() method.
//
// Note that it will not handle any "intermediate" frames, that possibly could
// be received between text/binary continuation frames. That is, if peer sent
// text/binary frame with fin flag "false", then it could send ping frame, and
// eventually remaining part of text/binary frame with fin "true" – with
// NextReader() the ping frame will be dropped without any notice. To handle
// this rare, but possible situation (and if you do not know exactly which
// frames peer could send), you could use Reader with OnIntermediate field set.
func NextReader(r io.Reader, s ws.State) (ws.Header, io.Reader, error) {
	rd := &Reader{
		Source: r,
		State:  s,
	}
	header, err := rd.NextFrame()
	if err != nil {
		return header, nil, err
	}
	return header, rd, nil
}
