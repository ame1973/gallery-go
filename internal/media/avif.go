package media

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"time"
)

var (
	errAVIF            = errors.New("invalid bounded AVIF metadata")
	errAVIFUnsupported = errors.New("AVIF metadata profile is unavailable")
)

type avifBox struct {
	kind   string
	body   []byte
	offset int64
}
type avifExtent struct{ offset, length int64 }
type avifItem struct {
	kind        string
	located     bool
	hidden      bool
	method      uint64
	extents     []avifExtent
	properties  []uint32
	essential   []bool
	metadataFor []uint64
}
type avifInfo struct {
	width, height int64
	animationInfo
	loopKnown   bool
	orientation *int64
	capture     *time.Time
}

type avifReader struct {
	ctx             context.Context
	file            io.ReaderAt
	size, remaining int64
	boxes           int
}

func (r *avifReader) read(offset, length int64) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > r.size || length > r.size-offset {
		return nil, errAVIF
	}
	if length > r.remaining {
		return nil, errMetadataLimit
	}
	r.remaining -= length
	data := make([]byte, int(length))
	if _, err := r.file.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return data, r.ctx.Err()
}

func (r *avifReader) children(data []byte, offset int64) ([]avifBox, error) {
	result := make([]avifBox, 0)
	for position := 0; position < len(data); {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		if r.boxes >= 4096 {
			return nil, errMetadataLimit
		}
		r.boxes++
		if len(data)-position < 8 {
			return nil, errAVIF
		}
		size := uint64(binary.BigEndian.Uint32(data[position:]))
		header := 8
		if size == 1 {
			if len(data)-position < 16 {
				return nil, errAVIF
			}
			size, header = binary.BigEndian.Uint64(data[position+8:]), 16
		}
		if size == 0 {
			size = uint64(len(data) - position)
		}
		if size < uint64(header) || size > uint64(len(data)-position) {
			return nil, errAVIF
		}
		end := position + int(size)
		result = append(result, avifBox{string(data[position+4 : position+8]), data[position+header : end], offset + int64(position+header)})
		position = end
	}
	return result, nil
}

// AVIF metadata is sought through bounded local boxes, never through arbitrary
// item URLs or external data references. Compressed mdat payloads are not copied.
func parseAVIF(ctx context.Context, file io.ReaderAt, size int64, policy InspectPolicy) (avifInfo, error) {
	r := &avifReader{ctx: ctx, file: file, size: size, remaining: policy.HeaderBytes}
	var meta, movie *avifBox
	var mediaData []avifExtent
	var brands []string
	for offset := int64(0); offset < size; {
		if r.boxes >= 4096 {
			return avifInfo{}, errMetadataLimit
		}
		r.boxes++
		header, err := r.read(offset, 8)
		if err != nil {
			return avifInfo{}, err
		}
		length := uint64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if length == 1 {
			extended, err := r.read(offset+8, 8)
			if err != nil {
				return avifInfo{}, err
			}
			length, headerSize = binary.BigEndian.Uint64(extended), 16
		}
		if length == 0 {
			length = uint64(size - offset)
		}
		if length < uint64(headerSize) || length > uint64(size-offset) {
			return avifInfo{}, errAVIF
		}
		kind := string(header[4:8])
		bodyOffset, bodySize := offset+headerSize, int64(length)-headerSize
		switch kind {
		case "ftyp", "meta", "moov":
			data, err := r.read(bodyOffset, bodySize)
			if err != nil {
				return avifInfo{}, err
			}
			box := avifBox{kind, data, bodyOffset}
			switch kind {
			case "ftyp":
				if len(brands) != 0 || len(data) < 8 || len(data)%4 != 0 {
					return avifInfo{}, errAVIF
				}
				brands = append(brands, string(data[:4]))
				for position := 8; position < len(data); position += 4 {
					brands = append(brands, string(data[position:position+4]))
				}
			case "meta":
				if meta != nil {
					return avifInfo{}, errAVIF
				}
				meta = &box
			case "moov":
				if movie != nil {
					return avifInfo{}, errAVIF
				}
				movie = &box
			}
		case "mdat":
			mediaData = append(mediaData, avifExtent{bodyOffset, bodySize})
		}
		offset += int64(length)
	}
	avifBrand := false
	for _, brand := range brands {
		avifBrand = avifBrand || brand == "avif" || brand == "avis"
	}
	if !avifBrand || meta == nil {
		return avifInfo{}, errAVIF
	}
	// The fixed installed MOV demuxer recognizes AVIF image items only under
	// explicit AVIF major brands. Keep generic HEIF compatibility truthful.
	if brands[0] != "avif" && brands[0] != "avis" {
		return avifInfo{}, errAVIFUnsupported
	}
	info, err := r.meta(*meta, mediaData, policy)
	if err != nil {
		return avifInfo{}, err
	}
	if movie == nil {
		for _, brand := range brands {
			if brand == "avis" {
				return avifInfo{}, errAVIF
			}
		}
		info.frames, info.loops, info.loopKnown = 1, 1, true
	} else {
		animation, known, err := r.sequence(*movie, mediaData, policy)
		if err != nil {
			return avifInfo{}, err
		}
		info.animationInfo, info.loopKnown = animation, known
	}
	return info, nil
}

func inspectAVIF(ctx context.Context, file *os.File, size int64, policy InspectPolicy) (avifInfo, error) {
	info, err := parseAVIF(ctx, file, size, policy)
	if err != nil {
		return avifInfo{}, err
	}
	output, err := probeCodec(ctx, file, policy)
	if err != nil {
		return avifInfo{}, err
	}
	projection, err := decodeCodecProjection(output, "mov,mp4,m4a,3gp,3g2,mj2", policy)
	if err != nil {
		return avifInfo{}, err
	}
	stream := projection.Streams[0]
	if stream.Codec != "av1" {
		return avifInfo{}, errAVIFUnsupported
	}
	if *stream.Width != info.width || *stream.Height != info.height {
		return avifInfo{}, errAVIF
	}
	return info, nil
}

// A small latching cursor keeps every versioned count/offset check in one place.
// It never allocates from an untrusted length or follows an unbounded pointer.
type avifCursor struct {
	data []byte
	err  error
}

func (c *avifCursor) take(length int) []byte {
	if c.err != nil {
		return nil
	}
	if length < 0 || length > len(c.data) {
		c.err = errAVIF
		return nil
	}
	value := c.data[:length]
	c.data = c.data[length:]
	return value
}
func (c *avifCursor) number(length int) uint64 {
	if length < 0 || length > 8 {
		c.err = errAVIF
		return 0
	}
	var value uint64
	for _, digit := range c.take(length) {
		value = value<<8 | uint64(digit)
	}
	return value
}
func (c *avifCursor) done() error {
	if c.err != nil {
		return c.err
	}
	if len(c.data) != 0 {
		return errAVIF
	}
	return nil
}

func avifFull(box avifBox) (*avifCursor, byte, uint32, error) {
	if len(box.body) < 4 {
		return nil, 0, 0, errAVIF
	}
	return &avifCursor{data: box.body[4:]}, box.body[0], binary.BigEndian.Uint32(box.body[:4]) & 0xffffff, nil
}

func (r *avifReader) meta(box avifBox, mediaData []avifExtent, policy InspectPolicy) (avifInfo, error) {
	cursor, version, flags, err := avifFull(box)
	if err != nil || version != 0 || flags != 0 {
		return avifInfo{}, errAVIF
	}
	children, err := r.children(cursor.data, box.offset+4)
	if err != nil {
		return avifInfo{}, err
	}
	items := make(map[uint64]*avifItem)
	item := func(id uint64) (*avifItem, error) {
		if id == 0 {
			return nil, errAVIF
		}
		if existing := items[id]; existing != nil {
			return existing, nil
		}
		if len(items) >= 256 {
			return nil, errMetadataLimit
		}
		created := &avifItem{}
		items[id] = created
		return created, nil
	}
	seen := make(map[string]bool)
	var primary uint64
	var properties []avifBox
	var idat *avifBox
	for _, child := range children {
		if child.kind != "free" && child.kind != "skip" {
			if seen[child.kind] {
				return avifInfo{}, errAVIF
			}
			seen[child.kind] = true
		}
		switch child.kind {
		case "hdlr":
			if len(child.body) < 12 || string(child.body[8:12]) != "pict" {
				return avifInfo{}, errAVIF
			}
		case "pitm":
			c, v, f, err := avifFull(child)
			if err != nil || v > 1 || f != 0 {
				return avifInfo{}, errAVIF
			}
			if v == 0 {
				primary = c.number(2)
			} else {
				primary = c.number(4)
			}
			if c.done() != nil || primary == 0 {
				return avifInfo{}, errAVIF
			}
		case "iinf":
			c, v, f, err := avifFull(child)
			if err != nil || v > 1 || f != 0 {
				return avifInfo{}, errAVIF
			}
			count := c.number(2)
			if v == 1 {
				c = &avifCursor{data: child.body[4:]}
				count = c.number(4)
			}
			if c.err != nil || count > 256 {
				return avifInfo{}, errMetadataLimit
			}
			entries, err := r.children(c.data, child.offset+int64(len(child.body)-len(c.data)))
			if err != nil || uint64(len(entries)) != count {
				return avifInfo{}, errAVIF
			}
			for _, entry := range entries {
				if entry.kind != "infe" {
					return avifInfo{}, errAVIF
				}
				e, v, f, err := avifFull(entry)
				if err != nil || f > 1 {
					return avifInfo{}, errAVIF
				}
				if v != 2 && v != 3 {
					return avifInfo{}, errAVIFUnsupported
				}
				id := e.number(2)
				if v == 3 {
					e = &avifCursor{data: entry.body[4:]}
					id = e.number(4)
				}
				protection := e.number(2)
				kind := string(e.take(4))
				if e.err != nil || len(e.data) < 1 || !containsAVIFNUL(e.data) {
					return avifInfo{}, errAVIF
				}
				if protection != 0 {
					return avifInfo{}, errAVIFUnsupported
				}
				value, err := item(id)
				if err != nil {
					return avifInfo{}, err
				}
				if value.kind != "" {
					return avifInfo{}, errAVIF
				}
				value.kind, value.hidden = kind, f&1 != 0
			}
		case "iloc":
			if err := parseAVIFLocations(child, item); err != nil {
				return avifInfo{}, err
			}
		case "iref":
			if err := r.references(child, item); err != nil {
				return avifInfo{}, err
			}
		case "iprp":
			properties, err = r.properties(child, item)
			if err != nil {
				return avifInfo{}, err
			}
		case "idat":
			copy := child
			idat = &copy
		}
	}
	if !seen["hdlr"] || !seen["iinf"] || !seen["iloc"] || !seen["iprp"] || primary == 0 {
		return avifInfo{}, errAVIF
	}
	main := items[primary]
	if main == nil || main.kind == "" || !main.located {
		return avifInfo{}, errAVIF
	}
	if main.kind != "av01" || main.hidden {
		return avifInfo{}, errAVIFUnsupported
	}
	for _, value := range items {
		if value.kind == "" || !value.located || len(value.extents) == 0 {
			return avifInfo{}, errAVIF
		}
		for index, extent := range value.extents {
			if value.method == 1 {
				if idat == nil || extent.offset > int64(len(idat.body)) || extent.length > int64(len(idat.body))-extent.offset {
					return avifInfo{}, errAVIF
				}
				value.extents[index].offset += idat.offset
			} else {
				valid := false
				for _, region := range mediaData {
					if extent.offset >= region.offset && extent.offset-region.offset <= region.length && extent.length <= region.length-(extent.offset-region.offset) {
						valid = true
						break
					}
				}
				if !valid {
					return avifInfo{}, errAVIF
				}
			}
		}
	}
	info, err := avifPrimaryProperties(main, properties, policy)
	if err != nil {
		return avifInfo{}, err
	}
	var exif *avifItem
	for _, candidate := range items {
		if candidate.kind != "Exif" {
			continue
		}
		for _, target := range candidate.metadataFor {
			if items[target] == nil {
				return avifInfo{}, errAVIF
			}
			if target == primary {
				if exif != nil {
					return avifInfo{}, errAVIF
				}
				exif = candidate
			}
		}
	}
	if exif != nil {
		var data []byte
		for _, extent := range exif.extents {
			if extent.length > 64<<10-int64(len(data)) {
				return avifInfo{}, errMetadataLimit
			}
			part, err := r.read(extent.offset, extent.length)
			if err != nil {
				return avifInfo{}, err
			}
			data = append(data, part...)
		}
		if len(data) < 12 {
			return avifInfo{}, errEXIF
		}
		offset := uint64(binary.BigEndian.Uint32(data[:4])) + 4
		if offset > uint64(len(data)-8) {
			return avifInfo{}, errEXIF
		}
		orientation, capture, err := parseEXIF(data[offset:])
		if err != nil {
			return avifInfo{}, err
		}
		if info.orientation == nil {
			info.orientation = orientation
		}
		info.capture = capture
	}
	return info, nil
}

func containsAVIFNUL(data []byte) bool {
	for _, value := range data {
		if value == 0 {
			return true
		}
	}
	return false
}

func parseAVIFLocations(box avifBox, item func(uint64) (*avifItem, error)) error {
	c, v, f, err := avifFull(box)
	if err != nil || v > 2 || f != 0 {
		return errAVIF
	}
	field := c.number(1)
	offsetSize, lengthSize := int(field>>4), int(field&15)
	field = c.number(1)
	baseSize, indexSize := int(field>>4), int(field&15)
	if offsetSize > 8 || lengthSize > 8 || baseSize > 8 || indexSize > 8 || v == 0 && indexSize != 0 {
		return errAVIF
	}
	countSize := 2
	if v == 2 {
		countSize = 4
	}
	count := c.number(countSize)
	if count > 256 {
		return errMetadataLimit
	}
	for range count {
		id := c.number(countSize)
		value, err := item(id)
		if err != nil {
			return err
		}
		if value.located {
			return errAVIF
		}
		value.located = true
		if v > 0 {
			method := c.number(2)
			if method > 1 {
				return errAVIFUnsupported
			}
			value.method = method
		}
		if c.number(2) != 0 {
			return errAVIFUnsupported
		}
		base := c.number(baseSize)
		extentCount := c.number(2)
		if extentCount > 16 {
			return errMetadataLimit
		}
		for range extentCount {
			if v > 0 && indexSize > 0 {
				_ = c.number(indexSize)
			}
			offset, length := c.number(offsetSize), c.number(lengthSize)
			if c.err != nil || base > math.MaxInt64 || offset > math.MaxInt64-base || length == 0 || length > math.MaxInt64 {
				return errAVIF
			}
			value.extents = append(value.extents, avifExtent{int64(base + offset), int64(length)})
		}
	}
	return c.done()
}

func (r *avifReader) references(box avifBox, item func(uint64) (*avifItem, error)) error {
	c, v, f, err := avifFull(box)
	if err != nil || v > 1 || f != 0 {
		return errAVIF
	}
	children, err := r.children(c.data, box.offset+4)
	if err != nil {
		return err
	}
	for _, child := range children {
		width := 2
		if v == 1 {
			width = 4
		}
		entry := &avifCursor{data: child.body}
		from := entry.number(width)
		count := entry.number(2)
		if count > 256 {
			return errMetadataLimit
		}
		value, err := item(from)
		if err != nil {
			return err
		}
		for range count {
			id := entry.number(width)
			if child.kind == "cdsc" {
				value.metadataFor = append(value.metadataFor, id)
			}
		}
		if err := entry.done(); err != nil {
			return err
		}
	}
	return nil
}

func (r *avifReader) properties(box avifBox, item func(uint64) (*avifItem, error)) ([]avifBox, error) {
	children, err := r.children(box.body, box.offset)
	if err != nil {
		return nil, err
	}
	var properties []avifBox
	for _, child := range children {
		if child.kind == "ipco" {
			if properties != nil {
				return nil, errAVIF
			}
			properties, err = r.children(child.body, child.offset)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, child := range children {
		if child.kind != "ipma" {
			continue
		}
		c, v, f, err := avifFull(child)
		if err != nil || v > 1 || f > 1 {
			return nil, errAVIF
		}
		count := c.number(4)
		if count > 256 {
			return nil, errMetadataLimit
		}
		for range count {
			width := 2
			if v == 1 {
				width = 4
			}
			value, err := item(c.number(width))
			if err != nil {
				return nil, err
			}
			if value.properties != nil {
				return nil, errAVIF
			}
			value.properties = []uint32{}
			associations := c.number(1)
			if associations > 64 {
				return nil, errMetadataLimit
			}
			for range associations {
				width := 1
				if f == 1 {
					width = 2
				}
				association := c.number(width)
				mask := uint64(1) << (width*8 - 1)
				index := association & (mask - 1)
				if index == 0 {
					continue
				}
				if index > uint64(len(properties)) {
					return nil, errAVIF
				}
				value.properties = append(value.properties, uint32(index))
				value.essential = append(value.essential, association&mask != 0)
			}
		}
		if c.done() != nil {
			return nil, errAVIF
		}
	}
	if properties == nil {
		return nil, errAVIF
	}
	return properties, nil
}

func avifPrimaryProperties(item *avifItem, properties []avifBox, policy InspectPolicy) (avifInfo, error) {
	var result avifInfo
	seen := make(map[string]bool)
	// EXIF's eight transforms form a closed group; apply properties in association
	// order, as required by AVIF, rather than always mirroring before rotation.
	transform := [4]int64{1, 0, 0, 1}
	transformed := false
	for offset, index := range item.properties {
		property := properties[index-1]
		if seen[property.kind] {
			return avifInfo{}, errAVIF
		}
		seen[property.kind] = true
		var next [4]int64
		switch property.kind {
		case "ispe":
			if len(property.body) != 12 || binary.BigEndian.Uint32(property.body[:4]) != 0 {
				return avifInfo{}, errAVIF
			}
			result.width, result.height = int64(binary.BigEndian.Uint32(property.body[4:8])), int64(binary.BigEndian.Uint32(property.body[8:12]))
		case "av1C":
			if len(property.body) < 4 || property.body[0] != 0x81 {
				return avifInfo{}, errAVIF
			}
		case "irot":
			if len(property.body) != 1 || property.body[0] > 3 {
				return avifInfo{}, errAVIF
			}
			next = [][4]int64{{1, 0, 0, 1}, {0, -1, 1, 0}, {-1, 0, 0, -1}, {0, 1, -1, 0}}[property.body[0]]
		case "imir":
			if len(property.body) != 1 || property.body[0] > 1 {
				return avifInfo{}, errAVIF
			}
			next = [][4]int64{{1, 0, 0, -1}, {-1, 0, 0, 1}}[property.body[0]]
		case "pixi", "colr", "clap", "pasp", "clli", "mdcv":
		default:
			if item.essential[offset] {
				return avifInfo{}, errAVIFUnsupported
			}
		}
		if next != [4]int64{} {
			transform = [4]int64{next[0]*transform[0] + next[2]*transform[1], next[1]*transform[0] + next[3]*transform[1], next[0]*transform[2] + next[2]*transform[3], next[1]*transform[2] + next[3]*transform[3]}
			transformed = true
		}
	}
	if !seen["ispe"] || !seen["av1C"] || result.width < 1 || result.height < 1 {
		return avifInfo{}, errAVIF
	}
	if result.width > int64(policy.MaxDimension) || result.height > int64(policy.MaxDimension) || result.width > policy.MaxPixels/result.height {
		return avifInfo{}, errMetadataLimit
	}
	if transformed {
		orientations := map[[4]int64]int64{{1, 0, 0, 1}: 1, {-1, 0, 0, 1}: 2, {-1, 0, 0, -1}: 3, {1, 0, 0, -1}: 4, {0, 1, 1, 0}: 5, {0, 1, -1, 0}: 6, {0, -1, -1, 0}: 7, {0, -1, 1, 0}: 8}
		value := orientations[transform]
		result.orientation = &value
	}
	return result, nil
}
