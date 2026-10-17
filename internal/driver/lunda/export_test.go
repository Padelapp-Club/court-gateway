package lunda

import "encoding/json"

// EncodeSession is a saved session as the driver writes it, for tests that
// start logged in.
func EncodeSession(token, club, clubName string) []byte {
	raw, _ := json.Marshal(session{V: 1, Token: token, Club: club, ClubName: clubName})
	return raw
}

// SessionToken is the token of a saved session ("" when it does not read).
func SessionToken(raw []byte) string {
	var s session
	_ = json.Unmarshal(raw, &s)
	return s.Token
}
