package stories

import (
	"slices"
	"strings"
	"testing"
)

const sampleBranchingTwee = `:: StoryTitle
The Whispering Woods

:: StoryData
{"ifid":"12345678-1234-4123-8123-1234567890ab","start":"Woods"}

:: Woods {"bg":"misty pine forest"}
You stand at a fork in the trail.
<<set $visited to true>><<set $coins to 2>>
[[Take the mossy path->Moss]]
[[Follow the lantern|Lantern]]
[[Wait]]

:: Moss
A tiny frog offers you a key.
<<set $has_key to true>>
[[Go to the gate->Gate]]

:: Lantern
The lantern warms your hands.
[[Go to the gate->Gate]]

:: Wait
You hear an owl hoot.
[[Follow the lantern|Lantern]]

:: Gate {"end":true}
<<if $has_key>>The gate clicks open to a sunlit garden!<<else>>The gate is locked, so you rest under the stars.<<endif>>
`

func TestParseTweeValidAndRuntime(t *testing.T) {
	t.Parallel()
	cs, err := Parse(sampleBranchingTwee)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cs.Title != "The Whispering Woods" {
		t.Fatalf("unexpected title: %q", cs.Title)
	}
	if cs.Passages[cs.Start].Name != "Woods" || cs.Passages[cs.Start].BG != "misty pine forest" {
		t.Fatalf("unexpected start passage: %+v", cs.Passages[cs.Start])
	}
	flags := cs.FlagsSlice(nil)
	txt, choices := cs.EvalPassage(cs.Start, flags, true)
	if !strings.Contains(txt, "fork in the trail") || len(choices) != 3 {
		t.Fatalf("unexpected start eval: text=%q choices=%v", txt, choices)
	}
	if !flags[cs.FlagIdx["visited"]].Truthy() || flags[cs.FlagIdx["coins"]].Num != 2 {
		t.Fatalf("unexpected flags after start: %+v", flags)
	}

	// Branch 1: Lantern -> Gate (without key).
	_, lChoices := cs.EvalPassage(choices[1].Target, flags, true)
	gateText, gateChoices := cs.EvalPassage(lChoices[0].Target, flags, true)
	if len(gateChoices) != 0 || !strings.Contains(gateText, "locked") {
		t.Fatalf("expected locked ending, got text=%q choices=%v", gateText, gateChoices)
	}

	// Branch 2: Moss -> Gate (with key).
	_, mChoices := cs.EvalPassage(choices[0].Target, flags, true)
	gateText2, _ := cs.EvalPassage(mChoices[0].Target, flags, true)
	if !strings.Contains(gateText2, "sunlit garden") {
		t.Fatalf("expected unlocked ending, got %q", gateText2)
	}
}

func TestParseTweeValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		twee string
		want string
	}{
		{
			name: "missing StoryTitle",
			twee: ":: Start {\"end\":true}\nHi\n",
			want: "StoryTitle",
		},
		{
			name: "missing target",
			twee: ":: StoryTitle\nT\n:: Start\nHi\n[[Go->Nowhere]]\n",
			want: "line 5:",
		},
		{
			name: "unreachable",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\nHi\n:: Secret {\"end\":true}\nHidden\n",
			want: "unreachable",
		},
		{
			name: "dead end",
			twee: ":: StoryTitle\nT\n:: Start\nNo links here.\n",
			want: "dead end",
		},
		{
			name: "ending with link",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\nDone\n[[Again->Start]]\n",
			want: "must not have links",
		},
		{
			name: "too many links",
			twee: ":: StoryTitle\nT\n:: Start\n[[A]] [[B]] [[C]] [[D]]\n:: A {\"end\":true}\nA\n:: B {\"end\":true}\nB\n:: C {\"end\":true}\nC\n:: D {\"end\":true}\nD\n",
			want: "max 3",
		},
		{
			name: "passage too long",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\n" + strings.Repeat("a", MaxPassageText+1) + "\n",
			want: "600",
		},
		{
			name: "html rejected",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\nHello <b>world</b>\n",
			want: "line 4: HTML",
		},
		{
			name: "unsupported macro",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\n<<print \"hi\">>\n",
			want: "line 4: unsupported macro",
		},
		{
			name: "unclosed if",
			twee: ":: StoryTitle\nT\n:: Start {\"end\":true}\n<<if $x>>Hi\n",
			want: "line 4: unclosed <<if>>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.twee)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMatchChoice(t *testing.T) {
	t.Parallel()
	choices := []Choice{
		{Label: "Take the mossy path", Target: 1},
		{Label: "Follow the lantern", Target: 2},
		{Label: "Wait here", Target: 3},
	}
	cases := []struct {
		spoken string
		want   int
		ok     bool
	}{
		{"let's take the mossy path please", 0, true},
		{"lantern", 1, true},
		{"the first one", 0, true},
		{"second choice", 1, true},
		{"the last one", 2, true},
		{"what is the weather like today", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		idx, ok := MatchChoice(tc.spoken, choices)
		if ok != tc.ok || (ok && idx != tc.want) {
			t.Errorf("MatchChoice(%q) = (%d, %v), want (%d, %v)", tc.spoken, idx, ok, tc.want, tc.ok)
		}
	}
	if !slices.Equal(filterStopTokens(normalizeSpeechTokens("The mossy path!")), []string{"mossy", "path"}) {
		t.Fatal("unexpected token normalization")
	}
}

func BenchmarkParseTwee(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if _, err := Parse(sampleBranchingTwee); err != nil {
			b.Fatal(err)
		}
	}
}
