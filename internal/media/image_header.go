package media

import (
	"context"
	"os"
	"strconv"
	"strings"
)

type imageGeometry struct {
	width, height int64
	loader        string
}

func inspectImageHeader(ctx context.Context, input *os.File, policy InspectPolicy) (imageGeometry, error) {
	data, err := probeCodecProfile(ctx, input, policy, true)
	if err != nil {
		return imageGeometry{}, err
	}
	return parseImageHeader(data, policy)
}

// Only the default fixed-FD summary is accepted. Never parse all-fields/history
// output, arbitrary filenames or newline-bearing metadata as geometry evidence.
func parseImageHeader(data []byte, policy InspectPolicy) (imageGeometry, error) {
	if len(data) > 1024 {
		return imageGeometry{}, errMetadataLimit
	}
	text, ok := strings.CutSuffix(string(data), "\n")
	if !ok || strings.ContainsAny(text, "\r\n") {
		return imageGeometry{}, errVideoMetadata
	}
	text, ok = strings.CutPrefix(text, "/proc/self/fd/3: ")
	if !ok {
		return imageGeometry{}, errVideoMetadata
	}
	size, tail, ok := strings.Cut(text, " ")
	if !ok {
		return imageGeometry{}, errVideoMetadata
	}
	width, height, ok := strings.Cut(size, "x")
	if !ok {
		return imageGeometry{}, errVideoMetadata
	}
	dimension := func(value string) (int64, error) {
		if len(value) < 1 || len(value) > 10 || value[0] == '0' {
			return 0, errVideoMetadata
		}
		for _, digit := range value {
			if digit < '0' || digit > '9' {
				return 0, errVideoMetadata
			}
		}
		return strconv.ParseInt(value, 10, 64)
	}
	w, err := dimension(width)
	if err != nil {
		return imageGeometry{}, errVideoMetadata
	}
	h, err := dimension(height)
	if err != nil {
		return imageGeometry{}, errVideoMetadata
	}
	if w > int64(policy.MaxDimension) || h > int64(policy.MaxDimension) || w > policy.MaxPixels/h {
		return imageGeometry{}, errMetadataLimit
	}
	fields := strings.Split(tail, ", ")
	if len(fields) != 4 {
		return imageGeometry{}, errVideoMetadata
	}
	switch fields[0] {
	case "uchar", "char", "ushort", "short", "uint", "int", "float", "double", "complex", "dpcomplex":
	default:
		return imageGeometry{}, errVideoMetadata
	}
	bands := strings.TrimSuffix(strings.TrimSuffix(fields[1], " bands"), " band")
	count, err := dimension(bands)
	if err != nil || count > 16 || (count == 1 && fields[1] != bands+" band") || (count != 1 && fields[1] != bands+" bands") || !ValidCodec(strings.ReplaceAll(fields[2], "-", "_")) || !ValidCodec(fields[3]) {
		return imageGeometry{}, errVideoMetadata
	}
	return imageGeometry{w, h, fields[3]}, nil
}
