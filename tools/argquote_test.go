package tools

import (
	"encoding/json"
	"testing"
)

func TestUnwrapQuotedArgs(t *testing.T) {
	cases := map[string]string{
		`{"path":"\\\"fact.txt\\\""}`:             `{"path":"fact.txt"}`,
		`{"path":"\"fact.txt\""}`:                 `{"path":"fact.txt"}`,
		`{"pattern":"\\\"TODO\\\""}`:              `{"pattern":"TODO"}`,
		`{"path":"a.txt","content":"\"quoted\""}`: `{"path":"a.txt","content":"\"quoted\""}`, // content keeps bare quotes
		`{"path":"plain.go"}`:                     `{"path":"plain.go"}`,
		`{"command":"echo \"hi\""}`:               `{"command":"echo \"hi\""}`,
		`not json`:                                `not json`,
	}
	for in, want := range cases {
		got := string(UnwrapQuotedArgs(json.RawMessage(in)))
		var g, w any
		if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil {
			if got != want {
				t.Errorf("%s -> %s, want %s", in, got, want)
			}
			continue
		}
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
