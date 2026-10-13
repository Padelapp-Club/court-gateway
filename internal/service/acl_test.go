package service

import "testing"

// The Windows DACL verdict (the reading and setting are Windows-only and
// not run here): protected, allow entries for exactly Administrators and
// SYSTEM, nothing else of any type.
func TestACLProblem(t *testing.T) {
	const users = "S-1-5-32-545"
	admins, system := ACE{0, SIDAdministrators}, ACE{0, SIDSystem}
	cases := []struct {
		name      string
		protected bool
		null      bool
		aces      []ACE
		ok        bool
	}{
		{"exact", true, false, []ACE{admins, system}, true},
		{"split inherit-only entries", true, false, []ACE{admins, admins, system, system}, true},
		{"inherits", false, false, []ACE{admins, system}, false},
		{"users may read", true, false, []ACE{admins, system, {0, users}}, false},
		{"object allow entry", true, false, []ACE{admins, system, {5, SIDAdministrators}}, false},
		{"deny entry", true, false, []ACE{admins, system, {1, SIDSystem}}, false},
		{"no SYSTEM", true, false, []ACE{admins}, false},
		{"empty", true, false, nil, false},
		{"null DACL", true, true, nil, false},
	}
	for _, c := range cases {
		if got := ACLProblem(c.protected, c.null, c.aces); (got == "") != c.ok {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
