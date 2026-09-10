package inference

import (
	"mime/multipart"
	"testing"
)

func BenchmarkSTTOptionDecoding(b *testing.B) {
	data := []byte(`{"sample_rate":16000,"channels":2,"format":true,"multichannel":true,"diarize":1,"filler_words":"on","vad_threshold":0.25}`)
	form := &multipart.Form{Value: map[string][]string{"sample_rate": {"16000"}, "channels": {"2"}, "format": {"true"}, "multichannel": {"true"}, "diarize": {"1"}, "filler_words": {"on"}, "vad_threshold": {"0.25"}}}
	for _, encoding := range []string{"json", "multipart"} {
		b.Run(encoding, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				input, err := benchmarkDecodeSTTOptions(encoding, data, form)
				if err != nil || input.SampleRate != "16000" || input.Channels != 2 || !input.Format || !input.Multichannel || !input.Diarize || !input.FillerWords || input.VADThreshold == nil || *input.VADThreshold != 0.25 {
					b.Fatalf("bad decoded result: %+v %v", input, err)
				}
			}
		})
	}
}
