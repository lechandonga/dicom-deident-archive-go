package dicom

import (
	"encoding/binary"
	"fmt"
	"io"
)

// readFileMeta parses the group-0002 file meta information, always encoded
// in Explicit VR Little Endian.
//
// (0002,0000) is optional. With a valid declaration, exactly the declared
// number of bytes are consumed as meta. Without one, elements are read
// while their tag group is 0002; the first other group is left untouched
// for the dataset walker (which matters for Explicit VR Big Endian
// datasets, whose dataset bytes are not LE).
func readFileMeta(br *reader) (map[Tag]string, syntax, error) {
	syn := littleSyntax(true)
	out := map[Tag]string{}

	first, hraw, err := readHeader(br, syn)
	if err != nil {
		if err == io.EOF {
			return nil, syntax{}, fmt.Errorf("%w: empty file meta group", ErrTruncated)
		}
		return nil, syntax{}, err
	}

	var pending *Element
	var pendingRaw []byte
	var remaining int64 = -1

	switch {
	case first.Tag == TagFileMetaInfoGroupLength && !first.Undefined && first.Length == 4:
		gl := make([]byte, 4)
		if _, err := io.ReadFull(br, gl); err != nil {
			return nil, syntax{}, fmt.Errorf("%w: meta group length value", ErrTruncated)
		}
		remaining = int64(binary.LittleEndian.Uint32(gl))
	case first.Tag == TagFileMetaInfoGroupLength:
		if _, err := io.CopyN(io.Discard, br, int64(first.Length)); err != nil {
			return nil, syntax{}, fmt.Errorf("%w: malformed meta group length", ErrTruncated)
		}
	default:
		// No group length: first element is a normal meta element.
		pending = &first
		pendingRaw = hraw
	}

	readOne := func() (Element, []byte, []byte, error) {
		if pending != nil {
			e := *pending
			r := pendingRaw
			pending, pendingRaw = nil, nil
			val := make([]byte, e.Length)
			if _, err := io.ReadFull(br, val); err != nil {
				return Element{}, nil, nil, fmt.Errorf("%w: value of meta %s", ErrTruncated, tagName(e.Tag))
			}
			return e, r, val, nil
		}
		if remaining < 0 {
			var probe [4]byte
			n, err := io.ReadFull(br, probe[:])
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return Element{}, nil, nil, io.EOF
			}
			if err != nil {
				return Element{}, nil, nil, err
			}
			if binary.LittleEndian.Uint16(probe[0:2]) != 0x0002 {
				// Dataset begins: give the tag bytes back untouched.
				br.unread = append(br.unread, probe[:n]...)
				return Element{}, nil, nil, io.EOF
			}
			br.unread = append(br.unread, probe[:n]...)
		}
		e, hdr, err := readHeader(br, syn)
		if err != nil {
			return Element{}, nil, nil, err
		}
		if e.Undefined {
			return Element{}, nil, nil, fmt.Errorf("%w: undefined-length meta element %s", ErrCorrupt, tagName(e.Tag))
		}
		if e.Length > maxBufferedValue {
			return Element{}, nil, nil, fmt.Errorf("%w: oversized meta element %s (%d)", ErrCorrupt, tagName(e.Tag), e.Length)
		}
		val := make([]byte, e.Length)
		if _, err := io.ReadFull(br, val); err != nil {
			return Element{}, nil, nil, fmt.Errorf("%w: value of meta %s", ErrTruncated, tagName(e.Tag))
		}
		return e, hdr, val, nil
	}

	for remaining != 0 {
		e, hdr, val, err := readOne()
		if err == io.EOF {
			if remaining > 0 {
				return nil, syntax{}, fmt.Errorf("%w: meta group ends %d bytes early", ErrTruncated, remaining)
			}
			break
		}
		if err != nil {
			return nil, syntax{}, err
		}
		if remaining > 0 {
			remaining -= int64(len(hdr)) + int64(len(val))
			if remaining < 0 {
				return nil, syntax{}, fmt.Errorf("%w: meta element %s overruns declared group length", ErrTruncated, tagName(e.Tag))
			}
		}
		out[e.Tag] = cleanValue(val)
	}

	tsUID := out[TagTransferSyntaxUID]
	if tsUID == "" {
		return nil, syntax{}, &MissingTagError{Tag: TagTransferSyntaxUID}
	}
	switch tsUID {
	case TSImplicitVRLittleEndian:
		return out, littleSyntax(false), nil
	case TSExplicitVRBigEndian:
		return out, syntax{explicit: true, big: true, order: binary.BigEndian}, nil
	default:
		return out, littleSyntax(true), nil
	}
}
