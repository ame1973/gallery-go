package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"math/big"
	"time"
)

var (
	errPNG            = errors.New("invalid bounded PNG metadata")
	errPNGUnsupported = errors.New("unsupported PNG critical chunk")
)

type pngInfo struct {
	animationInfo
	orientation *int64
	capture     *time.Time
}

// inspectPNG checks the complete byte-bounded PNG/APNG chunk stream and every CRC,
// not zlib/pixel contents. Unknown ancillary data is discarded. eXIf is separately
// capped at 64 KiB and only the shared TIFF whitelist is interpreted.
func inspectPNG(ctx context.Context, input io.Reader, size int64, policy InspectPolicy) (pngInfo, error) {
	if size > policy.HeaderBytes {
		return pngInfo{}, errMetadataLimit
	}
	reader := bufio.NewReaderSize(&io.LimitedReader{R: inspectReader{ctx, input}, N: policy.HeaderBytes}, 32<<10)
	var consumed int64
	read := func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(len(data)) > size-consumed {
			return errPNG
		}
		_, err := io.ReadFull(reader, data)
		consumed += int64(len(data))
		return err
	}
	var header [8]byte
	if err := read(header[:]); err != nil {
		return pngInfo{}, err
	}
	if string(header[:]) != "\x89PNG\r\n\x1a\n" {
		return pngInfo{}, errPNG
	}
	order := binary.BigEndian
	var width, height, frames, declared, idatBytes, frameBytes int64
	var depth, color byte
	var sequence uint64
	headerSeen, paletteSeen, exifSeen, idatSeen, idatClosed := false, false, false, false, false
	animation, frameOpen, frameDefault, timingKnown := false, false, false, true
	result := pngInfo{animationInfo: animationInfo{frames: 1, loops: 1}}
	var duration, delay, maximum big.Rat
	maximum.SetFrac64(int64(policy.MaxDuration), int64(time.Millisecond))
	nextSequence := func(data []byte) error {
		value := uint64(order.Uint32(data[:4]))
		if value > math.MaxInt32 || value != sequence {
			return errPNG
		}
		sequence++
		return nil
	}
	var prefix [26]byte
	var block [32 << 10]byte
	var storedCRC [4]byte
	for {
		if err := read(header[:]); err != nil {
			return pngInfo{}, err
		}
		length := int64(order.Uint32(header[:4]))
		if length > math.MaxInt32 || length > size-consumed-4 {
			return pngInfo{}, errPNG
		}
		for _, value := range header[4:] {
			if !(value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z') {
				return pngInfo{}, errPNG
			}
		}
		if header[6]&0x20 != 0 {
			return pngInfo{}, errPNG
		}
		kind := string(header[4:])
		if !headerSeen && kind != "IHDR" {
			return pngInfo{}, errPNG
		}
		if kind == "eXIf" && length > 64<<10 {
			return pngInfo{}, errMetadataLimit
		}
		checksum := crc32.NewIEEE()
		_, _ = checksum.Write(header[4:])
		data := prefix[:min(length, int64(len(prefix)))]
		if kind == "eXIf" {
			data = make([]byte, int(length))
		}
		if err := read(data); err != nil {
			return pngInfo{}, err
		}
		_, _ = checksum.Write(data)
		for remaining := length - int64(len(data)); remaining > 0; {
			slice := block[:min(remaining, int64(len(block)))]
			if err := read(slice); err != nil {
				return pngInfo{}, err
			}
			_, _ = checksum.Write(slice)
			remaining -= int64(len(slice))
		}
		if err := read(storedCRC[:]); err != nil {
			return pngInfo{}, err
		}
		if order.Uint32(storedCRC[:]) != checksum.Sum32() {
			return pngInfo{}, errPNG
		}
		if idatSeen && kind != "IDAT" {
			idatClosed = true
		}
		switch kind {
		case "IHDR":
			if headerSeen || length != 13 {
				return pngInfo{}, errPNG
			}
			width, height = int64(order.Uint32(data[:4])), int64(order.Uint32(data[4:8]))
			depth, color = data[8], data[9]
			if width < 1 || height < 1 || width > math.MaxInt32 || height > math.MaxInt32 || !pngDepth(depth, color) || data[10] != 0 || data[11] != 0 || data[12] > 1 {
				return pngInfo{}, errPNG
			}
			if width > int64(policy.MaxDimension) || height > int64(policy.MaxDimension) || width > policy.MaxPixels/height {
				return pngInfo{}, errMetadataLimit
			}
			headerSeen = true
		case "PLTE":
			if paletteSeen || idatSeen || color == 0 || color == 4 || length == 0 || length > 768 || length%3 != 0 || color == 3 && length/3 > 1<<depth {
				return pngInfo{}, errPNG
			}
			paletteSeen = true
		case "eXIf":
			if exifSeen || idatSeen {
				return pngInfo{}, errPNG
			}
			var err error
			result.orientation, result.capture, err = parseEXIF(data)
			if err != nil {
				return pngInfo{}, err
			}
			exifSeen = true
		case "acTL":
			if animation || idatSeen || length != 8 {
				return pngInfo{}, errPNG
			}
			declared = int64(order.Uint32(data[:4]))
			result.loops = int64(order.Uint32(data[4:8]))
			if declared == 0 || declared > math.MaxInt32 || result.loops > math.MaxInt32 {
				return pngInfo{}, errPNG
			}
			if declared > int64(policy.MaxFrames) {
				return pngInfo{}, errMetadataLimit
			}
			animation = true
		case "fcTL":
			if !animation || length != 26 || frameOpen && frameBytes == 0 || !idatSeen && frames > 0 || frames >= declared || idatSeen && idatBytes == 0 {
				return pngInfo{}, errPNG
			}
			if err := nextSequence(data); err != nil {
				return pngInfo{}, err
			}
			w, h := int64(order.Uint32(data[4:8])), int64(order.Uint32(data[8:12]))
			x, y := int64(order.Uint32(data[12:16])), int64(order.Uint32(data[16:20]))
			if w < 1 || h < 1 || x+w > width || y+h > height || data[24] > 2 || data[25] > 1 || !idatSeen && (x != 0 || y != 0 || w != width || h != height) {
				return pngInfo{}, errPNG
			}
			numerator, denominator := int64(order.Uint16(data[20:22])), int64(order.Uint16(data[22:24]))
			if denominator == 0 {
				denominator = 100
			}
			delay.SetFrac64(numerator*1000, denominator)
			duration.Add(&duration, &delay)
			if duration.Cmp(&maximum) > 0 {
				return pngInfo{}, errMetadataLimit
			}
			timingKnown = timingKnown && numerator > 0
			frames++
			frameOpen, frameDefault, frameBytes = true, !idatSeen, 0
		case "IDAT":
			if idatClosed || color == 3 && !paletteSeen || frameOpen && !frameDefault {
				return pngInfo{}, errPNG
			}
			idatSeen = true
			idatBytes += length
			if frameOpen {
				frameBytes += length
			}
		case "fdAT":
			if !animation || !idatSeen || !frameOpen || frameDefault || length < 4 {
				return pngInfo{}, errPNG
			}
			if err := nextSequence(data); err != nil {
				return pngInfo{}, err
			}
			frameBytes += length - 4
		case "IEND":
			if length != 0 || !idatSeen || idatBytes == 0 || consumed != size || animation && (frames != declared || !frameOpen || frameBytes == 0) {
				return pngInfo{}, errPNG
			}
			if animation {
				result.frames = frames
				if timingKnown {
					// Round the exact rational cycle once, not every individual frame.
					var quotient, remainder big.Int
					quotient.QuoRem(duration.Num(), duration.Denom(), &remainder)
					if remainder.Sign() != 0 {
						quotient.Add(&quotient, big.NewInt(1))
					}
					milliseconds := quotient.Int64()
					result.duration = &milliseconds
				}
			}
			return result, nil
		default:
			if header[4]&0x20 == 0 {
				return pngInfo{}, errPNGUnsupported
			}
		}
	}
}

func pngDepth(depth, color byte) bool {
	switch color {
	case 0:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case 2, 4, 6:
		return depth == 8 || depth == 16
	case 3:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	default:
		return false
	}
}
