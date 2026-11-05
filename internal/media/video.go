package media

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"gallerygo/internal/sandbox"
	"gallerygo/internal/strictjson"
)

var (
	errVideoMetadata    = errors.New("invalid bounded video metadata")
	errVideoUnsupported = errors.New("video metadata capability is unavailable")
)

type videoTags struct {
	Created       *string `json:"creation_time"`
	CreatedUpper  *string `json:"CREATION_TIME"`
	Original      *string `json:"com.apple.quicktime.creationdate"`
	OriginalUpper *string `json:"COM.APPLE.QUICKTIME.CREATIONDATE"`
	Rotate        *string `json:"rotate"`
	RotateUpper   *string `json:"ROTATE"`
}

type videoSideData struct {
	Matrix   *string `json:"displaymatrix"`
	Rotation *int64  `json:"rotation"`
}

type videoProjection struct {
	Programs     []struct{} `json:"programs"`
	StreamGroups []struct{} `json:"stream_groups"`
	Streams      []struct {
		Index       *int64  `json:"index"`
		Codec       string  `json:"codec_name"`
		Kind        string  `json:"codec_type"`
		Width       *int64  `json:"width"`
		Height      *int64  `json:"height"`
		Duration    *string `json:"duration"`
		Disposition struct {
			Attached *int64 `json:"attached_pic"`
		} `json:"disposition"`
		Tags     videoTags       `json:"tags"`
		SideData []videoSideData `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Name     string    `json:"format_name"`
		Duration *string   `json:"duration"`
		Tags     videoTags `json:"tags"`
	} `json:"format"`
}

type videoInfo struct {
	width, height         int64
	codec                 string
	duration, orientation *int64
	capture               *time.Time
}

// PinCodecRuntime freezes capability/installation identity before a writer opens.
// Missing tools remain explicit failed media work, not unrestricted execution or a
// reason to prevent unrelated native image inventory on a codec-free deployment.
func (policy InspectPolicy) PinCodecRuntime(ctx context.Context) (InspectPolicy, error) {
	if err := policy.Validate(); err != nil {
		return InspectPolicy{}, err
	}
	pin, err := sandbox.Signature(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return InspectPolicy{}, ctx.Err()
		}
		pin = "unavailable"
	}
	policy.codecRuntime = pin
	return policy, nil
}

// CodecRuntimePin is private processing evidence, never a public configuration,
// filesystem path, capability claim or metadata API field.
func (policy InspectPolicy) CodecRuntimePin() string { return policy.codecRuntime }

func inspectVideo(ctx context.Context, input *os.File, container string, policy InspectPolicy) (videoInfo, error) {
	if policy.codecRuntime == "" {
		var err error
		policy, err = policy.PinCodecRuntime(ctx)
		if err != nil {
			return videoInfo{}, err
		}
	}
	if policy.codecRuntime == "unavailable" {
		return videoInfo{}, errVideoUnsupported
	}
	limits := sandbox.DefaultLimits()
	limits.Timeout = policy.Timeout
	output, err := sandbox.Probe(ctx, input, limits, policy.codecRuntime)
	switch {
	case errors.Is(err, sandbox.ErrRuntimeChanged):
		return videoInfo{}, ErrContentChanged
	case errors.Is(err, sandbox.ErrUnavailable):
		return videoInfo{}, errVideoUnsupported
	case errors.Is(err, sandbox.ErrResource), errors.Is(err, sandbox.ErrInput):
		return videoInfo{}, errMetadataLimit
	case errors.Is(err, sandbox.ErrProbe):
		return videoInfo{}, errVideoMetadata
	case err != nil:
		return videoInfo{}, err
	}
	return parseVideoProjection(output, container, policy)
}

func parseVideoProjection(data []byte, container string, policy InspectPolicy) (videoInfo, error) {
	if len(data) > 1<<20 {
		return videoInfo{}, errMetadataLimit
	}
	var projection videoProjection
	if strictjson.Decode(data, &projection) != nil || len(projection.Programs) != 0 || len(projection.StreamGroups) != 0 || len(projection.Streams) > 1 {
		return videoInfo{}, errVideoMetadata
	}
	family := "mov,mp4,m4a,3gp,3g2,mj2"
	switch container {
	case "mp4", "quicktime":
	case "matroska", "webm":
		family = "matroska,webm"
	default:
		return videoInfo{}, errVideoUnsupported
	}
	if projection.Format.Name != family {
		return videoInfo{}, errVideoMetadata
	}
	if len(projection.Streams) == 0 {
		return videoInfo{}, errVideoUnsupported
	}
	stream := projection.Streams[0]
	if stream.Index == nil || *stream.Index < 0 || *stream.Index >= 64 || stream.Kind != "video" || stream.Disposition.Attached == nil || *stream.Disposition.Attached != 0 {
		return videoInfo{}, errVideoMetadata
	}
	if stream.Codec == "" || stream.Codec == "unknown" {
		return videoInfo{}, errVideoUnsupported
	}
	if !ValidCodec(stream.Codec) || stream.Width == nil || stream.Height == nil || *stream.Width < 1 || *stream.Height < 1 {
		return videoInfo{}, errVideoMetadata
	}
	if *stream.Width > int64(policy.MaxDimension) || *stream.Height > int64(policy.MaxDimension) || *stream.Width > policy.MaxPixels / *stream.Height {
		return videoInfo{}, errMetadataLimit
	}
	duration, err := videoDuration(stream.Duration, policy.MaxDuration)
	if err != nil {
		return videoInfo{}, err
	}
	formatDuration, err := videoDuration(projection.Format.Duration, policy.MaxDuration)
	if err != nil {
		return videoInfo{}, err
	}
	if duration == nil {
		duration = formatDuration
	}
	orientation, err := videoOrientation(stream.SideData, stream.Tags)
	if err != nil {
		return videoInfo{}, err
	}
	capture, err := videoCapture(stream.Tags, projection.Format.Tags)
	if err != nil {
		return videoInfo{}, err
	}
	return videoInfo{*stream.Width, *stream.Height, stream.Codec, duration, orientation, capture}, nil
}

// ValidCodec admits only bounded FFprobe/native codec identifiers, never a raw tag,
// profile, path, URL or arbitrary diagnostic. Unknown codecs are explicit failures.
func ValidCodec(value string) bool {
	if len(value) < 1 || len(value) > 64 || value == "unknown" {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

// Duration is a declared selected-track value, with container fallback only when
// absent/zero/N/A. Never derive it from frame rate/count; round exact seconds once.
func videoDuration(value *string, maximum time.Duration) (*int64, error) {
	if value == nil || *value == "N/A" || *value == "" {
		return nil, nil
	}
	text := *value
	if len(text) > 32 {
		return nil, errMetadataLimit
	}
	dot := false
	for index, digit := range text {
		if digit == '.' && !dot && index > 0 && index < len(text)-1 {
			dot = true
			continue
		}
		if digit < '0' || digit > '9' {
			return nil, errVideoMetadata
		}
	}
	seconds, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, errVideoMetadata
	}
	if seconds.Sign() == 0 {
		return nil, nil
	}
	if seconds.Cmp(new(big.Rat).SetFrac64(int64(maximum), int64(time.Second))) > 0 {
		return nil, errMetadataLimit
	}
	milliseconds := new(big.Rat).Mul(seconds, big.NewRat(1000, 1))
	valueMS := ceilRationalMS(milliseconds)
	return &valueMS, nil
}

func ceilRationalMS(value *big.Rat) int64 {
	var quotient, remainder big.Int
	quotient.QuoRem(value.Num(), value.Denom(), &remainder)
	if remainder.Sign() != 0 {
		quotient.Add(&quotient, big.NewInt(1))
	}
	return quotient.Int64()
}

func tagValue(lower, upper *string) (*string, error) {
	if lower != nil && upper != nil {
		return nil, errVideoMetadata
	}
	if lower != nil {
		return lower, nil
	}
	return upper, nil
}

func videoCapture(stream, format videoTags) (*time.Time, error) {
	original, err := tagValue(stream.Original, stream.OriginalUpper)
	if err != nil {
		return nil, err
	}
	formatOriginal, err := tagValue(format.Original, format.OriginalUpper)
	if err != nil {
		return nil, err
	}
	values := []*string{original, formatOriginal}
	if original == nil && formatOriginal == nil {
		created, err := tagValue(stream.Created, stream.CreatedUpper)
		if err != nil {
			return nil, err
		}
		formatCreated, err := tagValue(format.Created, format.CreatedUpper)
		if err != nil {
			return nil, err
		}
		values = []*string{created, formatCreated}
	}
	var capture *time.Time
	for _, text := range values {
		if text == nil {
			continue
		}
		if len(*text) > 64 {
			return nil, errMetadataLimit
		}
		if len(*text) < 20 || (*text)[4] != '-' || (*text)[7] != '-' || (*text)[10] != 'T' || (*text)[13] != ':' || (*text)[16] != ':' || strings.Contains(*text, ",") {
			return nil, nil
		}
		if strings.HasSuffix(*text, "-00:00") {
			return nil, nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, *text)
		if err != nil {
			return nil, nil
		}
		// Go accepts out-of-range RFC3339 zone components; do not normalize them.
		if !strings.HasSuffix(*text, "Z") {
			if len(*text) < 6 {
				return nil, nil
			}
			zone := (*text)[len(*text)-6:]
			hour, hourErr := strconv.Atoi(zone[1:3])
			minute, minuteErr := strconv.Atoi(zone[4:6])
			if (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' || hourErr != nil || minuteErr != nil || hour > 23 || minute > 59 {
				return nil, nil
			}
		}
		utc := parsed.UTC().Truncate(time.Millisecond)
		if utc.Year() < 1 || utc.Year() > 9999 {
			return nil, nil
		}
		if capture != nil && !capture.Equal(utc) {
			return nil, nil
		}
		capture = &utc
	}
	return capture, nil
}

func videoOrientation(side []videoSideData, tags videoTags) (*int64, error) {
	if len(side) > 16 {
		return nil, errMetadataLimit
	}
	var matrix *string
	for _, data := range side {
		if data.Matrix != nil {
			if matrix != nil {
				return nil, errVideoMetadata
			}
			matrix = data.Matrix
		}
	}
	if matrix != nil {
		if len(*matrix) > 512 {
			return nil, errMetadataLimit
		}
		fields := strings.Fields(*matrix)
		if len(fields) != 12 {
			return nil, errVideoMetadata
		}
		var entries [9]int64
		for row := range 3 {
			if fields[row*4] != "0000000"+strconv.Itoa(row)+":" {
				return nil, errVideoMetadata
			}
			for column := range 3 {
				value, err := strconv.ParseInt(fields[row*4+column+1], 10, 32)
				if err != nil {
					return nil, errVideoMetadata
				}
				entries[row*3+column] = value
			}
		}
		// Arbitrary scaling/shear/perspective/translation is not an EXIF transform.
		if entries[2] != 0 || entries[5] != 0 || entries[6] != 0 || entries[7] != 0 || entries[8] != 1<<30 {
			return nil, nil
		}
		transforms := map[[4]int64]int64{
			{65536, 0, 0, 65536}: 1, {-65536, 0, 0, 65536}: 2,
			{-65536, 0, 0, -65536}: 3, {65536, 0, 0, -65536}: 4,
			{0, 65536, 65536, 0}: 5, {0, 65536, -65536, 0}: 6,
			{0, -65536, -65536, 0}: 7, {0, -65536, 65536, 0}: 8,
		}
		orientation, ok := transforms[[4]int64{entries[0], entries[1], entries[3], entries[4]}]
		if !ok {
			return nil, nil
		}
		return &orientation, nil
	}
	rotate, err := tagValue(tags.Rotate, tags.RotateUpper)
	if err != nil {
		return nil, err
	}
	if rotate == nil {
		return nil, nil
	}
	if len(*rotate) > 32 {
		return nil, errMetadataLimit
	}
	angle, err := strconv.ParseInt(*rotate, 10, 64)
	if err != nil || angle%90 != 0 {
		return nil, nil
	}
	angle = ((angle % 360) + 360) % 360
	orientation := map[int64]int64{0: 1, 90: 6, 180: 3, 270: 8}[angle]
	return &orientation, nil
}
