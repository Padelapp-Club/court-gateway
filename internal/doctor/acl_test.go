package doctor

import "testing"

// The Windows ACL verdict (the reading itself is Windows-only and not run
// here): protected, Administrators and SYSTEM only.
func TestFolderACLProblem(t *testing.T) {
	const admins, system, users = "S-1-5-32-544", "S-1-5-18", "S-1-5-32-545"
	cases := []struct {
		name      string
		protected bool
		null      bool
		sids      []string
		ok        bool
	}{
		{"restricted", true, false, []string{admins, system}, true},
		{"inherits", false, false, []string{admins, system}, false},
		{"users may read", true, false, []string{admins, system, users}, false},
		{"null DACL", true, true, nil, false},
	}
	for _, c := range cases {
		if got := folderACLProblem(c.protected, c.null, c.sids); (got == "") != c.ok {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
