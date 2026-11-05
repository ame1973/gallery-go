package media

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"time"
)

var (
	errTIFF            = errors.New("invalid bounded TIFF metadata")
	errTIFFUnsupported = errors.New("TIFF metadata profile is unavailable")
)

type tiffInfo struct {
	width, height int64
	orientation   *int64
	capture       *time.Time
}

func inspectTIFF(ctx context.Context, file *os.File, size int64, policy InspectPolicy) (tiffInfo, error) {
	info, err := parseTIFF(ctx, file, size, policy)
	if err != nil {
		return tiffInfo{}, err
	}
	header, err := inspectImageHeader(ctx, file, policy)
	if err != nil {
		return tiffInfo{}, err
	}
	if header.loader != "tiffload" {
		return tiffInfo{}, errTIFFUnsupported
	}
	if header.width != info.width || header.height != info.height {
		return tiffInfo{}, errTIFF
	}
	return info, nil
}

// Parse only classic TIFF's first full-resolution page and its EXIF whitelist.
// Other page directories are bounded/cycle-checked, not interpreted as animation.
// Compressed pixels and private/unknown tag payloads are never copied or followed.
func parseTIFF(ctx context.Context, input io.ReaderAt, size int64, policy InspectPolicy) (tiffInfo, error) {
	remaining := policy.HeaderBytes
	read := func(offset, length int64) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if offset < 0 || length < 0 || offset > size || length > size-offset {
			return nil, errTIFF
		}
		if length > remaining {
			return nil, errMetadataLimit
		}
		remaining -= length
		data := make([]byte, int(length))
		if _, err := input.ReadAt(data, offset); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return data, nil
	}
	header, err := read(0, 8)
	if err != nil {
		return tiffInfo{}, err
	}
	var order binary.ByteOrder
	switch string(header[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return tiffInfo{}, errTIFF
	}
	if order.Uint16(header[2:4]) == 43 {
		return tiffInfo{}, errTIFFUnsupported
	}
	if order.Uint16(header[2:4]) != 42 {
		return tiffInfo{}, errTIFF
	}
	if size >= 12 {
		signature, err := read(8, 4)
		if err != nil {
			return tiffInfo{}, err
		}
		if string(signature) == "CR\x02\x00" {
			return tiffInfo{}, errTIFFUnsupported
		}
	}
	first := order.Uint32(header[4:8])
	if first == 0 {
		return tiffInfo{}, errTIFF
	}
	seen := make(map[uint32]bool)
	var info tiffInfo
	for offset := first; offset != 0; {
		if seen[offset] {
			return tiffInfo{}, errTIFF
		}
		if len(seen) >= min(policy.MaxFrames, 4096) {
			return tiffInfo{}, errMetadataLimit
		}
		seen[offset] = true
		entries, next, err := readTIFFDirectory(read, order, offset)
		if err != nil {
			return tiffInfo{}, err
		}
		if offset == first {
			info, err = tiffPrimary(entries, order, policy)
			if err == nil {
				err = tiffImageExtents(read, entries, order, size)
			}
			if err != nil {
				return tiffInfo{}, err
			}
		}
		offset = next
	}
	info.orientation, info.capture, err = readEXIF(read)
	if err != nil {
		return tiffInfo{}, err
	}
	return info, nil
}

// TIFF stores local compressed pixels in strips or tiles. Bound both vectors and
// verify their declared extents without reading/allocating the pixel payloads.
func tiffImageExtents(read func(int64, int64) ([]byte, error), entries []byte, order binary.ByteOrder, size int64) error {
	fields := make(map[uint16][]byte)
	for len(entries) > 0 {
		entry := entries[:12]
		entries = entries[12:]
		switch tag := order.Uint16(entry[:2]); tag {
		case 0x0111, 0x0117, 0x0144, 0x0145:
			fields[tag] = entry
		}
	}
	offsets, lengths := fields[0x0111], fields[0x0117]
	if fields[0x0144] != nil || fields[0x0145] != nil {
		if offsets != nil || lengths != nil {
			return errTIFF
		}
		offsets, lengths = fields[0x0144], fields[0x0145]
	}
	if offsets == nil || lengths == nil {
		return errTIFF
	}
	vector := func(entry []byte) ([]byte, int, error) {
		width := 0
		switch order.Uint16(entry[2:4]) {
		case 3:
			width = 2
		case 4:
			width = 4
		default:
			return nil, 0, errTIFFUnsupported
		}
		count := int64(order.Uint32(entry[4:8]))
		if count == 0 {
			return nil, 0, errTIFF
		}
		if count > 1<<20 {
			return nil, 0, errMetadataLimit
		}
		if count*int64(width) <= 4 {
			return entry[8 : 8+count*int64(width)], width, nil
		}
		offset := int64(order.Uint32(entry[8:12]))
		if offset < 8 {
			return nil, 0, errTIFF
		}
		data, err := read(offset, count*int64(width))
		return data, width, err
	}
	positions, positionWidth, err := vector(offsets)
	if err != nil {
		return err
	}
	counts, countWidth, err := vector(lengths)
	if err != nil {
		return err
	}
	if len(positions)/positionWidth != len(counts)/countWidth {
		return errTIFF
	}
	number := func(data []byte, width int) int64 {
		if width == 2 {
			return int64(order.Uint16(data[:2]))
		}
		return int64(order.Uint32(data[:4]))
	}
	for len(positions) > 0 {
		offset, length := number(positions, positionWidth), number(counts, countWidth)
		if offset < 8 || length < 1 || offset > size || length > size-offset {
			return errTIFF
		}
		positions, counts = positions[positionWidth:], counts[countWidth:]
	}
	return nil
}

func tiffPrimary(entries []byte, order binary.ByteOrder, policy InspectPolicy) (tiffInfo, error) {
	var info tiffInfo
	seen := make(map[uint16]bool)
	for len(entries) > 0 {
		entry := entries[:12]
		entries = entries[12:]
		tag := order.Uint16(entry[:2])
		if seen[tag] {
			return tiffInfo{}, errTIFF
		}
		seen[tag] = true
		switch tag {
		case 0x014a, 0xc612: // SubIFDs and DNG require a distinct primary/raw profile.
			return tiffInfo{}, errTIFFUnsupported
		case 0x00fe, 0x00ff, 0x0100, 0x0101, 0x0103, 0x0106:
			kind := order.Uint16(entry[2:4])
			if (kind != 3 && kind != 4) || order.Uint32(entry[4:8]) != 1 {
				return tiffInfo{}, errTIFF
			}
			value := int64(order.Uint32(entry[8:12]))
			if kind == 3 {
				value = int64(order.Uint16(entry[8:10]))
			}
			switch tag {
			case 0x00fe:
				if value & ^int64(2) != 0 { // Page bit is allowed; reduced/mask/unknown is not.
					return tiffInfo{}, errTIFFUnsupported
				}
			case 0x00ff:
				if value != 1 && value != 3 {
					return tiffInfo{}, errTIFFUnsupported
				}
			case 0x0100:
				info.width = value
			case 0x0101:
				info.height = value
			case 0x0103:
				switch value {
				case 1, 2, 3, 4, 5, 6, 7, 8, 32773, 32946:
				default:
					return tiffInfo{}, errTIFFUnsupported
				}
			case 0x0106:
				switch value {
				case 0, 1, 2, 3, 5, 6, 8, 9, 10, 32844, 32845:
				default:
					return tiffInfo{}, errTIFFUnsupported
				}
			}
		}
	}
	if info.width < 1 || info.height < 1 {
		return tiffInfo{}, errTIFF
	}
	if info.width > int64(policy.MaxDimension) || info.height > int64(policy.MaxDimension) || info.width > policy.MaxPixels/info.height {
		return tiffInfo{}, errMetadataLimit
	}
	return info, nil
}
