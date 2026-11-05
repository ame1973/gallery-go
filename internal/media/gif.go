package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
)

var (
	errGIF            = errors.New("invalid bounded GIF metadata")
	errGIFUnsupported = errors.New("unsupported GIF rendering extension")
)

// inspectGIF walks a complete bounded container without allocating frame pixels or
// decoding LZW. Timing is unknown if any frame lacks a positive delay or waits for
// user input. Loops are total playthroughs (0 means infinite), not repeat counts.
func inspectGIF(ctx context.Context, input io.Reader, size int64, policy InspectPolicy) (animationInfo, error) {
	if size > policy.HeaderBytes {
		return animationInfo{}, errMetadataLimit
	}
	reader := bufio.NewReaderSize(&io.LimitedReader{R: inspectReader{ctx, input}, N: policy.HeaderBytes}, 32<<10)
	var consumed int64
	read := func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(len(data)) > size-consumed {
			return errGIF
		}
		_, err := io.ReadFull(reader, data)
		consumed += int64(len(data))
		return err
	}
	var buffer [768]byte
	blocks := func() (int64, error) {
		var total int64
		for {
			if err := read(buffer[:1]); err != nil {
				return 0, err
			}
			length := int(buffer[0])
			if length == 0 {
				return total, nil
			}
			if err := read(buffer[:length]); err != nil {
				return 0, err
			}
			total += int64(length)
		}
	}
	if err := read(buffer[:13]); err != nil {
		return animationInfo{}, err
	}
	if string(buffer[:6]) != "GIF87a" && string(buffer[:6]) != "GIF89a" {
		return animationInfo{}, errGIF
	}
	order := binary.LittleEndian
	width, height := int64(order.Uint16(buffer[6:8])), int64(order.Uint16(buffer[8:10]))
	global := buffer[10]&0x80 != 0
	if width < 1 || height < 1 {
		return animationInfo{}, errGIF
	}
	if width > int64(policy.MaxDimension) || height > int64(policy.MaxDimension) || width > policy.MaxPixels/height {
		return animationInfo{}, errMetadataLimit
	}
	if global {
		length := 3 * (2 << (buffer[10] & 7))
		if err := read(buffer[:length]); err != nil {
			return animationInfo{}, err
		}
	}
	result := animationInfo{loops: 1}
	var duration, delay int64
	known, control, userInput, loopSeen := true, false, false, false
	for {
		if err := read(buffer[:1]); err != nil {
			return animationInfo{}, err
		}
		switch buffer[0] {
		case 0x3b: // Trailer. No concatenated streams or unaccounted trailing input.
			if result.frames == 0 || control || consumed != size {
				return animationInfo{}, errGIF
			}
			if known {
				result.duration = &duration
			}
			return result, nil
		case 0x21:
			if err := read(buffer[:1]); err != nil {
				return animationInfo{}, err
			}
			switch buffer[0] {
			case 0xf9: // Graphic Control Extension applies only to the next image.
				if err := read(buffer[:6]); err != nil {
					return animationInfo{}, err
				}
				if control || buffer[0] != 4 || buffer[5] != 0 || buffer[1]&0xe0 != 0 || (buffer[1]>>2)&7 > 3 {
					return animationInfo{}, errGIF
				}
				control, userInput = true, buffer[1]&2 != 0
				delay = int64(order.Uint16(buffer[2:4])) * 10
			case 0xfe: // Comments are private uninterpreted sub-blocks.
				if _, err := blocks(); err != nil {
					return animationInfo{}, err
				}
			case 0xff:
				if err := read(buffer[:1]); err != nil {
					return animationInfo{}, err
				}
				length := int(buffer[0])
				if length != 11 {
					return animationInfo{}, errGIF
				}
				if err := read(buffer[:length]); err != nil {
					return animationInfo{}, err
				}
				application := string(buffer[:length])
				if application == "NETSCAPE2.0" || application == "ANIMEXTS1.0" {
					if loopSeen || result.frames > 0 {
						return animationInfo{}, errGIF
					}
					if err := read(buffer[:5]); err != nil {
						return animationInfo{}, err
					}
					if buffer[0] != 3 || buffer[1] != 1 || buffer[4] != 0 {
						return animationInfo{}, errGIF
					}
					result.loops = int64(order.Uint16(buffer[2:4]))
					if result.loops > 0 {
						result.loops++
					}
					loopSeen = true
				} else if _, err := blocks(); err != nil {
					return animationInfo{}, err
				}
			default:
				// Plain Text and unknown rendering extensions cannot prove frame timing.
				return animationInfo{}, errGIFUnsupported
			}
		case 0x2c:
			if result.frames >= int64(policy.MaxFrames) {
				return animationInfo{}, errMetadataLimit
			}
			if err := read(buffer[:9]); err != nil {
				return animationInfo{}, err
			}
			x, y := int64(order.Uint16(buffer[:2])), int64(order.Uint16(buffer[2:4]))
			w, h := int64(order.Uint16(buffer[4:6])), int64(order.Uint16(buffer[6:8]))
			local := buffer[8]&0x80 != 0
			if w < 1 || h < 1 || x+w > width || y+h > height || buffer[8]&0x18 != 0 || !local && !global {
				return animationInfo{}, errGIF
			}
			if local {
				length := 3 * (2 << (buffer[8] & 7))
				if err := read(buffer[:length]); err != nil {
					return animationInfo{}, err
				}
			}
			if err := read(buffer[:1]); err != nil {
				return animationInfo{}, err
			}
			if buffer[0] < 2 || buffer[0] > 8 {
				return animationInfo{}, errGIF
			}
			length, err := blocks()
			if err != nil {
				return animationInfo{}, err
			}
			if length == 0 {
				return animationInfo{}, errGIF
			}
			if delay > policy.MaxDuration.Milliseconds()-duration {
				return animationInfo{}, errMetadataLimit
			}
			duration += delay
			known = known && control && delay > 0 && !userInput
			result.frames++
			control, userInput, delay = false, false, 0
		default:
			return animationInfo{}, errGIF
		}
	}
}
