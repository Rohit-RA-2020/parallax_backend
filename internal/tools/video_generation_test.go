package tools

import "testing"

func TestChooseVideoProvider(t *testing.T) {
	tests := []struct {
		name     string
		task     string
		previous string
		video    bool
		last     bool
		refs     int
		duration int
		res      string
		want     string
	}{
		{name: "default omni", task: "text_to_video", want: "omni"},
		{name: "omni continuation", task: "edit", previous: "interaction-1", want: "omni"},
		{name: "veo reference", task: "reference_to_video", refs: 1, want: "veo"},
		{name: "veo interpolation", task: "interpolate", last: true, want: "veo"},
		{name: "veo extension", task: "extend", video: true, want: "veo"},
		{name: "veo duration", task: "text_to_video", duration: 6, want: "veo"},
		{name: "veo resolution", task: "text_to_video", res: "4k", want: "veo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseVideoProvider(tt.task, tt.previous, tt.video, tt.last, tt.refs, tt.duration, tt.res)
			if err != nil || got != tt.want {
				t.Fatalf("provider=%q err=%v want=%q", got, err, tt.want)
			}
		})
	}
}

func TestChooseVideoProviderRejectsConflictingContinuation(t *testing.T) {
	if _, err := chooseVideoProvider("edit", "interaction-1", false, false, 0, 8, ""); err == nil {
		t.Fatal("expected Omni/Veo conflict")
	}
}

func TestChooseVideoProviderValidatesSpecialTasks(t *testing.T) {
	if _, err := chooseVideoProvider("extend", "", false, false, 0, 0, ""); err == nil {
		t.Fatal("expected missing source video error")
	}
	if _, err := chooseVideoProvider("interpolate", "", false, false, 0, 0, ""); err == nil {
		t.Fatal("expected missing last frame error")
	}
}
