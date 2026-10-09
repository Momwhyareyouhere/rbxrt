package apk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// errNoSigningBlock is returned when an APK has no v2/v3 signing block before
// its central directory. Callers may fall back to inspecting v1 JAR signatures.
var errNoSigningBlock = errors.New("apk: no APK signing block")

// eocd is the location and size of the ZIP End of Central Directory record.
type eocd struct {
	offset   int64 // absolute file offset of the EOCD record
	size     int64 // record size including its comment
	cdOffset int64 // absolute file offset of the central directory
	cdSize   int64 // central directory size in bytes
}

// findEOCD locates the EOCD record. It must be the last thing in the file and
// must be found within 65,557 bytes of the end (the ZIP format's own limit).
func findEOCD(r io.ReaderAt, fileSize int64) (*eocd, error) {
	window := fileSize
	if max := int64(65557 + 22); window > max {
		window = max
	}
	start := fileSize - window
	buf := make([]byte, window)
	if _, err := r.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	for p := len(buf) - 22; p >= 0; p-- {
		if binary.LittleEndian.Uint32(buf[p:]) != 0x06054b50 {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(buf[p+20:]))
		if p+22+commentLen != len(buf) {
			// Something follows this candidate; it is not the real EOCD.
			continue
		}
		cdSize := int64(binary.LittleEndian.Uint32(buf[p+12:]))
		cdOffset := int64(binary.LittleEndian.Uint32(buf[p+16:]))
		if cdOffset == 0xffffffff || cdSize == 0xffffffff || cdOffset+cdSize != start+int64(p) {
			if cdOffset == 0xffffffff || cdSize == 0xffffffff {
				return nil, fmt.Errorf("apk: zip64 EOCD records are not supported yet")
			}
			continue
		}
		return &eocd{
			offset:   start + int64(p),
			size:     22 + int64(commentLen),
			cdOffset: cdOffset,
			cdSize:   cdSize,
		}, nil
	}
	return nil, fmt.Errorf("apk: no End of Central Directory record found in the last %d bytes", window)
}

// signingBlockLayout describes the APK signing block and where the sections it
// protects begin and end.
type signingBlockLayout struct {
	start int64 // offset of the block's leading size field
	end   int64 // offset of the central directory (== cdOffset)
	pairs map[uint32][]byte
}

// findSigningBlock walks back from the central directory to the APK signing
// block and parses its ID-value pairs.
func findSigningBlock(r io.ReaderAt, cdOffset int64) (*signingBlockLayout, error) {
	if cdOffset < 24+8 {
		return nil, errNoSigningBlock
	}
	footer := make([]byte, 24)
	if _, err := r.ReadAt(footer, cdOffset-24); err != nil {
		return nil, err
	}
	if string(footer[8:]) != "APK Sig Block 42" {
		return nil, errNoSigningBlock
	}
	blockSize := int64(binary.LittleEndian.Uint64(footer[:8]))
	if blockSize < 24 {
		return nil, fmt.Errorf("apk: implausible signing block size %d", blockSize)
	}
	blockStart := cdOffset - blockSize - 8
	if blockStart < 0 {
		return nil, fmt.Errorf("apk: signing block size overruns the file")
	}
	lead := make([]byte, 8)
	if _, err := r.ReadAt(lead, blockStart); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint64(lead) != uint64(blockSize) {
		return nil, fmt.Errorf("apk: signing block size fields disagree (tampered?)")
	}

	pairs := map[uint32][]byte{}
	pos := blockStart + 8
	end := cdOffset - 24
	buf := make([]byte, end-pos)
	if _, err := r.ReadAt(buf, pos); err != nil {
		return nil, err
	}
	for off := 0; off < len(buf); {
		if off+8 > len(buf) {
			return nil, fmt.Errorf("apk: truncated ID-value pair")
		}
		pairLen := int64(binary.LittleEndian.Uint64(buf[off:]))
		off += 8
		if pairLen < 4 || int64(off)+pairLen > int64(len(buf)) {
			return nil, fmt.Errorf("apk: bad pair length %d", pairLen)
		}
		id := binary.LittleEndian.Uint32(buf[off:])
		pairs[id] = append([]byte(nil), buf[off+4:off+int(pairLen)]...)
		off += int(pairLen)
	}
	return &signingBlockLayout{start: blockStart, end: cdOffset, pairs: pairs}, nil
}
