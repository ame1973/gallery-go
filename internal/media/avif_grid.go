package media

import (
	"bytes"
	"encoding/binary"
)

// Grid geometry is declared by a tiny local descriptor. Its inputs must be
// directly coded, compatible AV1 or HEVC tiles, never a recursive derived graph.
func (r *avifReader) grid(main *avifItem, items map[uint64]*avifItem, properties []avifBox, info avifInfo, policy InspectPolicy) error {
	var descriptor []byte
	for _, extent := range main.extents {
		if extent.length > 12-int64(len(descriptor)) {
			return errAVIF
		}
		part, err := r.read(extent.offset, extent.length)
		if err != nil {
			return err
		}
		descriptor = append(descriptor, part...)
	}
	if len(descriptor) < 8 || descriptor[0] != 0 || descriptor[1] > 1 {
		return errAVIF
	}
	rows, columns := int64(descriptor[2])+1, int64(descriptor[3])+1
	if rows*columns > 256 {
		return errMetadataLimit
	}
	if int64(len(main.derived)) != rows*columns {
		return errAVIF
	}
	var width, height int64
	if descriptor[1] == 0 {
		if len(descriptor) != 8 {
			return errAVIF
		}
		width, height = int64(binary.BigEndian.Uint16(descriptor[4:6])), int64(binary.BigEndian.Uint16(descriptor[6:8]))
	} else {
		if len(descriptor) != 12 {
			return errAVIF
		}
		width, height = int64(binary.BigEndian.Uint32(descriptor[4:8])), int64(binary.BigEndian.Uint32(descriptor[8:12]))
	}
	if width != info.width || height != info.height {
		return errAVIF
	}
	var tileWidth, tileHeight int64
	var configuration []byte
	coding, configType := "av01", "av1C"
	if r.hevc {
		coding, configType = "hvc1", "hvcC"
	}
	for _, id := range main.derived {
		tile := items[id]
		if tile == nil {
			return errAVIF
		}
		if tile.kind != coding || len(tile.derived) != 0 {
			return errAVIFUnsupported
		}
		geometry, err := avifPrimaryProperties(tile, properties, policy)
		if err != nil {
			return err
		}
		if geometry.orientation != nil {
			return errAVIFUnsupported
		}
		if geometry.width < 64 || geometry.height < 64 || geometry.width%64 != 0 || geometry.height%64 != 0 {
			return errAVIFUnsupported
		}
		if r.hevc {
			if err := r.hevcItem(tile); err != nil {
				return err
			}
		}
		var config []byte
		for _, index := range tile.properties {
			if properties[index-1].kind == configType {
				config = properties[index-1].body
			}
		}
		if tileWidth == 0 {
			tileWidth, tileHeight, configuration = geometry.width, geometry.height, config
		} else if geometry.width != tileWidth || geometry.height != tileHeight || !bytes.Equal(configuration, config) {
			return errAVIFUnsupported
		}
	}
	if tileWidth*(columns-1) >= width || width > tileWidth*columns || tileHeight*(rows-1) >= height || height > tileHeight*rows {
		return errAVIF
	}
	return nil
}
