package util

import (
	"net/http"
	"reflect"
	"testing"
)

func TestSanitizeSurrogatesPreservesEmoji(t *testing.T) {
	input := "Hello 🙈 World"
	if got := SanitizeSurrogates(input); got != input {
		t.Fatalf("SanitizeSurrogates() = %q, want %q", got, input)
	}
}

func TestSanitizeSurrogatesDropsEncodedLoneSurrogate(t *testing.T) {
	unpairedHighSurrogate := string([]byte{0xed, 0xa0, 0xbd})
	input := "Text " + unpairedHighSurrogate + " here"
	if got := SanitizeSurrogates(input); got != "Text  here" {
		t.Fatalf("SanitizeSurrogates() = %q, want %q", got, "Text  here")
	}
}

func TestHeadersToRecord(t *testing.T) {
	headers := http.Header{
		"X-Test":  []string{"a", "b"},
		"Content": []string{"application/json"},
	}
	want := map[string]string{
		"x-test":  "a, b",
		"content": "application/json",
	}
	if got := HeadersToRecord(headers); !reflect.DeepEqual(got, want) {
		t.Fatalf("HeadersToRecord() = %#v, want %#v", got, want)
	}
}

func TestProviderHeadersToRecord(t *testing.T) {
	kept := "value"
	got := ProviderHeadersToRecord(map[string]*string{
		"x-keep": &kept,
		"x-drop": nil,
	})
	want := map[string]string{"x-keep": "value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderHeadersToRecord() = %#v, want %#v", got, want)
	}

	if got := ProviderHeadersToRecord(nil); got != nil {
		t.Fatalf("ProviderHeadersToRecord(nil) = %#v, want nil", got)
	}
	if got := ProviderHeadersToRecord(map[string]*string{}); got != nil {
		t.Fatalf("ProviderHeadersToRecord(empty) = %#v, want nil", got)
	}
	if got := ProviderHeadersToRecord(map[string]*string{"x": nil}); got != nil {
		t.Fatalf("ProviderHeadersToRecord(all nil) = %#v, want nil", got)
	}
}
