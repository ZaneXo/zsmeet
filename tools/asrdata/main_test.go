package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseTextGridSpeakerTiers(t *testing.T) {
	const sample = `File type = "ooTextFile"
Object class = "TextGrid"

xmin = 0
xmax = 5
tiers? <exists>
size = 2
item []:
    item [1]:
        class = "IntervalTier"
        name = "SPK001"
        xmin = 0
        xmax = 5
        intervals: size = 2
        intervals [1]:
            xmin = 0.2
            xmax = 1.5
            text = "今天先过一下排期<sil>"
        intervals [2]:
            xmin = 1.5
            xmax = 5
            text = ""
    item [2]:
        class = "IntervalTier"
        name = "SPK002"
        xmin = 0
        xmax = 5
        intervals: size = 1
        intervals [1]:
            xmin = 1.8
            xmax = 3.6
            text = "[laughter]好的没问题"
`
	p := filepath.Join(t.TempDir(), "test.TextGrid")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseTextGrid(p)
	if err != nil {
		t.Fatal(err)
	}
	var cleaned []refSegment
	for _, iv := range got {
		if txt := cleanText(iv.text); txt != "" {
			cleaned = append(cleaned, refSegment{Start: iv.start, End: iv.end, Speaker: iv.speaker, Text: txt})
		}
	}
	want := []refSegment{
		{Start: 0.2, End: 1.5, Speaker: 0, Text: "今天先过一下排期"},
		{Start: 1.8, End: 3.6, Speaker: 1, Text: "好的没问题"},
	}
	if !slices.Equal(cleaned, want) {
		t.Fatalf("got %+v, want %+v", cleaned, want)
	}
}
