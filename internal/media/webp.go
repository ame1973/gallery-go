package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"time"
)

var errWebP = errors.New("invalid bounded WebP metadata")

type webpInfo struct {
	animationInfo
	width, height int64
	orientation   *int64
	capture       *time.Time
}

type webpChunk struct {
	kind        string
	length, end int64
}

type webpReader struct {
	ctx        context.Context
	input      io.Reader
	size, used int64
	buffer     [32 << 10]byte
}

func (r *webpReader) read(data []byte) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > r.size-r.used {
		return errWebP
	}
	_, err := io.ReadFull(r.input, data)
	r.used += int64(len(data))
	return err
}

func (r *webpReader) chunk(end int64) (webpChunk, error) {
	if end-r.used < 8 {
		return webpChunk{}, errWebP
	}
	var header [8]byte
	if err := r.read(header[:]); err != nil {
		return webpChunk{}, err
	}
	length := int64(binary.LittleEndian.Uint32(header[4:]))
	if length+(length&1) > end-r.used {
		return webpChunk{}, errWebP
	}
	return webpChunk{string(header[:4]), length, r.used + length}, nil
}

func (r *webpReader) padding(chunk webpChunk) error {
	if r.used != chunk.end {
		return errWebP
	}
	if chunk.length&1 != 0 {
		var pad [1]byte
		if err := r.read(pad[:]); err != nil {
			return err
		}
		if pad[0] != 0 {
			return errWebP
		}
	}
	return nil
}

func (r *webpReader) payload(chunk webpChunk, keep int64) ([]byte, error) {
	data := make([]byte, int(min(chunk.length, keep)))
	if err := r.read(data); err != nil {
		return nil, err
	}
	for r.used < chunk.end {
		part := r.buffer[:min(chunk.end-r.used, int64(len(r.buffer)))]
		if err := r.read(part); err != nil {
			return nil, err
		}
	}
	return data, r.padding(chunk)
}

func webp24(data []byte) int64 {
	return int64(data[0]) | int64(data[1])<<8 | int64(data[2])<<16
}

// webpDimensions reads only key-frame headers, never entropy-coded pixels.
func webpDimensions(kind string, data []byte, length int64) (int64, int64, error) {
	switch kind {
	case "VP8 ":
		if length <= 10 || len(data) < 10 || data[0]&1 != 0 || (data[0]>>1)&7 > 3 || data[0]&0x10 == 0 || !bytes.Equal(data[3:6], []byte{0x9d, 0x01, 0x2a}) {
			return 0, 0, errWebP
		}
		width, height := int64(binary.LittleEndian.Uint16(data[6:8])&0x3fff), int64(binary.LittleEndian.Uint16(data[8:10])&0x3fff)
		if width == 0 || height == 0 {
			return 0, 0, errWebP
		}
		return width, height, nil
	case "VP8L":
		if length <= 5 || len(data) < 5 || data[0] != 0x2f {
			return 0, 0, errWebP
		}
		bits := binary.LittleEndian.Uint32(data[1:5])
		if bits>>29 != 0 {
			return 0, 0, errWebP
		}
		return int64(bits&0x3fff) + 1, int64((bits>>14)&0x3fff) + 1, nil
	default:
		return 0, 0, errWebP
	}
}

// webpFrame checks one ANMF payload's alpha/bitstream order and exact rectangle.
// Unknown subchunks are permitted after the bitstream, never recursively decoded.
func webpFrame(r *webpReader, end, width, height int64) error {
	alpha, image := false, false
	for r.used < end {
		chunk, err := r.chunk(end)
		if err != nil {
			return err
		}
		data, err := r.payload(chunk, 10)
		if err != nil {
			return err
		}
		switch chunk.kind {
		case "ALPH":
			if alpha || image || chunk.length == 0 {
				return errWebP
			}
			alpha = true
		case "VP8 ", "VP8L":
			if image || alpha && chunk.kind == "VP8L" {
				return errWebP
			}
			w, h, err := webpDimensions(chunk.kind, data, chunk.length)
			if err != nil || w != width || h != height {
				return errWebP
			}
			image = true
		default:
			if !image {
				return errWebP
			}
		}
	}
	if !image || r.used != end {
		return errWebP
	}
	return nil
}

// inspectWebP streams the complete bounded RIFF extent without decoding pixels.
// WebP loop counts are already total plays; reserved VP8X/ANMF bits are ignored as
// required by the format. Source hash/revision binding remains the caller's duty.
func inspectWebP(ctx context.Context, input io.Reader, size int64, policy InspectPolicy) (webpInfo, error) {
	if size > policy.HeaderBytes {
		return webpInfo{}, errMetadataLimit
	}
	r := &webpReader{ctx: ctx, input: bufio.NewReaderSize(inspectReader{ctx, input}, 32<<10), size: size}
	var header [12]byte
	if err := r.read(header[:]); err != nil {
		return webpInfo{}, err
	}
	declared := int64(binary.LittleEndian.Uint32(header[4:8]))
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" || declared+8 != size || declared&1 != 0 || declared > math.MaxUint32-9 {
		return webpInfo{}, errWebP
	}
	result := webpInfo{animationInfo: animationInfo{frames: 1, loops: 1}}
	var flags byte
	extended, profile, animated, image, alpha, exif := false, false, false, false, false, false
	var frames, duration int64
	known := true
	for r.used < size {
		chunk, err := r.chunk(size)
		if err != nil {
			return webpInfo{}, err
		}
		if chunk.kind == "ANMF" && extended && flags&2 != 0 {
			if !animated || image || chunk.length < 16 {
				return webpInfo{}, errWebP
			}
			if frames >= int64(policy.MaxFrames) {
				return webpInfo{}, errMetadataLimit
			}
			var frame [16]byte
			if err := r.read(frame[:]); err != nil {
				return webpInfo{}, err
			}
			x, y := webp24(frame[:3])*2, webp24(frame[3:6])*2
			w, h := webp24(frame[6:9])+1, webp24(frame[9:12])+1
			if x+w > result.width || y+h > result.height {
				return webpInfo{}, errWebP
			}
			milliseconds := webp24(frame[12:15])
			if milliseconds > policy.MaxDuration.Milliseconds()-duration {
				return webpInfo{}, errMetadataLimit
			}
			if err := webpFrame(r, chunk.end, w, h); err != nil {
				return webpInfo{}, err
			}
			if err := r.padding(chunk); err != nil {
				return webpInfo{}, err
			}
			duration += milliseconds
			known = known && milliseconds > 0
			frames++
			continue
		}
		keep := int64(10)
		if chunk.kind == "EXIF" {
			if chunk.length > 64<<10 {
				return webpInfo{}, errMetadataLimit
			}
			keep = chunk.length
		}
		data, err := r.payload(chunk, keep)
		if err != nil {
			return webpInfo{}, err
		}
		switch chunk.kind {
		case "VP8X":
			if extended || image || animated || chunk.length < 10 {
				return webpInfo{}, errWebP
			}
			flags = data[0]
			result.width, result.height = webp24(data[4:7])+1, webp24(data[7:10])+1
			if result.width*result.height > math.MaxUint32 {
				return webpInfo{}, errWebP
			}
			extended = true
		case "ICCP":
			if !extended || profile || image || animated || frames > 0 {
				return webpInfo{}, errWebP
			}
			profile = true
		case "ANIM":
			if extended && flags&2 != 0 {
				if animated || frames > 0 || chunk.length < 6 {
					return webpInfo{}, errWebP
				}
				result.loops = int64(binary.LittleEndian.Uint16(data[4:6]))
				animated = true
			}
		case "ALPH":
			if !extended || flags&2 != 0 || alpha || image || chunk.length == 0 {
				return webpInfo{}, errWebP
			}
			alpha = true
		case "VP8 ", "VP8L":
			if image || flags&2 != 0 || alpha && chunk.kind == "VP8L" {
				return webpInfo{}, errWebP
			}
			w, h, err := webpDimensions(chunk.kind, data, chunk.length)
			if err != nil || extended && (w != result.width || h != result.height) {
				return webpInfo{}, errWebP
			}
			result.width, result.height, image = w, h, true
		case "EXIF":
			if exif {
				return webpInfo{}, errWebP
			}
			if bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
				data = data[6:]
			}
			result.orientation, result.capture, err = parseEXIF(data)
			if err != nil {
				return webpInfo{}, err
			}
			exif = true
		}
		if result.width > int64(policy.MaxDimension) || result.height > int64(policy.MaxDimension) || result.height > 0 && result.width > policy.MaxPixels/result.height {
			return webpInfo{}, errMetadataLimit
		}
	}
	if extended && flags&2 != 0 {
		if !animated || frames == 0 || image {
			return webpInfo{}, errWebP
		}
		result.frames = frames
		if known {
			result.duration = &duration
		}
	} else if !image {
		return webpInfo{}, errWebP
	}
	if extended && (flags&8 != 0 && !exif || flags&32 != 0 && !profile) {
		return webpInfo{}, errWebP
	}
	return result, nil
}
