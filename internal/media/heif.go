package media

import (
	"context"
	"encoding/binary"
	"io"
	"os"
)

func parseHEIF(ctx context.Context, file io.ReaderAt, size int64, policy InspectPolicy) (avifInfo, error) {
	return parseItemImage(ctx, file, size, policy, true)
}

func inspectHEIF(ctx context.Context, file *os.File, size int64, policy InspectPolicy) (avifInfo, error) {
	info, err := parseHEIF(ctx, file, size, policy)
	if err == nil {
		err = checkHEIFGeometry(ctx, file, info, policy)
	}
	if err != nil {
		return avifInfo{}, err
	}
	return info, nil
}

// libheif reports display geometry after item transforms. The stored dimensions
// remain encoded ispe dimensions; only a proven quarter-turn may swap the check.
func checkHEIFGeometry(ctx context.Context, file *os.File, info avifInfo, policy InspectPolicy) error {
	header, err := inspectImageHeader(ctx, file, policy)
	if err != nil {
		return err
	}
	if header.loader != "heifload" {
		return errAVIFUnsupported
	}
	matches := header.width == info.width && header.height == info.height
	if info.orientation != nil && *info.orientation >= 5 {
		matches = matches || header.width == info.height && header.height == info.width
	}
	if !matches {
		return errAVIF
	}
	return nil
}

func hevcNALHeader(data []byte) (byte, error) {
	if len(data) < 3 || data[0]&0x80 != 0 || data[1]&7 == 0 {
		return 0, errAVIF
	}
	if data[0]&1 != 0 || data[1]&0xf8 != 0 {
		return 0, errAVIFUnsupported // Multilayer/dependent pictures are not a still profile.
	}
	return data[0] >> 1 & 63, nil
}

// Validate bounded decoder-record/NAL framing, not SPS/RBSP semantics or pixels.
// hvc1 parameter sets are complete out-of-band; opaque SEI is never projected.
func hevcConfiguration(data []byte) error {
	if len(data) > 64<<10 {
		return errMetadataLimit
	}
	if len(data) < 23 || data[0] != 1 {
		return errAVIF
	}
	if data[21]&3 != 3 {
		return errAVIFUnsupported
	}
	if data[22] > 16 {
		return errMetadataLimit
	}
	cursor := &avifCursor{data: data[23:]}
	seen := make(map[byte]bool)
	for range data[22] {
		kind := byte(cursor.number(1)) & 63
		if seen[kind] {
			return errAVIF
		}
		seen[kind] = true
		switch kind {
		case 32, 33, 34, 39, 40:
		default:
			return errAVIFUnsupported
		}
		count := cursor.number(2)
		if count > 64 {
			return errMetadataLimit
		}
		if count == 0 {
			return errAVIF
		}
		for range count {
			unit := cursor.take(int(cursor.number(2)))
			nalType, err := hevcNALHeader(unit)
			if err != nil {
				return err
			}
			if nalType != kind {
				return errAVIF
			}
		}
	}
	if cursor.done() != nil || !seen[32] || !seen[33] || !seen[34] {
		return errAVIF
	}
	return nil
}

// A static hvc1 item is one independently coded IRAP picture with bounded NAL
// framing. Seek past compressed bodies; never infer multiple frames from slices.
func (r *avifReader) hevcItem(item *avifItem) error {
	if len(item.extents) != 1 {
		return errAVIFUnsupported
	}
	extent := item.extents[0]
	end := extent.offset + extent.length
	picture := false
	for offset, units := extent.offset, 0; offset < end; units++ {
		if units >= 4096 {
			return errMetadataLimit
		}
		if end-offset < 4 {
			return errAVIF
		}
		prefix, err := r.read(offset, 4)
		if err != nil {
			return err
		}
		length := int64(binary.BigEndian.Uint32(prefix))
		if length < 3 || length > end-offset-4 {
			return errAVIF
		}
		header, err := r.read(offset+4, 3)
		if err != nil {
			return err
		}
		kind, err := hevcNALHeader(header)
		if err != nil {
			return err
		}
		if kind < 32 {
			if kind < 16 || kind > 21 || header[1]&7 != 1 {
				return errAVIFUnsupported
			}
			if header[2]&0x80 != 0 {
				if picture {
					return errAVIFUnsupported
				}
				picture = true
			} else if !picture {
				return errAVIF
			}
		} else if kind < 35 || kind > 40 {
			return errAVIFUnsupported
		}
		offset += 4 + length
	}
	if !picture {
		return errAVIF
	}
	return nil
}
