package metadata

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const (
	CaptureCandidateEXIF  = "exif"
	CaptureCandidateVideo = "video"

	discardMissingDateTime = "missing_datetime"
	discardInvalidDateTime = "invalid_datetime"
	discardInvalidOffset   = "invalid_offset"
	discardUnknownKind     = "unknown_candidate_kind"
	discardDSTGap          = "nonexistent_local_time"
	discardLowerPriority   = "lower_priority"
	foldEarlierUTC         = "earlier_utc"
)

var (
	exifDateTimePattern  = regexp.MustCompile(`^(\d{4}):(\d{2}):(\d{2}) (\d{2}):(\d{2}):(\d{2})$`)
	videoDateTimePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})?$`)
)

type captureDateTime struct {
	year, month, day int
	hour, min, sec   int
	nsec             int
	offset           string
}

// ResolveCapture selects the first valid candidate. The supplied timezone is
// loaded only if an offsetless EXIF candidate needs it.
func ResolveCapture(candidates []CaptureCandidate, defaultTimezone string) (Capture, error) {
	capture := Capture{
		Source:     TakenAtUnknown,
		Candidates: append([]CaptureCandidate(nil), candidates...),
	}
	var location *time.Location
	for i := range capture.Candidates {
		capture.Candidates[i].Discarded = ""
	}

	for i := range capture.Candidates {
		candidate := &capture.Candidates[i]
		kind := candidate.Kind
		if kind == "" {
			kind = inferredCandidateKind(candidate.Name)
		}
		if kind != CaptureCandidateEXIF && kind != CaptureCandidateVideo {
			candidate.Discarded = discardUnknownKind
			continue
		}
		candidate.Kind = kind

		parsed, ok := parseCaptureDateTime(candidate.DateTime, kind)
		if !ok {
			if candidate.DateTime == "" {
				candidate.Discarded = discardMissingDateTime
			} else {
				candidate.Discarded = discardInvalidDateTime
			}
			continue
		}

		offset := parsed.offset
		offsetPresent := candidate.OffsetPresent || candidate.Offset != ""
		if kind == CaptureCandidateVideo {
			if offsetPresent {
				candidate.Discarded = discardInvalidOffset
				continue
			}
		} else if offsetPresent {
			if !candidate.OffsetOK {
				candidate.Discarded = discardInvalidOffset
				continue
			}
			var valid bool
			if _, valid = parseNumericOffset(candidate.Offset); !valid {
				candidate.Discarded = discardInvalidOffset
				continue
			}
			offset = candidate.Offset
		}

		var instant time.Time
		if offset != "" {
			offsetSeconds, valid := parseCandidateOffset(offset, kind)
			if !valid {
				candidate.Discarded = discardInvalidOffset
				continue
			}
			instant = parsed.inLocation(time.FixedZone(offset, offsetSeconds)).UTC()
			capture.Source = TakenAtEmbeddedOffset
		} else {
			if location == nil {
				var err error
				location, err = time.LoadLocation(defaultTimezone)
				if err != nil {
					return Capture{}, fmt.Errorf("invalid default timezone %q: %w", defaultTimezone, err)
				}
			}
			instants := localInstants(parsed, location)
			if len(instants) == 0 {
				candidate.Discarded = discardDSTGap
				continue
			}
			instant = instants[0]
			capture.Source = TakenAtDefaultTimezone
			timezone := defaultTimezone
			capture.Timezone = &timezone
			if len(instants) > 1 {
				capture.FoldDecision = foldEarlierUTC
			}
		}

		capture.TakenAt = &instant
		selected := *candidate
		capture.Selected = &selected
		selectedIndex := i
		capture.SelectedIndex = &selectedIndex
		for j := i + 1; j < len(capture.Candidates); j++ {
			capture.Candidates[j].Discarded = discardLowerPriority
		}
		return capture, nil
	}

	return capture, nil
}

func parseCaptureDateTime(value, kind string) (captureDateTime, bool) {
	var match []string
	switch kind {
	case CaptureCandidateEXIF:
		match = exifDateTimePattern.FindStringSubmatch(value)
		if match == nil {
			return captureDateTime{}, false
		}
		return captureDateTimeFromMatch(match[1:7], "", "")
	case CaptureCandidateVideo:
		match = videoDateTimePattern.FindStringSubmatch(value)
	default:
		return captureDateTime{}, false
	}
	if match == nil {
		return captureDateTime{}, false
	}
	return captureDateTimeFromMatch(match[1:7], match[7], match[8])
}

func inferredCandidateKind(name string) string {
	switch name {
	case "DateTimeOriginal", "DateTimeDigitized", "DateTime":
		return CaptureCandidateEXIF
	case "primary_stream_creation_time", "container_creation_time":
		return CaptureCandidateVideo
	default:
		return ""
	}
}

func captureDateTimeFromMatch(parts []string, fraction, offset string) (captureDateTime, bool) {
	values := make([]int, len(parts))
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return captureDateTime{}, false
		}
		values[i] = value
	}

	nsec := 0
	if fraction != "" {
		padded := fraction + "000000000"[:9-len(fraction)]
		nsec, _ = strconv.Atoi(padded)
	}
	parsed := captureDateTime{
		year: values[0], month: values[1], day: values[2],
		hour: values[3], min: values[4], sec: values[5],
		nsec: nsec, offset: offset,
	}

	// time.Date normalizes invalid fields, so verify the round trip in UTC.
	if !parsed.matches(parsed.inLocation(time.UTC), time.UTC) {
		return captureDateTime{}, false
	}
	return parsed, true
}

func parseNumericOffset(value string) (int, bool) {
	if len(value) != 6 {
		return 0, false
	}
	if value[0] != '+' && value[0] != '-' {
		return 0, false
	}
	if value[3] != ':' {
		return 0, false
	}
	digits := value[1:3] + value[4:6]
	hours, hourErr := strconv.Atoi(digits[:2])
	minutes, minuteErr := strconv.Atoi(digits[2:])
	if hourErr != nil || minuteErr != nil || hours > 23 || minutes > 59 {
		return 0, false
	}
	seconds := hours*60*60 + minutes*60
	if value[0] == '-' {
		seconds = -seconds
	}
	return seconds, true
}

func parseCandidateOffset(value, kind string) (int, bool) {
	if kind == CaptureCandidateVideo {
		if value == "Z" {
			return 0, true
		}
		if value == "-00:00" {
			return 0, false
		}
	}
	return parseNumericOffset(value)
}

func localInstants(parsed captureDateTime, location *time.Location) []time.Time {
	anchor := parsed.inLocation(location)
	offsets := make(map[int]struct{})
	for elapsed := -72 * time.Hour; elapsed <= 72*time.Hour; elapsed += 15 * time.Minute {
		_, offset := anchor.Add(elapsed).Zone()
		offsets[offset] = struct{}{}
	}

	wallUTC := parsed.inLocation(time.UTC)
	instants := make([]time.Time, 0, 2)
	for offset := range offsets {
		instant := wallUTC.Add(-time.Duration(offset) * time.Second).UTC()
		if parsed.matches(instant, location) {
			instants = append(instants, instant)
		}
	}
	sort.Slice(instants, func(i, j int) bool { return instants[i].Before(instants[j]) })
	return instants
}

func (parsed captureDateTime) inLocation(location *time.Location) time.Time {
	return time.Date(parsed.year, time.Month(parsed.month), parsed.day, parsed.hour, parsed.min, parsed.sec, parsed.nsec, location)
}

func (parsed captureDateTime) matches(value time.Time, location *time.Location) bool {
	local := value.In(location)
	return local.Year() == parsed.year && int(local.Month()) == parsed.month && local.Day() == parsed.day &&
		local.Hour() == parsed.hour && local.Minute() == parsed.min && local.Second() == parsed.sec &&
		local.Nanosecond() == parsed.nsec
}
