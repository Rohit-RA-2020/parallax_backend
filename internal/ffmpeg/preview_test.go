package ffmpeg

import "testing"

func TestAV1PreviewAvoidsFullCUDADecode(t *testing.T) {
	bins := Bins{Accel: Accel{
		Backend: "cuda",
		Label:   "NVIDIA GeForce GTX 1650",
		H264:    "h264_nvenc",
	}}

	plan := PreviewEncodePlanForSource(bins, "av1")
	if !plan.Hardware || plan.Encoder != "h264_nvenc" || plan.Pipeline != "gpu_encode" {
		t.Fatalf("AV1 preview plan=%+v", plan)
	}
	if fullGPUPreviewSupported(bins.Accel, "av1") {
		t.Fatal("AV1 must not force CUDA decoding when decoder capabilities are unknown")
	}
	if !fullGPUPreviewSupported(bins.Accel, "h264") {
		t.Fatal("H.264 should retain the full CUDA pipeline")
	}
}
