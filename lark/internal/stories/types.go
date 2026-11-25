package stories

import "encoding/json"

const (
	MaxPassages    = 64
	MaxPassageText = 600
	MaxChoices     = 3
	MaxFlags       = 64
)

// Value is one story flag value: a boolean or a number.
type Value struct {
	IsNum bool
	Bool  bool
	Num   float64
}

func (v Value) Truthy() bool {
	if v.IsNum {
		return v.Num != 0
	}
	return v.Bool
}

func (v Value) MarshalJSON() ([]byte, error) {
	if v.IsNum {
		return json.Marshal(v.Num)
	}
	return json.Marshal(v.Bool)
}

func (v *Value) UnmarshalJSON(data []byte) error {
	var b bool
	if json.Unmarshal(data, &b) == nil {
		*v = Value{Bool: b}
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = Value{IsNum: true, Num: n}
	return nil
}

const (
	opText uint8 = iota + 1
	opChoice
	opSet
	opIfNot
	opJump
)

type op struct {
	kind   uint8
	flag   uint16
	val    Value
	text   string
	target int // passage index for opChoice, op index for opIfNot/opJump
}

// Passage is one compiled Twee passage.
type Passage struct {
	Name string
	BG   string
	End  bool
	Ops  []op
}

// Choice is one compiled link, resolved to a passage index.
type Choice struct {
	Label  string
	Target int
}

// Story is a compiled Twee 3 story.
type Story struct {
	Title    string
	Start    int
	Passages []Passage
	ByName   map[string]int
	Flags    []string
	FlagIdx  map[string]uint16
}

// Saved is story metadata as stored in the stories table.
type Saved struct {
	ID        string
	Title     string
	Twee      string
	Author    string
	UpdatedAt int64
	Passages  int
}

// Progress is one companion's saved reading position.
type Progress struct {
	CompanionID string
	StoryID     string
	Passage     string
	Page        int
	Flags       map[string]Value
	UpdatedAt   int64
}
