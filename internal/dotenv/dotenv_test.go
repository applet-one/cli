package dotenv

import (
	"maps"
	"testing"
)

func TestParse(t *testing.T) {
	got := Parse("# comment\nA=1\nexport B = two words # note\r\nC=\"multi\\nline\"\nD='single $HOME'\nE=`back`\nF=\nnot a pair\nG=\"open\n=skip\nH=a=b\n")
	want := map[string]string{
		"A": "1",
		"B": "two words",
		"C": "multi\nline",
		"D": "single $HOME",
		"E": "back",
		"F": "",
		"G": "\"open",
		"H": "a=b",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestParseMultilineQuoted(t *testing.T) {
	got := Parse("KEY=\"first\nsecond\"\nNEXT=1")
	if got["KEY"] != "first\nsecond" || got["NEXT"] != "1" {
		t.Fatalf("got %#v", got)
	}
}
