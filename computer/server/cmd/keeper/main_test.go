package main

import (
	"reflect"
	"testing"
)

func TestParseItemized(t *testing.T) {
	out := `.d..t...... ./
*deleting   Documents/client/notes.txt
.d..t...... Documents/
cd+++++++++ Documents/new/
>f+++++++++ Documents/new/a.txt
>f.st...... Documents/plan.md
`
	want := []change{
		{"deleted", "Documents/client/notes.txt"},
		{"added", "Documents/new/"},
		{"added", "Documents/new/a.txt"},
		{"changed", "Documents/plan.md"},
	}
	if got := parseItemized(out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
