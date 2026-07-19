package truncate

import "testing"

func TestFormatSize(t *testing.T) {
	cases := map[int]string{
		0:                 "0B",
		1023:              "1023B",
		1024:              "1.0KB",
		1536:              "1.5KB",
		1024 * 1024:       "1.0MB",
		1536 * 1024:       "1.5MB",
		DefaultMaxBytes:   "50.0KB",
		GrepMaxLineLength: "500B",
	}
	for input, want := range cases {
		if got := FormatSize(input); got != want {
			t.Fatalf("FormatSize(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestHeadNoTruncationCountsTrailingNewline(t *testing.T) {
	got := Head("a\nb\n", Options{MaxLines: Int(5), MaxBytes: Int(20)})
	if got.Truncated || got.Content != "a\nb\n" || got.TotalLines != 2 || got.OutputLines != 2 || got.TotalBytes != 4 || got.OutputBytes != 4 {
		t.Fatalf("Head no truncation = %+v", got)
	}
}

func TestHeadLineAndByteLimits(t *testing.T) {
	byLines := Head("a\nb\nc", Options{MaxLines: Int(2), MaxBytes: Int(100)})
	if !byLines.Truncated || byLines.TruncatedBy != TruncatedByLines || byLines.Content != "a\nb" || byLines.OutputLines != 2 || byLines.OutputBytes != 3 {
		t.Fatalf("Head line limit = %+v", byLines)
	}

	byBytes := Head("aa\nbb\ncc", Options{MaxLines: Int(100), MaxBytes: Int(5)})
	if !byBytes.Truncated || byBytes.TruncatedBy != TruncatedByBytes || byBytes.Content != "aa\nbb" || byBytes.OutputBytes != 5 {
		t.Fatalf("Head byte limit = %+v", byBytes)
	}

	firstLine := Head("abcdef\nsecond", Options{MaxLines: Int(100), MaxBytes: Int(5)})
	if !firstLine.Truncated || !firstLine.FirstLineExceedsLimit || firstLine.Content != "" || firstLine.OutputLines != 0 {
		t.Fatalf("Head first-line limit = %+v", firstLine)
	}
}

func TestTailLineAndByteLimits(t *testing.T) {
	byLines := Tail("a\nb\nc", Options{MaxLines: Int(2), MaxBytes: Int(100)})
	if !byLines.Truncated || byLines.TruncatedBy != TruncatedByLines || byLines.Content != "b\nc" || byLines.OutputLines != 2 || byLines.OutputBytes != 3 {
		t.Fatalf("Tail line limit = %+v", byLines)
	}

	byBytes := Tail("aa\nbb\ncc", Options{MaxLines: Int(100), MaxBytes: Int(5)})
	if !byBytes.Truncated || byBytes.TruncatedBy != TruncatedByBytes || byBytes.Content != "bb\ncc" || byBytes.OutputBytes != 5 {
		t.Fatalf("Tail byte limit = %+v", byBytes)
	}
}

func TestTailPartialLinePreservesUTF8Boundary(t *testing.T) {
	got := Tail("ééé", Options{MaxLines: Int(10), MaxBytes: Int(5)})
	if !got.Truncated || got.TruncatedBy != TruncatedByBytes || !got.LastLinePartial || got.Content != "éé" || got.OutputBytes != 4 {
		t.Fatalf("Tail partial UTF-8 = %+v", got)
	}
}

func TestLine(t *testing.T) {
	if got := Line("abc", 3); got.WasTruncated || got.Text != "abc" {
		t.Fatalf("Line no truncation = %+v", got)
	}
	if got := Line("abcdef", 3); !got.WasTruncated || got.Text != "abc... [truncated]" {
		t.Fatalf("Line truncation = %+v", got)
	}
	long := make([]rune, GrepMaxLineLength+1)
	for i := range long {
		long[i] = 'x'
	}
	if got := Line(string(long), 0); !got.WasTruncated || len([]rune(got.Text)) != GrepMaxLineLength+len([]rune(truncatedLineSuffix)) {
		t.Fatalf("Line default max = %+v", got)
	}
}
