package ffmpeg

import (
	"strconv"
	"strings"
)

var softwareFamilies = map[string]string{
	"libx264":    "h264",
	"h264":       "h264",
	"libx265":    "hevc",
	"libvpx-vp9": "vp9",
	"vp9":        "vp9",
	"libsvtav1":  "av1",
	"libaom-av1": "av1",
	"librav1e":   "av1",
}

// RewriteSoftwareEncode swaps CPU video encoders for the selected GPU backend.
// Returns ok=false when the argv should run unchanged.
func RewriteSoftwareEncode(args []string, accel Accel) ([]string, bool) {
	if !accel.Enabled() || len(args) == 0 {
		return args, false
	}
	if alreadyUsesHWEncoder(args) {
		return args, false
	}
	if accel.needsUpload() && hasFilterComplex(args) {
		// Overlay/drawtext/subtitles graphs stay in system memory; VAAPI/QSV
		// need hwupload and cannot be spliced into those graphs safely.
		return args, false
	}

	out := append([]string(nil), args...)
	families := map[string]bool{}
	changed := false
	for i := 0; i < len(out); i++ {
		if !looksLikeFlag(out[i]) {
			continue
		}
		name, inline := splitFlag(out[i])
		if !isVideoCodecFlag(name) {
			continue
		}
		val := inline
		valIdx := -1
		if val == "" {
			if i+1 >= len(out) || looksLikeFlag(out[i+1]) {
				continue
			}
			i++
			val = out[i]
			valIdx = i
		}
		lower := strings.ToLower(strings.TrimSpace(val))
		if lower == "copy" {
			continue
		}
		family, ok := softwareFamilies[lower]
		if !ok {
			continue
		}
		enc := accel.encoderFor(family)
		if enc == "" {
			continue
		}
		if valIdx >= 0 {
			out[valIdx] = enc
		} else {
			out[i] = name + "=" + enc
		}
		families[family] = true
		changed = true
	}
	if !changed {
		return args, false
	}

	out = remapSoftwareOptions(out, accel)
	if accel.needsUpload() {
		out = injectHWUpload(out, accel)
	}
	if init := hwInitArgs(accel); len(init) > 0 && !hasFlag(out, "-init_hw_device") {
		out = insertAfterGlobals(out, init)
	}
	out = ensureHWDefaults(out, accel)
	return out, true
}

func alreadyUsesHWEncoder(args []string) bool {
	for i := 0; i < len(args); i++ {
		if !looksLikeFlag(args[i]) {
			continue
		}
		name, inline := splitFlag(args[i])
		if !isVideoCodecFlag(name) {
			continue
		}
		val := inline
		if val == "" && i+1 < len(args) && !looksLikeFlag(args[i+1]) {
			i++
			val = args[i]
		}
		if isHWEncoderName(val) {
			return true
		}
	}
	return false
}

func isHWEncoderName(val string) bool {
	lower := strings.ToLower(strings.TrimSpace(val))
	for _, suf := range []string{"_nvenc", "_vaapi", "_qsv", "_videotoolbox", "_amf", "_vulkan"} {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

func isVideoCodecFlag(name string) bool {
	switch name {
	case "-c:v", "-vcodec", "-codec:v", "-c:v:0", "-codec:v:0", "-c", "-codec":
		return true
	}
	return false
}

func hasFilterComplex(args []string) bool {
	return hasFlag(args, "-filter_complex") || hasFlag(args, "-filter_complex_script") || hasFlag(args, "-lavfi")
}

func remapSoftwareOptions(args []string, accel Accel) []string {
	hasRC := hasFlag(args, "-rc")
	out := make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		if !looksLikeFlag(args[i]) {
			out = append(out, args[i])
			continue
		}
		name, inline := splitFlag(args[i])
		switch name {
		case "-crf", "-crf:v":
			val, next := flagValue(args, i, inline)
			i = next
			out = append(out, qualityFromCRF(accel, val, hasRC)...)
		case "-preset", "-preset:v":
			val, next := flagValue(args, i, inline)
			i = next
			if mapped := mapPreset(accel, val); mapped != "" {
				out = append(out, "-preset", mapped)
			}
		case "-tune", "-tune:v":
			val, next := flagValue(args, i, inline)
			i = next
			if mapped := mapTune(accel, val); mapped != "" {
				out = append(out, "-tune", mapped)
			}
		case "-x264-params", "-x265-params", "-x264opts", "-x265opts", "-svtav1-params":
			_, next := flagValue(args, i, inline)
			i = next
		default:
			out = append(out, args[i])
		}
	}
	return out
}

func flagValue(args []string, i int, inline string) (string, int) {
	if inline != "" {
		return inline, i
	}
	if i+1 < len(args) && !looksLikeFlag(args[i+1]) {
		return args[i+1], i + 1
	}
	return "", i
}

func qualityFromCRF(accel Accel, val string, hasRC bool) []string {
	if strings.TrimSpace(val) == "" {
		val = "20"
	}
	switch accel.Backend {
	case "cuda":
		if hasRC {
			return []string{"-cq", val}
		}
		return []string{"-rc", "vbr", "-cq", val, "-b:v", "0"}
	case "vaapi":
		return []string{"-qp", val}
	case "qsv":
		return []string{"-global_quality", val}
	case "videotoolbox":
		return []string{"-q:v", videotoolboxQuality(val)}
	}
	return nil
}

func videotoolboxQuality(crf string) string {
	n, err := strconv.Atoi(strings.TrimSpace(crf))
	if err != nil || n <= 0 {
		n = 20
	}
	q := 100 - n*2
	if q < 20 {
		q = 20
	}
	if q > 90 {
		q = 90
	}
	return strconv.Itoa(q)
}

func mapPreset(accel Accel, val string) string {
	val = strings.ToLower(strings.TrimSpace(val))
	if val == "" {
		return ""
	}
	switch accel.Backend {
	case "cuda":
		switch val {
		case "ultrafast", "superfast", "p1":
			return "p1"
		case "veryfast", "p2":
			return "p2"
		case "faster", "p3":
			return "p3"
		case "fast", "medium", "default", "p4", "hp", "hq", "bd", "ll", "llhq", "llhp", "lossless", "losslesshp":
			if val == "fast" || val == "medium" || val == "default" {
				return "p4"
			}
			return val
		case "slow", "p5":
			return "p5"
		case "slower", "p6":
			return "p6"
		case "veryslow", "placebo", "p7":
			return "p7"
		default:
			if len(val) == 2 && val[0] == 'p' && val[1] >= '1' && val[1] <= '7' {
				return val
			}
			return "p4"
		}
	case "qsv", "vaapi":
		switch val {
		case "ultrafast", "superfast", "veryfast", "faster":
			return "veryfast"
		case "veryslow", "placebo", "slower":
			return "veryslow"
		case "fast", "medium", "slow":
			return val
		default:
			return "medium"
		}
	case "videotoolbox":
		return ""
	}
	return val
}

func mapTune(accel Accel, val string) string {
	val = strings.ToLower(strings.TrimSpace(val))
	if accel.Backend != "cuda" {
		return ""
	}
	switch val {
	case "hq", "ll", "ull", "lossless":
		return val
	case "zerolatency":
		return "ll"
	default:
		return "hq"
	}
}

func injectHWUpload(args []string, accel Accel) []string {
	vf := hwUploadFilter(accel)
	if vf == "" {
		return args
	}
	for i := 0; i < len(args); i++ {
		if !looksLikeFlag(args[i]) {
			continue
		}
		name, inline := splitFlag(args[i])
		if name != "-vf" && name != "-filter" {
			continue
		}
		if inline != "" {
			args[i] = name + "=" + appendUpload(inline, vf)
			return args
		}
		if i+1 < len(args) {
			args[i+1] = appendUpload(args[i+1], vf)
			return args
		}
	}
	return insertBeforeOutput(args, []string{"-vf", vf})
}

func appendUpload(existing, upload string) string {
	if strings.Contains(existing, "hwupload") {
		return existing
	}
	if existing == "" {
		return upload
	}
	return existing + "," + upload
}

func ensureHWDefaults(args []string, accel Accel) []string {
	var extra []string
	switch accel.Backend {
	case "cuda":
		if !hasFlag(args, "-preset") {
			extra = append(extra, "-preset", "p4")
		}
		if !hasFlag(args, "-tune") {
			extra = append(extra, "-tune", "hq")
		}
		if !hasFlag(args, "-rc") && !hasFlag(args, "-cq") {
			extra = append(extra, "-rc", "vbr", "-cq", "20", "-b:v", "0")
		}
		if !hasFlag(args, "-gpu") {
			extra = append(extra, hwEncoderExtras(accel)...)
		}
	case "vaapi":
		if !hasFlag(args, "-qp") && !hasFlag(args, "-global_quality") {
			extra = append(extra, "-qp", "20")
		}
	case "qsv":
		if !hasFlag(args, "-global_quality") && !hasFlag(args, "-qp") {
			extra = append(extra, "-global_quality", "20")
		}
	case "videotoolbox":
		if !hasFlag(args, "-q:v") && !hasFlag(args, "-b:v") {
			extra = append(extra, "-q:v", "65")
		}
	}
	if len(extra) == 0 {
		return args
	}
	return insertBeforeOutput(args, extra)
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if !looksLikeFlag(arg) {
			continue
		}
		got, _ := splitFlag(arg)
		if got == name {
			return true
		}
	}
	return false
}

func insertAfterGlobals(args, extra []string) []string {
	i := 0
	for i < len(args) {
		if !looksLikeFlag(args[i]) {
			break
		}
		name, inline := splitFlag(args[i])
		if !isLeadingGlobal(name) {
			break
		}
		i++
		if inline == "" && globalTakesValue(name) && i < len(args) && !looksLikeFlag(args[i]) {
			i++
		}
	}
	out := make([]string, 0, len(args)+len(extra))
	out = append(out, args[:i]...)
	out = append(out, extra...)
	out = append(out, args[i:]...)
	return out
}

func isLeadingGlobal(name string) bool {
	switch name {
	case "-y", "-n", "-hide_banner", "-nostdin", "-stats", "-nostats",
		"-loglevel", "-v", "-report":
		return true
	}
	return false
}

func globalTakesValue(name string) bool {
	switch name {
	case "-loglevel", "-v":
		return true
	}
	return false
}

func insertBeforeOutput(args, extra []string) []string {
	last := len(args)
	for i := len(args) - 1; i >= 0; i-- {
		if looksLikeFlag(args[i]) {
			continue
		}
		if i > 0 && looksLikeFlag(args[i-1]) {
			name, inline := splitFlag(args[i-1])
			if inline == "" && flagLikelyTakesValue(name) {
				continue
			}
		}
		last = i
		break
	}
	out := make([]string, 0, len(args)+len(extra))
	out = append(out, args[:last]...)
	out = append(out, extra...)
	out = append(out, args[last:]...)
	return out
}

func flagLikelyTakesValue(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "-y", "-n", "-hide_banner", "-nostdin", "-an", "-vn", "-sn",
		"-dn", "-stats", "-nostats", "-report", "-h", "-version":
		return false
	}
	return true
}
