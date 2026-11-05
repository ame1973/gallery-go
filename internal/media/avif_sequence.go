package media

import (
	"encoding/binary"
	"math"
	"math/big"
	"time"
)

type avifTrack struct {
	id, duration, scale, declared uint64
	frames, ticks                 int64
	timing, auxiliary, pict       bool
	repeat                        *bool
	editDuration                  uint64
	stbl                          avifBox
}

func avifFind(boxes []avifBox, kind string) (avifBox, bool, error) {
	var result avifBox
	found := false
	for _, box := range boxes {
		if box.kind == kind {
			if found {
				return avifBox{}, false, errAVIF
			}
			result, found = box, true
		}
	}
	return result, found, nil
}

func avifClock(box avifBox) (uint64, uint64, error) {
	c, v, f, err := avifFull(box)
	if err != nil || v > 1 || f != 0 {
		return 0, 0, errAVIF
	}
	width := 4
	if v == 1 {
		width = 8
	}
	_ = c.take(width * 2)
	scale, duration := c.number(4), c.number(width)
	if c.err != nil || scale == 0 {
		return 0, 0, errAVIF
	}
	return scale, duration, nil
}

func (r *avifReader) sequence(movie avifBox, regions []avifExtent, policy InspectPolicy) (animationInfo, bool, error) {
	boxes, err := r.children(movie.body, movie.offset)
	if err != nil {
		return animationInfo{}, false, err
	}
	header, found, err := avifFind(boxes, "mvhd")
	if err != nil || !found {
		return animationInfo{}, false, errAVIF
	}
	movieScale, _, err := avifClock(header)
	if err != nil {
		return animationInfo{}, false, err
	}
	var tracks []avifTrack
	ids := make(map[uint64]bool)
	for _, box := range boxes {
		if box.kind != "trak" {
			continue
		}
		if len(tracks) >= 64 {
			return animationInfo{}, false, errMetadataLimit
		}
		track, err := r.track(box, regions, policy)
		if err != nil {
			return animationInfo{}, false, err
		}
		if ids[track.id] || track.id == 0 {
			return animationInfo{}, false, errAVIF
		}
		ids[track.id] = true
		tracks = append(tracks, track)
	}
	var primary *avifTrack
	for index := range tracks {
		track := &tracks[index]
		if track.pict && !track.auxiliary {
			if primary != nil {
				return animationInfo{}, false, errAVIFUnsupported
			}
			primary = track
		}
	}
	if primary == nil {
		return animationInfo{}, false, errAVIFUnsupported
	}
	info := animationInfo{frames: primary.frames}
	var cycle big.Rat
	cycle.SetFrac64(primary.ticks*1000, int64(primary.scale))
	maximum := big.NewRat(int64(policy.MaxDuration), int64(time.Millisecond))
	if cycle.Cmp(maximum) > 0 {
		return animationInfo{}, false, errMetadataLimit
	}
	if primary.timing && primary.ticks > 0 {
		value := ceilRationalMS(&cycle)
		info.duration = &value
	}
	if primary.repeat == nil {
		return info, false, nil
	}
	if !*primary.repeat {
		info.loops = 1
		return info, true, nil
	}
	// FFmpeg writes INT64_MAX for infinite AVIF; ISO BMFF also permits an all-ones
	// indefinite duration in either track-header version. Never add an initial play.
	if primary.duration == math.MaxInt64 || primary.duration == math.MaxUint64 || primary.duration == math.MaxUint32 {
		info.loops = 0
		return info, true, nil
	}
	if primary.editDuration == 0 || !primary.timing || primary.ticks == 0 {
		return info, false, nil
	}
	// Track headers use the movie clock. Require a whole number of declared edits
	// and at most one clock tick of edit-list quantization, not an FPS estimate.
	declared := new(big.Rat).SetFrac64(int64(primary.editDuration), int64(movieScale))
	actual := new(big.Rat).SetFrac64(primary.ticks, int64(primary.scale))
	difference := new(big.Rat).Sub(declared, actual)
	difference.Abs(difference)
	if difference.Cmp(big.NewRat(1, int64(movieScale))) > 0 || primary.duration%primary.editDuration != 0 {
		return info, false, nil
	}
	plays := primary.duration / primary.editDuration
	if plays == 0 || plays > math.MaxInt64 {
		return info, false, nil
	}
	info.loops = int64(plays)
	return info, true, nil
}

func (r *avifReader) track(box avifBox, regions []avifExtent, policy InspectPolicy) (avifTrack, error) {
	var result avifTrack
	children, err := r.children(box.body, box.offset)
	if err != nil {
		return result, err
	}
	header, found, err := avifFind(children, "tkhd")
	if err != nil || !found {
		return result, errAVIF
	}
	c, v, _, err := avifFull(header)
	if err != nil || v > 1 {
		return result, errAVIF
	}
	width := 4
	if v == 1 {
		width = 8
	}
	_ = c.take(width * 2)
	result.id = c.number(4)
	_ = c.number(4)
	result.duration = c.number(width)
	if c.err != nil || len(c.data) < 60 {
		return result, errAVIF
	}
	media, found, err := avifFind(children, "mdia")
	if err != nil || !found {
		return result, errAVIF
	}
	mediaBoxes, err := r.children(media.body, media.offset)
	if err != nil {
		return result, err
	}
	handler, found, err := avifFind(mediaBoxes, "hdlr")
	if err != nil || !found || len(handler.body) < 12 {
		return result, errAVIF
	}
	result.pict = string(handler.body[8:12]) == "pict"
	if !result.pict {
		return result, nil
	}
	if reference, found, err := avifFind(children, "tref"); err != nil {
		return result, err
	} else if found {
		refs, err := r.children(reference.body, reference.offset)
		if err != nil {
			return result, err
		}
		for _, ref := range refs {
			if ref.kind == "auxl" {
				if len(ref.body) < 4 || len(ref.body)%4 != 0 {
					return result, errAVIF
				}
				result.auxiliary = true
			}
		}
	}
	clock, found, err := avifFind(mediaBoxes, "mdhd")
	if err != nil || !found {
		return result, errAVIF
	}
	result.scale, result.declared, err = avifClock(clock)
	if err != nil {
		return result, err
	}
	minimum, found, err := avifFind(mediaBoxes, "minf")
	if err != nil || !found {
		return result, errAVIF
	}
	minimumBoxes, err := r.children(minimum.body, minimum.offset)
	if err != nil {
		return result, err
	}
	dataInfo, found, err := avifFind(minimumBoxes, "dinf")
	if err != nil || !found {
		return result, errAVIF
	}
	dataBoxes, err := r.children(dataInfo.body, dataInfo.offset)
	if err != nil {
		return result, err
	}
	reference, found, err := avifFind(dataBoxes, "dref")
	if err != nil || !found {
		return result, errAVIF
	}
	d, version, flags, err := avifFull(reference)
	if err != nil || version != 0 || flags != 0 || d.number(4) != 1 {
		return result, errAVIFUnsupported
	}
	entries, err := r.children(d.data, reference.offset+8)
	if err != nil || len(entries) != 1 || entries[0].kind != "url " || len(entries[0].body) != 4 || binary.BigEndian.Uint32(entries[0].body) != 1 {
		return result, errAVIFUnsupported
	}
	stbl, found, err := avifFind(minimumBoxes, "stbl")
	if err != nil || !found {
		return result, errAVIF
	}
	result.stbl = stbl
	if err := r.samples(&result, regions, policy); err != nil {
		return result, err
	}
	if edits, found, err := avifFind(children, "edts"); err != nil {
		return result, err
	} else if found {
		editBoxes, err := r.children(edits.body, edits.offset)
		if err != nil {
			return result, err
		}
		edit, found, err := avifFind(editBoxes, "elst")
		if err != nil || !found {
			return result, errAVIF
		}
		e, version, flags, err := avifFull(edit)
		if err != nil || version > 1 || flags > 1 {
			return result, errAVIF
		}
		if e.number(4) != 1 {
			return result, errAVIFUnsupported
		}
		width := 4
		if version == 1 {
			width = 8
		}
		result.editDuration = e.number(width)
		if result.editDuration > math.MaxInt64 {
			return result, errMetadataLimit
		}
		if e.number(width) != 0 || e.number(2) != 1 || e.number(2) != 0 {
			return result, errAVIFUnsupported
		}
		if e.done() != nil {
			return result, errAVIF
		}
		repeat := flags&1 != 0
		result.repeat = &repeat
	}
	return result, nil
}

func (r *avifReader) samples(track *avifTrack, regions []avifExtent, policy InspectPolicy) error {
	boxes, err := r.children(track.stbl.body, track.stbl.offset)
	if err != nil {
		return err
	}
	description, found, err := avifFind(boxes, "stsd")
	if err != nil || !found {
		return errAVIF
	}
	d, v, f, err := avifFull(description)
	if err != nil || v != 0 || f != 0 || d.number(4) != 1 {
		return errAVIFUnsupported
	}
	entries, err := r.children(d.data, description.offset+8)
	if err != nil || len(entries) != 1 || entries[0].kind != "av01" || len(entries[0].body) < 78 {
		return errAVIFUnsupported
	}
	if binary.BigEndian.Uint16(entries[0].body[6:8]) != 1 {
		return errAVIFUnsupported
	}
	sizes, found, err := avifFind(boxes, "stsz")
	if err != nil || !found {
		return errAVIF
	}
	s, v, f, err := avifFull(sizes)
	if err != nil || v != 0 || f != 0 {
		return errAVIF
	}
	common, count := s.number(4), s.number(4)
	if count == 0 {
		return errAVIF
	}
	if count > uint64(policy.MaxFrames) {
		return errMetadataLimit
	}
	track.frames = int64(count)
	lengths := make([]int64, int(count))
	for index := range lengths {
		length := common
		if common == 0 {
			length = s.number(4)
		}
		if length == 0 {
			return errAVIF
		}
		lengths[index] = int64(length)
	}
	if s.done() != nil {
		return errAVIF
	}
	timing, found, err := avifFind(boxes, "stts")
	if err != nil || !found {
		return errAVIF
	}
	t, v, f, err := avifFull(timing)
	if err != nil || v != 0 || f != 0 {
		return errAVIF
	}
	runs := t.number(4)
	if runs > uint64(policy.MaxFrames) {
		return errMetadataLimit
	}
	var samples uint64
	track.timing = true
	for range runs {
		number, delta := t.number(4), t.number(4)
		if number == 0 || number > count-samples {
			return errAVIF
		}
		samples += number
		track.ticks += int64(number * delta)
		track.timing = track.timing && delta > 0
	}
	if t.done() != nil || samples != count {
		return errAVIF
	}
	if track.declared != 0 && track.declared != math.MaxUint32 && track.declared != math.MaxUint64 && track.declared != uint64(track.ticks) {
		return errAVIF
	}
	chunks, found, err := avifFind(boxes, "stco")
	wide, wideFound, wideErr := avifFind(boxes, "co64")
	if err != nil || wideErr != nil || found == wideFound {
		return errAVIF
	}
	width := 4
	if wideFound {
		chunks, width = wide, 8
	}
	o, v, f, err := avifFull(chunks)
	if err != nil || v != 0 || f != 0 {
		return errAVIF
	}
	chunkCount := o.number(4)
	if chunkCount == 0 || chunkCount > count {
		return errAVIF
	}
	offsets := make([]uint64, int(chunkCount))
	for index := range offsets {
		offsets[index] = o.number(width)
	}
	if o.done() != nil {
		return errAVIF
	}
	mapping, found, err := avifFind(boxes, "stsc")
	if err != nil || !found {
		return errAVIF
	}
	m, v, f, err := avifFull(mapping)
	if err != nil || v != 0 || f != 0 {
		return errAVIF
	}
	mappingCount := m.number(4)
	if mappingCount == 0 || mappingCount > chunkCount {
		return errAVIF
	}
	type run struct{ first, samples uint64 }
	mappingRuns := make([]run, 0, int(mappingCount))
	for range mappingCount {
		first, samples, index := m.number(4), m.number(4), m.number(4)
		if first == 0 || first > chunkCount || samples == 0 || samples > count || index != 1 || len(mappingRuns) > 0 && first <= mappingRuns[len(mappingRuns)-1].first {
			return errAVIF
		}
		mappingRuns = append(mappingRuns, run{first, samples})
	}
	if m.done() != nil || mappingRuns[0].first != 1 {
		return errAVIF
	}
	sample, runIndex := 0, 0
	for chunk, offset := range offsets {
		if runIndex+1 < len(mappingRuns) && uint64(chunk+1) == mappingRuns[runIndex+1].first {
			runIndex++
		}
		number := mappingRuns[runIndex].samples
		if number > uint64(len(lengths)-sample) {
			return errAVIF
		}
		var length int64
		for range number {
			length += lengths[sample]
			sample++
		}
		valid := false
		for _, region := range regions {
			if offset >= uint64(region.offset) && offset-uint64(region.offset) <= uint64(region.length) && uint64(length) <= uint64(region.length)-(offset-uint64(region.offset)) {
				valid = true
				break
			}
		}
		if !valid {
			return errAVIF
		}
	}
	if sample != len(lengths) {
		return errAVIF
	}
	return nil
}
