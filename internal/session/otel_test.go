package session

import (
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

func TestOTelResourceAttributes(t *testing.T) {
	tests := []struct {
		name      string
		inherited string
		id        DeckSessionID
		sessName  string
		repoPath  string
		want      string
	}{
		{"all three", "", "abc", "iori-3b06", "/src/ah-product", "deck.session_id=abc,deck.name=iori-3b06,deck.repo=ah-product"},
		{"no name", "", "abc", "", "/src/ah-product", "deck.session_id=abc,deck.repo=ah-product"},
		{"no repo", "", "abc", "iori-3b06", "", "deck.session_id=abc,deck.name=iori-3b06"},
		{"repo with trailing slash", "", "abc", "", "/src/ah-product/", "deck.session_id=abc,deck.repo=ah-product"},
		{"inherited kept in front", "service.name=x", "abc", "iori-3b06", "/src/ah-product", "service.name=x,deck.session_id=abc,deck.name=iori-3b06,deck.repo=ah-product"},
		{"inherited only", "service.name=x", "", "", "", "service.name=x"},
		{"inherited not re-escaped", "a=b%2Cc", "abc", "", "", "a=b%2Cc,deck.session_id=abc"},
		{"parent session's deck keys dropped", "service.name=x,deck.session_id=p,deck.name=parent,deck.repo=r", "abc", "child", "", "service.name=x,deck.session_id=abc,deck.name=child"},
		{"empty inherited elements dropped", "a=1,,b=2", "abc", "", "", "a=1,b=2,deck.session_id=abc"},
		{"nothing", "", "", "", "", ""},
		{"unreserved kept", "", "a-b_c.d~e", "", "", "deck.session_id=a-b_c.d~e"},
		{"separators escaped", "", "abc", "a,b=c d%e", "", "deck.session_id=abc,deck.name=a%2Cb%3Dc%20d%25e"},
		{"multibyte escaped", "", "abc", "作業", "", "deck.session_id=abc,deck.name=%E4%BD%9C%E6%A5%AD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := otelResourceAttributes(tt.inherited, tt.id, tt.sessName, tt.repoPath); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Whatever the name and repository are, the value splits back into the three
// deck keys, and each value decodes to what went in.
func TestOTelResourceAttributes_RoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune("ab ,=%;\"\\\t-_.~作/")
	randStr := func() string {
		r := make([]rune, 1+rng.IntN(8))
		for i := range r {
			r[i] = alphabet[rng.IntN(len(alphabet))]
		}
		return string(r)
	}
	for range 300 {
		name, repo := randStr(), "/x/"+strings.ReplaceAll(randStr(), "/", "")
		if strings.TrimRight(repo, "/") == "/x" {
			continue
		}
		got := map[string]string{}
		for kv := range strings.SplitSeq(otelResourceAttributes("", "id", name, repo), ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				t.Fatalf("element %q is not key=value", kv)
			}
			dv, err := url.PathUnescape(v)
			if err != nil {
				t.Fatalf("value %q does not decode: %v", v, err)
			}
			if _, dup := got[k]; dup {
				t.Fatalf("key %q appears twice", k)
			}
			got[k] = dv
		}
		want := map[string]string{"deck.session_id": "id", "deck.name": name, "deck.repo": repo[len("/x/"):]}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("name=%q repo=%q: %s = %q, want %q", name, repo, k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("name=%q repo=%q: keys %v", name, repo, got)
		}
	}
}
