package hivesrc

import (
	"encoding/json"
	"testing"
)

// json_metadata is NOT always a string, and a post that sends an object used to kill
// the whole collection.
//
// The field was declared `string` with a comment asserting "It is a STRING containing
// JSON, not nested JSON". Real Hive disagrees: some posts carry json_metadata as a
// nested OBJECT. Because RawPost is decoded with a single json.Unmarshal over the API's
// result array, one such post failed the ENTIRE decode:
//
//	Collect: json: cannot unmarshal object into Go struct field
//	         RawPost.json_metadata of type string
//
// Not one post dropped — every post in the window lost, and the epoch with them. The
// offline suite never caught it because every fixture wrote json_metadata the way the
// struct expected, so the fake agreed with the code by construction. It surfaced only
// against real nodes, which is exactly what `REPORTER_LIVE=1` exists for.
//
// The rule this pins: json_metadata is author-controlled untrusted input on a path that
// must never fail an epoch. Both encodings must decode, and anything unparseable must
// degrade to empty metadata rather than an error.
func TestRawPost_JSONMetadataAcceptsObjectAndString(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantTags []string
		wantApp  string
	}{{
		name:     "string form — json_metadata is a JSON string containing JSON",
		raw:      `{"json_metadata":"{\"tags\":[\"alpha\",\"beta\"],\"app\":\"peakd/2023.1\"}"}`,
		wantTags: []string{"alpha", "beta"},
		wantApp:  "peakd/2023.1",
	}, {
		name:     "OBJECT form — what broke the live run",
		raw:      `{"json_metadata":{"tags":["alpha","beta"],"app":"peakd/2023.1"}}`,
		wantTags: []string{"alpha", "beta"},
		wantApp:  "peakd/2023.1",
	}, {
		name:     "object form with app as an object, as seen in the wild",
		raw:      `{"json_metadata":{"tags":["gamma"],"app":{"name":"ecency","version":"3"}}}`,
		wantTags: []string{"gamma"},
		wantApp:  "ecency",
	}, {
		name:     "object form with tags as a bare string",
		raw:      `{"json_metadata":{"tags":"solo"}}`,
		wantTags: []string{"solo"},
	}, {
		name: "null degrades to empty, not an error",
		raw:  `{"json_metadata":null}`,
	}, {
		name: "absent degrades to empty",
		raw:  `{"author":"alice"}`,
	}, {
		name: "malformed string blob degrades to empty — author-controlled input",
		raw:  `{"json_metadata":"not json at all"}`,
	}, {
		name: "a bare number is still not an error",
		raw:  `{"json_metadata":12345}`,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p RawPost
			if err := json.Unmarshal([]byte(c.raw), &p); err != nil {
				t.Fatalf("decoding a post must never fail on author-controlled metadata: %v", err)
			}
			m := p.parseMeta()
			if len(m.Tags) != len(c.wantTags) {
				t.Fatalf("tags = %v, want %v", m.Tags, c.wantTags)
			}
			for i, want := range c.wantTags {
				if m.Tags[i] != want {
					t.Fatalf("tags[%d] = %q, want %q", i, m.Tags[i], want)
				}
			}
			if m.App != c.wantApp {
				t.Fatalf("app = %q, want %q", m.App, c.wantApp)
			}
		})
	}
}

// One object-form post must not take the rest of the window with it. This is the shape
// the live failure actually had: a batch decoded in one pass, where a single post's
// metadata encoding decided whether every other post survived.
func TestRawPost_OneObjectMetadataDoesNotKillTheBatch(t *testing.T) {
	batch := `[
	  {"author":"alice","json_metadata":"{\"tags\":[\"a\"]}"},
	  {"author":"bob","json_metadata":{"tags":["b"]}},
	  {"author":"carol","json_metadata":"{\"tags\":[\"c\"]}"}
	]`
	var posts []RawPost
	if err := json.Unmarshal([]byte(batch), &posts); err != nil {
		t.Fatalf("one object-form post killed the whole batch: %v", err)
	}
	if len(posts) != 3 {
		t.Fatalf("decoded %d posts, want 3", len(posts))
	}
	for i, want := range []string{"a", "b", "c"} {
		got := posts[i].parseMeta().Tags
		if len(got) != 1 || got[0] != want {
			t.Fatalf("posts[%d] tags = %v, want [%s]", i, got, want)
		}
	}
}
