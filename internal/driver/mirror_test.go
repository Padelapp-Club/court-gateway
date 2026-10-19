package driver

import "testing"

// The mirror's marker is read strictly (F5): exactly one marker, a known
// source, an id of [A-Za-z0-9_-] taken as written; anything else that
// starts with "[cg-mirror" is damaged, and a partner's marker is not one.
func TestParseMirrorMarker(t *testing.T) {
	for _, tc := range []struct {
		text  string
		state MarkerState
		want  MirrorMarker
	}{
		{"[cg-mirror:lunda:l-123]", MarkerValid, MirrorMarker{"lunda", "l-123"}},
		{"  [cg-mirror:viva:b-0a_Z]\n", MarkerValid, MirrorMarker{"viva", "b-0a_Z"}},
		{"[cg-mirror:fitness1c:x-room-202610201000-202610201100]", MarkerValid, MirrorMarker{"fitness1c", "x-room-202610201000-202610201100"}},
		{"[cg-mirror:yclients:y-1-2]", MarkerValid, MirrorMarker{"yclients", "y-1-2"}},
		{"[cg-mirror:lunda:L-123]", MarkerValid, MirrorMarker{"lunda", "L-123"}}, // case kept as written
		{"[cg-mirror:lunda:l-12", MarkerDamaged, MirrorMarker{}},                  // cut off
		{"[cg-mirror:lunda:l-1 23]", MarkerDamaged, MirrorMarker{}},               // a space inside
		{"[cg-mirror:lunda:l-123] Иванов", MarkerDamaged, MirrorMarker{}},         // more text
		{"[cg-mirror:lunda:l-123][cg-mirror:lunda:l-124]", MarkerDamaged, MirrorMarker{}},
		{"[cg-mirror:crm:l-123]", MarkerDamaged, MirrorMarker{}},   // no such CRM
		{"[cg-mirror:LUNDA:l-123]", MarkerDamaged, MirrorMarker{}}, // the source in lower case only
		{"[cg-mirror:lunda:]", MarkerDamaged, MirrorMarker{}},
		{"[cg-mirror]", MarkerDamaged, MirrorMarker{}},
		{"[cg:padelapp] [cg-mirror:lunda:l-1]", MarkerNone, MirrorMarker{}}, // a partner's
		{"бронь [cg-mirror:lunda:l-1]", MarkerNone, MirrorMarker{}},         // not at the start
		{"", MarkerNone, MirrorMarker{}},
	} {
		m, st := ParseMirrorMarker(tc.text)
		if st != tc.state || m != tc.want {
			t.Errorf("%q: %v %+v, want %v %+v", tc.text, st, m, tc.state, tc.want)
		}
		if st == MarkerValid && m.String() != tc.text && m.String() != trim(tc.text) {
			t.Errorf("%q: written back as %q", tc.text, m.String())
		}
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

func TestClassOfComment(t *testing.T) {
	for _, tc := range []struct {
		text    string
		class   MirrorClass
		partner string
	}{
		{"[cg:padelapp] Игра", ClassOwn, "padelapp"},
		{"[cg-mirror:lunda:l-1]", ClassMirror, ""},
		{"[cg-mirror:lunda:l-1", ClassDamaged, ""},
		{"Иванов", ClassClub, ""},
		{"[CG:padelapp]", ClassClub, ""},
		{"", ClassClub, ""},
	} {
		c, _, p := ClassOfComment(tc.text)
		if c != tc.class || p != tc.partner {
			t.Errorf("%q: %v %q", tc.text, c, p)
		}
	}
}
