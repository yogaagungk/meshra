package relay

import (
	"encoding/binary"
	"errors"
)

func voiceValidateWAV(audio []byte, microphone bool) error {
	if len(audio) < 44 || int64(len(audio)) > voiceAudioLimit || string(audio[:4]) != "RIFF" || string(audio[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(audio[4:8]))+8 != uint64(len(audio)) {
		return errors.New("invalid bounded RIFF WAV")
	}
	var channels, bits, align uint16
	var rate, byteRate uint32
	var dataBytes uint64
	formatSeen, dataSeen := false, false
	for offset := 12; offset < len(audio); {
		if len(audio)-offset < 8 {
			return errors.New("truncated WAV chunk")
		}
		size := uint64(binary.LittleEndian.Uint32(audio[offset+4 : offset+8]))
		start := offset + 8
		end := uint64(start) + size
		if end > uint64(len(audio)) {
			return errors.New("truncated WAV chunk")
		}
		switch string(audio[offset : offset+4]) {
		case "fmt ":
			if formatSeen || size < 16 || binary.LittleEndian.Uint16(audio[start:start+2]) != 1 {
				return errors.New("WAV must contain one PCM format")
			}
			formatSeen = true
			channels = binary.LittleEndian.Uint16(audio[start+2 : start+4])
			rate = binary.LittleEndian.Uint32(audio[start+4 : start+8])
			byteRate = binary.LittleEndian.Uint32(audio[start+8 : start+12])
			align = binary.LittleEndian.Uint16(audio[start+12 : start+14])
			bits = binary.LittleEndian.Uint16(audio[start+14 : start+16])
		case "data":
			if dataSeen || size == 0 {
				return errors.New("WAV must contain one nonempty data chunk")
			}
			dataSeen = true
			dataBytes = size
		}
		end += size % 2
		if end > uint64(len(audio)) {
			return errors.New("missing WAV chunk padding")
		}
		offset = int(end)
	}
	if !formatSeen || !dataSeen || channels < 1 || channels > 2 || bits != 16 || rate < 8000 || rate > 192000 || align != channels*2 || uint64(byteRate) != uint64(rate)*uint64(align) || dataBytes%uint64(align) != 0 {
		return errors.New("invalid16-bit PCM WAV parameters")
	}
	if microphone && (channels != 1 || rate != 16000 || dataBytes > uint64(byteRate)*60) {
		return errors.New("recording must be mono16kHz PCM and at most60seconds")
	}
	return nil
}
