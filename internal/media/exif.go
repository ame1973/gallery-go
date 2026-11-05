package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

var (
	errEXIF          = errors.New("invalid bounded EXIF metadata")
	errMetadataLimit = errors.New("metadata header resource limit exceeded")
)

// jpegEXIF reads only the bounded pre-scan JPEG marker stream. It never decodes
// pixels, follows metadata URLs/paths, or traverses GPS/MakerNote/thumbnail IFDs.
func jpegEXIF(ctx context.Context, file *os.File, budget int64) (*int64, *time.Time, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	limited := &io.LimitedReader{R: inspectReader{ctx, file}, N: budget}
	reader := bufio.NewReaderSize(limited, 32<<10)
	read := func(data []byte) error {
		_, err := io.ReadFull(reader, data)
		if err != nil && limited.N == 0 {
			return errMetadataLimit
		}
		return err
	}
	var pair [2]byte
	if err := read(pair[:]); err != nil {
		return nil, nil, err
	}
	if pair != [2]byte{0xff, 0xd8} {
		return nil, nil, errEXIF
	}
	var orientation *int64
	var capture *time.Time
	found := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := read(pair[:1]); err != nil {
			return nil, nil, err
		}
		if pair[0] != 0xff {
			return nil, nil, errEXIF
		}
		for {
			if err := read(pair[1:]); err != nil {
				return nil, nil, err
			}
			if pair[1] != 0xff {
				break
			}
		}
		marker := pair[1]
		if marker == 0xda || marker == 0xd9 {
			return orientation, capture, nil
		}
		if marker == 0x01 {
			continue
		}
		if marker == 0x00 || marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) {
			return nil, nil, errEXIF
		}
		if err := read(pair[:]); err != nil {
			return nil, nil, err
		}
		length := int(binary.BigEndian.Uint16(pair[:])) - 2
		if length < 0 {
			return nil, nil, errEXIF
		}
		if marker == 0xe1 {
			data := make([]byte, length)
			if err := read(data); err != nil {
				return nil, nil, err
			}
			if len(data) >= 6 && string(data[:6]) == "Exif\x00\x00" {
				if found {
					return nil, nil, errEXIF
				}
				found = true
				var err error
				orientation, capture, err = parseEXIF(data[6:])
				if err != nil {
					return nil, nil, err
				}
			}
		} else if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
			if limited.N == 0 {
				return nil, nil, errMetadataLimit
			}
			return nil, nil, err
		}
	}
}

// parseEXIF reads only the classic-TIFF whitelist from a caller-bounded profile.
// The same reader serves scattered TIFF fields without copying source pixels.
func parseEXIF(data []byte) (*int64, *time.Time, error) {
	return readEXIF(func(offset, length int64) ([]byte, error) {
		if offset < 0 || length < 0 || offset > int64(len(data)) || length > int64(len(data))-offset {
			return nil, errEXIF
		}
		return data[offset : offset+length], nil
	})
}

func readTIFFDirectory(read func(int64, int64) ([]byte, error), order binary.ByteOrder, offset uint32) ([]byte, uint32, error) {
	if offset < 8 {
		return nil, 0, errEXIF
	}
	countBytes, err := read(int64(offset), 2)
	if err != nil {
		return nil, 0, err
	}
	count := int64(order.Uint16(countBytes))
	if count > 512 {
		return nil, 0, errMetadataLimit
	}
	data, err := read(int64(offset)+2, count*12+4)
	if err != nil {
		return nil, 0, err
	}
	return data[:count*12], order.Uint32(data[count*12:]), nil
}

// Fractions are normalized to DB milliseconds; an unzoned clock is never host
// time or mtime. GPS, MakerNote, thumbnails and next-page EXIF are not followed.
func readEXIF(read func(int64, int64) ([]byte, error)) (*int64, *time.Time, error) {
	header, err := read(0, 8)
	if err != nil {
		return nil, nil, err
	}
	var order binary.ByteOrder
	switch string(header[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, nil, errEXIF
	}
	if order.Uint16(header[2:4]) != 42 {
		return nil, nil, errEXIF
	}
	first := order.Uint32(header[4:8])
	entries := func(offset uint32) ([]byte, error) {
		data, _, err := readTIFFDirectory(read, order, offset)
		return data, err
	}
	base, err := entries(first)
	if err != nil {
		return nil, nil, err
	}
	var orientation *int64
	var exif uint32
	seenOrientation, seenEXIF := false, false
	for len(base) > 0 {
		entry := base[:12]
		base = base[12:]
		switch order.Uint16(entry[:2]) {
		case 0x0112:
			if seenOrientation || order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
				return nil, nil, errEXIF
			}
			value := int64(order.Uint16(entry[8:10]))
			if value < 1 || value > 8 {
				return nil, nil, errEXIF
			}
			orientation = &value
			seenOrientation = true
		case 0x8769:
			if seenEXIF || order.Uint16(entry[2:4]) != 4 || order.Uint32(entry[4:8]) != 1 {
				return nil, nil, errEXIF
			}
			exif = order.Uint32(entry[8:12])
			seenEXIF = true
		}
	}
	if !seenEXIF {
		return orientation, nil, nil
	}
	if exif == first {
		return nil, nil, errEXIF
	}
	fields, err := entries(exif)
	if err != nil {
		return nil, nil, err
	}
	text := func(entry []byte, length uint32) (string, error) {
		if length < 1 || order.Uint16(entry[2:4]) != 2 || order.Uint32(entry[4:8]) != length {
			return "", errEXIF
		}
		var value []byte
		if length <= 4 {
			value = entry[8 : 8+length]
		} else {
			offset := int64(order.Uint32(entry[8:12]))
			if offset < 8 {
				return "", errEXIF
			}
			var err error
			value, err = read(offset, int64(length))
			if err != nil {
				return "", err
			}
		}
		if value[len(value)-1] != 0 || strings.ContainsRune(string(value[:len(value)-1]), 0) {
			return "", errEXIF
		}
		return string(value[:len(value)-1]), nil
	}
	date, zone, fraction := "", "", ""
	seenDate, seenZone, seenFraction := false, false, false
	for len(fields) > 0 {
		entry := fields[:12]
		fields = fields[12:]
		switch order.Uint16(entry[:2]) {
		case 0x9003:
			if seenDate {
				return nil, nil, errEXIF
			}
			seenDate = true
			date, err = text(entry, 20)
		case 0x9011:
			if seenZone {
				return nil, nil, errEXIF
			}
			seenZone = true
			zone, err = text(entry, 7)
		case 0x9291:
			if seenFraction {
				return nil, nil, errEXIF
			}
			count := order.Uint32(entry[4:8])
			if count > 64 {
				return nil, nil, errMetadataLimit
			}
			seenFraction = true
			fraction, err = text(entry, count)
			fraction = strings.TrimRight(fraction, " ")
			for _, digit := range fraction {
				if digit < '0' || digit > '9' {
					return nil, nil, errEXIF
				}
			}
		}
		if err != nil {
			return nil, nil, err
		}
	}
	if !seenDate || !seenZone || zone == "-00:00" {
		return orientation, nil, nil
	}
	if len(zone) != 6 || (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' {
		return nil, nil, errEXIF
	}
	for _, index := range []int{1, 2, 4, 5} {
		if zone[index] < '0' || zone[index] > '9' {
			return nil, nil, errEXIF
		}
	}
	hours, minutes := int(zone[1]-'0')*10+int(zone[2]-'0'), int(zone[4]-'0')*10+int(zone[5]-'0')
	if hours > 14 || minutes > 59 || (hours == 14 && minutes != 0) {
		return nil, nil, errEXIF
	}
	captured, err := time.Parse("2006:01:02 15:04:05-07:00", date+zone)
	if err != nil || captured.Year() < 1 {
		return nil, nil, errEXIF
	}
	var milliseconds int
	for index := range 3 {
		milliseconds *= 10
		if index < len(fraction) {
			milliseconds += int(fraction[index] - '0')
		}
	}
	utc := captured.UTC().Add(time.Duration(milliseconds) * time.Millisecond)
	if utc.Year() < 1 || utc.Year() > 9999 {
		return nil, nil, errEXIF
	}
	return orientation, &utc, nil
}
